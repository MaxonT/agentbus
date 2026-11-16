package api

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
)

// fakeApprovals is a held-reply source for the handler test: one approval, and
// a record of the answer it was given.
type fakeApprovals struct {
	mu       sync.Mutex
	aps      map[string]gateway.ApprovalInfo
	deciding map[string]bool
	decided  []string // "<id>:<allow|refuse>:<by>"
}

func (f *fakeApprovals) Approvals() []gateway.ApprovalInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gateway.ApprovalInfo{}
	for _, ap := range f.aps {
		out = append(out, ap)
	}
	return out
}

func (f *fakeApprovals) Claim(id string) (gateway.ApprovalInfo, bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.aps[id]
	if !ok {
		return gateway.ApprovalInfo{}, false, false
	}
	if f.deciding[id] {
		return ap, false, true
	}
	if f.deciding == nil {
		f.deciding = map[string]bool{}
	}
	f.deciding[id] = true
	return ap, true, true
}

func (f *fakeApprovals) Release(id string) {
	f.mu.Lock()
	delete(f.deciding, id)
	f.mu.Unlock()
}

func (f *fakeApprovals) Decide(id string, allow bool, by string) (gateway.ApprovalInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.aps[id]
	if !ok {
		return gateway.ApprovalInfo{}, false
	}
	delete(f.aps, id)
	delete(f.deciding, id)
	verb := "refuse"
	if allow {
		verb = "allow"
	}
	f.decided = append(f.decided, id+":"+verb+":"+by)
	return ap, true
}

func askStack(t *testing.T, fa *fakeApprovals) *env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.toml"), []byte("ask = [\"git push\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return stackEnv(t, stack{permissions: filepath.Join(dir, "permissions.toml"), approvals: fa})
}

// Allow moves each approved command from ask to allow in the settings (so the
// runner allows it from the next turn), records the answer, and forgets it. The
// continuation turn is skipped here because the turn that asked is gone.
func TestDecideApprovalAllowMovesAskToAllow(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{
		"ap1": {ID: "ap1", Session: "s1", Turn: "t_gone", Commands: []string{"git push"}, Rule: "git push", Rules: []string{"git push"}},
	}}
	e := askStack(t, fa)
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if !strings.Contains(out, `"allow":["git push"]`) || !strings.Contains(out, `"ask":[]`) {
		t.Fatalf("the command was not moved from ask to allow:\n%s", out)
	}
	if len(fa.decided) != 1 || fa.decided[0] != "ap1:allow:a person" {
		t.Fatalf("the answer was not recorded: %v", fa.decided)
	}
}

// Refuse records the answer and changes no settings.
func TestDecideApprovalRefuseRecordsOnly(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{
		"ap1": {ID: "ap1", Session: "s1", Turn: "t_gone", Commands: []string{"git push"}, Rule: "git push", Rules: []string{"git push"}},
	}}
	e := askStack(t, fa)
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"refuse"}`); resp.StatusCode != 204 {
		t.Fatalf("refuse: %d %s", resp.StatusCode, out)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if !strings.Contains(out, `"ask":["git push"]`) || strings.Contains(out, `"allow":["git push"]`) {
		t.Fatalf("refuse changed the settings:\n%s", out)
	}
	if len(fa.decided) != 1 || fa.decided[0] != "ap1:refuse:a person" {
		t.Fatalf("the answer was not recorded: %v", fa.decided)
	}
}

// findContinuation returns the continuation turn job of a session, the one with
// the approval prompt, or fails.
func findContinuation(t *testing.T, e *env) queue.Job {
	t.Helper()
	jobs, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: "s1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if strings.HasPrefix(j.Spec.Prompt, "A person approved:") {
			return j
		}
	}
	t.Fatalf("no continuation turn was started in the session; jobs: %d", len(jobs))
	return queue.Job{}
}

// Allow starts a continuation turn in the same session, on the agent of the turn
// that asked, with the approval prompt naming the commands.
func TestDecideApprovalAllowStartsContinuation(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{}}
	e := askStack(t, fa)
	// The turn that asked, so there is an agent to continue on.
	orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "do a thing"},
		queue.Links{}, queue.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	fa.aps["ap1"] = gateway.ApprovalInfo{ID: "ap1", Session: "s1", Turn: orig.Turn, Commands: []string{"git push"}, Rule: "git push", Rules: []string{"git push"}}
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	c := findContinuation(t, e)
	if c.Spec.Harness != "claude" || c.Spec.AgentID != "a1" || c.Spec.Session != "s1" {
		t.Fatalf("the continuation did not run on the asking turn's agent: %+v", c.Spec)
	}
	if !strings.Contains(c.Spec.Prompt, "git push") {
		t.Fatalf("the continuation prompt did not name the commands: %q", c.Spec.Prompt)
	}
	// The moved command is in the policy the runner will read for that turn.
	if _, out := send(t, e, "GET", "/api/v0/settings/permissions", "", ""); !strings.Contains(out, `"allow":["git push"]`) {
		t.Fatalf("the command was not moved to allow:\n%s", out)
	}
}

// Two Allows at once (two tabs, a retry) start exactly one continuation: one
// wins (204), the other is refused (409).
func TestDecideApprovalConcurrentAllow(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{}}
	e := askStack(t, fa)
	orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "x"},
		queue.Links{}, queue.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	fa.aps["ap1"] = gateway.ApprovalInfo{ID: "ap1", Session: "s1", Turn: orig.Turn, Commands: []string{"git push"}, Rules: []string{"git push"}}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`)
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	got := map[int]int{}
	for _, c := range codes {
		got[c]++
	}
	if got[204] != 1 || got[409] != 1 {
		t.Fatalf("want one 204 and one 409, got %v", codes)
	}
	n := 0
	jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{Session: "s1"}, 50)
	for _, j := range jobs {
		if strings.HasPrefix(j.Spec.Prompt, "A person approved:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one continuation, got %d", n)
	}
}

// Two Allows on DIFFERENT commands at once both succeed: the settings write
// retries the concurrent-change conflict rather than failing one with 500.
func TestDecideApprovalConcurrentDifferentCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.toml"), []byte("ask = [\"git push\", \"npm publish\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{
		"ap1": {ID: "ap1", Session: "s1", Turn: "t_gone", Commands: []string{"git push"}, Rules: []string{"git push"}},
		"ap2": {ID: "ap2", Session: "s1", Turn: "t_gone", Commands: []string{"npm publish"}, Rules: []string{"npm publish"}},
	}}
	e := stackEnv(t, stack{permissions: filepath.Join(dir, "permissions.toml"), approvals: fa})
	var wg sync.WaitGroup
	codes := map[string]int{}
	var mu sync.Mutex
	for _, id := range []string{"ap1", "ap2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			resp, _ := send(t, e, "POST", "/api/v0/approvals/"+id, "", `{"decision":"allow"}`)
			mu.Lock()
			codes[id] = resp.StatusCode
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	if codes["ap1"] != 204 || codes["ap2"] != 204 {
		t.Fatalf("both allows should succeed: %v", codes)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if !strings.Contains(out, "git push") || !strings.Contains(out, "npm publish") || !strings.Contains(out, `"ask":[]`) {
		t.Fatalf("both commands were not moved to allow:\n%s", out)
	}
}

// When the turn that asked is gone, allow still moves the command to allow and
// starts no continuation.
func TestDecideApprovalAllowAskingTurnGone(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{
		"ap1": {ID: "ap1", Session: "s1", Turn: "t_gone", Commands: []string{"git push"}, Rule: "git push", Rules: []string{"git push"}},
	}}
	e := askStack(t, fa)
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	jobs, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: "s1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a continuation started for a gone turn: %d jobs", len(jobs))
	}
	if _, out := send(t, e, "GET", "/api/v0/settings/permissions", "", ""); !strings.Contains(out, `"allow":["git push"]`) {
		t.Fatalf("the command was not moved to allow:\n%s", out)
	}
}

// An unknown id, or one already answered, is 404.
func TestDecideApprovalUnknown(t *testing.T) {
	fa := &fakeApprovals{aps: map[string]gateway.ApprovalInfo{}}
	e := askStack(t, fa)
	if resp, _ := send(t, e, "POST", "/api/v0/approvals/nope", "", `{"decision":"allow"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
}
