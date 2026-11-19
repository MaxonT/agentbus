package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/settings"
	"golang.org/x/sys/unix"
)

func TestFilesDirectOperations(t *testing.T) {
	e, _ := settingsEnv(t)
	dir := t.TempDir()
	f1 := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f1, []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. read_file
	resp, out := send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, f1))
	if resp.StatusCode != 200 {
		t.Fatalf("readFile: %d %s", resp.StatusCode, out)
	}
	var rf ReadFileResponse
	if err := json.Unmarshal([]byte(out), &rf); err != nil || rf.Content != "world" || rf.Size != 5 {
		t.Fatalf("readFile body: %v, out: %s", err, out)
	}

	// read missing file -> 404
	resp, _ = send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "nope.txt")))
	if resp.StatusCode != 404 {
		t.Fatalf("read missing file: %d", resp.StatusCode)
	}

	// read directory -> 400
	resp, _ = send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, dir))
	if resp.StatusCode != 400 {
		t.Fatalf("read dir: %d", resp.StatusCode)
	}

	// 2. list_folder
	resp, out = send(t, e, "POST", "/api/v0/files/list", "", fmt.Sprintf(`{"path":%q}`, dir))
	if resp.StatusCode != 200 {
		t.Fatalf("listFolder: %d %s", resp.StatusCode, out)
	}
	var lf ListFolderResponse
	if err := json.Unmarshal([]byte(out), &lf); err != nil || len(lf.Entries) != 1 || lf.Entries[0].Name != "hello.txt" || lf.Entries[0].Kind != "file" {
		t.Fatalf("listFolder body: %v, out: %s", err, out)
	}

	// list file -> 400
	resp, _ = send(t, e, "POST", "/api/v0/files/list", "", fmt.Sprintf(`{"path":%q}`, f1))
	if resp.StatusCode != 400 {
		t.Fatalf("list file: %d", resp.StatusCode)
	}

	// 3. write_file
	f2 := filepath.Join(dir, "sub", "test.txt")
	resp, out = send(t, e, "POST", "/api/v0/files/write", "", fmt.Sprintf(`{"path":%q,"content":"new content"}`, f2))
	if resp.StatusCode != 200 {
		t.Fatalf("writeFile: %d %s", resp.StatusCode, out)
	}
	var wf WriteFileResponse
	if err := json.Unmarshal([]byte(out), &wf); err != nil || wf.Written != 11 {
		t.Fatalf("writeFile body: %v, out: %s", err, out)
	}
	b, err := os.ReadFile(f2)
	if err != nil || string(b) != "new content" {
		t.Fatalf("written file: %s %v", string(b), err)
	}
}

// B8 tool gate tests: read_file through MCP with once, always for this folder, and refuse.
func TestFolderToolsGateWithHold(t *testing.T) {
	dir := t.TempDir()
	outsideFile := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside content"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondFile := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(secondFile, []byte("second content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Ask on read_file holds, person answers "once":
	// The call completes in the same turn. Next call to the same folder asks again.
	{
		e := toolGateEnv(t, 5*time.Second, 0, nil)
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, outsideFile)

		done := make(chan map[string]any, 1)
		go func() {
			_, v := rpc(t, e, mcpWriteTok, call)
			done <- v
		}()

		id := waitForApproval(t, e)
		answerApproval(t, e, id, "once")

		select {
		case v := <-done:
			if text := resultText(v); !strings.Contains(text, "outside content") {
				t.Fatalf("once allow did not return content: %v", text)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("tools/call timed out waiting for answer")
		}

		// A second call to another file in the folder is NOT pre-granted: it asks again!
		done2 := make(chan map[string]any, 1)
		call2 := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, secondFile)
		go func() {
			_, v := rpc(t, e, mcpWriteTok, call2)
			done2 <- v
		}()
		id2 := waitForApproval(t, e)
		if id2 == "" {
			t.Fatal("second call was not held")
		}
		answerApproval(t, e, id2, "refuse")
		v2 := <-done2
		if !strings.Contains(resultText(v2), "refused this call") {
			t.Fatalf("second call was not refused: %v", v2)
		}
	}

	// 2. Ask on read_file holds, person answers "always for this folder":
	// A grant row is written to control's folder_grants table (never a repo file).
	// The first call succeeds, and a second call to another file in that folder succeeds immediately!
	{
		e := toolGateEnv(t, 5*time.Second, 0, nil)
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, outsideFile)

		done := make(chan map[string]any, 1)
		go func() {
			_, v := rpc(t, e, mcpWriteTok, call)
			done <- v
		}()

		id := waitForApproval(t, e)
		// Check the suggested rule is the target folder:
		h, exists, _, err := e.api.approvalByID(id)
		if err != nil || !exists || len(h.rules) == 0 || (h.rules[0] != dir && strings.TrimPrefix(h.rules[0], "/private") != strings.TrimPrefix(dir, "/private")) {
			t.Fatalf("approval rule: %+v, want folder %s", h, dir)
		}

		answerApproval(t, e, id, "always for this folder")

		select {
		case v := <-done:
			if text := resultText(v); !strings.Contains(text, "outside content") {
				t.Fatalf("always allow did not return content: %v", text)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("tools/call timed out")
		}

		// Check folder_grants row on control:
		grants, err := e.store.ListFolderGrants("s1")
		if err != nil || len(grants) != 1 || (grants[0].Folder != dir && strings.TrimPrefix(grants[0].Folder, "/private") != strings.TrimPrefix(dir, "/private")) {
			t.Fatalf("ListFolderGrants: %v %v", grants, err)
		}

		// Second call to another file in the granted folder runs immediately without asking!
		call2 := fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, secondFile)
		_, v2 := rpc(t, e, mcpWriteTok, call2)
		if text := resultText(v2); !strings.Contains(text, "second content") {
			t.Fatalf("second call with grant did not return content: %v", text)
		}
		// Confirm no approval was requested for second call:
		if pending := pendingIDs(t, e); len(pending) != 0 {
			t.Fatalf("approval pending when already granted: %v", pending)
		}
	}
}

func TestDirectTokenFileAccessRequiresGrant(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	dir := t.TempDir()
	f := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(f, []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Direct call without grant -> 403
	req := e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out := do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("direct call without grant: %d %s", resp.StatusCode, out)
	}

	// Add grant to store for s1
	if err := e.store.AddFolderGrant("s1", dir); err != nil {
		t.Fatal(err)
	}

	// Direct call with grant -> 200
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 200 || !strings.Contains(out, "shh") {
		t.Fatalf("direct call with grant: %d %s", resp.StatusCode, out)
	}

	// Finding 1: Symlink escape inside granted folder is blocked
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside_secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "link_to_outside")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, symlinkPath)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("symlink escape was not blocked: %d %s", resp.StatusCode, out)
	}

	// Finding 2: Direct file requests respect forbid rules, including path-style rules
	eForbid := toolGateEnv(t, 5*time.Second, 0, func(p *settings.Policy) {
		p.Forbid = []string{"/usr/bin/read_file", "write_file"}
	})
	if err := eForbid.store.AddFolderGrant("s1", dir); err != nil {
		t.Fatal(err)
	}
	req = eForbid.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "tool forbidden by policy") {
		t.Fatalf("direct call with forbid rule was not rejected: %d %s", resp.StatusCode, out)
	}

	// Finding 1: Cyclic symlink returns 400 instead of crashing or looping
	cycleA := filepath.Join(dir, "cycle_a")
	cycleB := filepath.Join(dir, "cycle_b")
	_ = os.Symlink(cycleB, cycleA)
	_ = os.Symlink(cycleA, cycleB)
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, cycleA)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "symbolic links") {
		t.Fatalf("cyclic symlink was not rejected with 400: %d %s", resp.StatusCode, out)
	}

	// Finding 3: Missing content in writeFile is rejected (400)
	req = e.req(t, "POST", "/api/v0/files/write", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "path and content") {
		t.Fatalf("missing content in write_file was not rejected: %d %s", resp.StatusCode, out)
	}
	// Verify file content was not truncated
	shhB, _ := os.ReadFile(f)
	if string(shhB) != "shh" {
		t.Fatalf("file content was corrupted: %q", string(shhB))
	}

	// Finding 1: Granting /private does not grant /Users paths
	ePrivate := toolGateEnv(t, 5*time.Second, 0, nil)
	if err := ePrivate.store.AddFolderGrant("s1", "/private"); err != nil {
		t.Fatal(err)
	}
	req = ePrivate.req(t, "POST", "/api/v0/files/read", strings.NewReader(`{"path":"/Users/example/data.txt"}`))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("granting /private granted /Users: %d %s", resp.StatusCode, out)
	}

	// Finding 3: Non-regular file (e.g. /dev/null) is rejected
	_ = e.store.AddFolderGrant("s1", "/dev")
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(`{"path":"/dev/null"}`))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "not a regular file") {
		t.Fatalf("reading /dev/null was not rejected: %d %s", resp.StatusCode, out)
	}

	// Codex Round 5 P1 tests:
	// 1. Cyclic symlink in MCP call is rejected immediately without holding
	{
		eMCP := toolGateEnv(t, 5*time.Second, 0, nil)
		cycleA := filepath.Join(dir, "mcp_cycle_a")
		cycleB := filepath.Join(dir, "mcp_cycle_b")
		_ = os.Symlink(cycleB, cycleA)
		_ = os.Symlink(cycleA, cycleB)
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, cycleA)
		_, v := rpc(t, eMCP, mcpWriteTok, call)
		if text := resultText(v); !strings.Contains(text, "symbolic links") {
			t.Fatalf("cyclic symlink in MCP was not rejected immediately: %v", v)
		}
		if pending := pendingIDs(t, eMCP); len(pending) != 0 {
			t.Fatalf("cyclic symlink was held: %v", pending)
		}
	}

	// 2. Directory swap cannot escape granted folder
	{
		subDir := filepath.Join(dir, "sub_swap")
		if err := os.Mkdir(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		subFile := filepath.Join(subDir, "file.txt")
		if err := os.WriteFile(subFile, []byte("sub file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(subDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, subDir); err != nil {
			t.Fatal(err)
		}
		req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, filepath.Join(subDir, "outside_secret.txt"))))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode == 200 {
			t.Fatalf("directory swap read succeeded! %s", out)
		}
	}

	// 3. FIFO read and list do not block
	{
		fifoPath := filepath.Join(dir, "test_fifo")
		if err := unix.Mkfifo(fifoPath, 0666); err == nil {
			req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, fifoPath)))
			req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
			resp, out = do(t, req)
			if resp.StatusCode != 400 || !strings.Contains(out, "not a regular file") {
				t.Fatalf("FIFO read did not fail with 400: %d %s", resp.StatusCode, out)
			}

			req = e.req(t, "POST", "/api/v0/files/list", strings.NewReader(fmt.Sprintf(`{"path":%q}`, fifoPath)))
			req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
			resp, out = do(t, req)
			if resp.StatusCode != 400 || !strings.Contains(out, "path is a file") {
				t.Fatalf("FIFO list did not fail with 400: %d %s", resp.StatusCode, out)
			}
		}
	}

	// 4. MCP write to nested missing directory under existing folder grant succeeds
	{
		nestedFile := filepath.Join(dir, "new", "nested", "file.txt")
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"write_file","arguments":{"path":%q,"content":"nested hello"}}}`, nestedFile)
		_, v := rpc(t, e, mcpWriteTok, call)
		if text := resultText(v); strings.Contains(text, "Error") || strings.Contains(text, "failed") {
			t.Fatalf("MCP write to nested directory failed: %v", v)
		}
		data, err := os.ReadFile(nestedFile)
		if err != nil || string(data) != "nested hello" {
			t.Fatalf("nested file not written: %s %v", string(data), err)
		}
	}

	// 5. Valid file names starting with .. (e.g. ..notes) are not refused
	{
		dotdotFile := filepath.Join(dir, "..notes")
		if err := os.WriteFile(dotdotFile, []byte("dotdot content"), 0o600); err != nil {
			t.Fatal(err)
		}
		req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, dotdotFile)))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode != 200 || !strings.Contains(out, "dotdot content") {
			t.Fatalf("reading ..notes was refused: %d %s", resp.StatusCode, out)
		}
	}

	// 6. Refused write does not truncate the destination file
	{
		origFile := filepath.Join(dir, "no_trunc.txt")
		if err := os.WriteFile(origFile, []byte("important content"), 0o600); err != nil {
			t.Fatal(err)
		}
		eNoGrant := toolGateEnv(t, 5*time.Second, 0, nil)
		req = eNoGrant.req(t, "POST", "/api/v0/files/write", strings.NewReader(fmt.Sprintf(`{"path":%q,"content":"bad content"}`, origFile)))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode != 403 {
			t.Fatalf("ungranted write status: %d %s", resp.StatusCode, out)
		}
		content, _ := os.ReadFile(origFile)
		if string(content) != "important content" {
			t.Fatalf("refused write truncated file! content: %q", string(content))
		}
	}

	// 7. "always for this folder" when parent directories do not exist on disk yet
	{
		eHold := toolGateEnv(t, 5*time.Second, 0, nil)
		deepMissing := filepath.Join(dir, "brand_new_folder", "sub", "test.txt")
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"write_file","arguments":{"path":%q,"content":"deep hello"}}}`, deepMissing)
		done := make(chan map[string]any, 1)
		go func() {
			_, v := rpc(t, eHold, mcpWriteTok, call)
			done <- v
		}()

		id := waitForApproval(t, eHold)
		answerApproval(t, eHold, id, "always for this folder")

		select {
		case v := <-done:
			if text := resultText(v); strings.Contains(text, "Error") || strings.Contains(text, "failed") {
				t.Fatalf("always for missing folder failed: %v", text)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for approval execution")
		}
		data, err := os.ReadFile(deepMissing)
		if err != nil || string(data) != "deep hello" {
			t.Fatalf("deep missing file not created: %s %v", string(data), err)
		}
	}

	// 8. A changed symlink target between hold and approval is refused
	{
		eSym := toolGateEnv(t, 5*time.Second, 0, nil)
		targetA := filepath.Join(dir, "target_a.txt")
		targetB := filepath.Join(dir, "target_b.txt")
		_ = os.WriteFile(targetA, []byte("A"), 0o600)
		_ = os.WriteFile(targetB, []byte("B"), 0o600)
		linkPath := filepath.Join(dir, "dynamic_link.txt")
		_ = os.Symlink(targetA, linkPath)

		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}`, linkPath)
		done := make(chan map[string]any, 1)
		go func() {
			_, v := rpc(t, eSym, mcpWriteTok, call)
			done <- v
		}()

		id := waitForApproval(t, eSym)
		// Change the symlink to targetB during hold:
		_ = os.Remove(linkPath)
		_ = os.Symlink(targetB, linkPath)

		answerApproval(t, eSym, id, "once")
		v := <-done
		if text := resultText(v); !strings.Contains(text, "path changed to ungranted target") {
			t.Fatalf("changed symlink was not refused: %v", v)
		}
	}
}
