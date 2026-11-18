package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/staysup/agentbus/toolreg"
)

// toolEntry is one tool as tools/list sends it, by name.
func toolEntry(t *testing.T, e *env, tok, name string) map[string]any {
	t.Helper()
	_, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, x := range dig(v, "result", "tools").([]any) {
		m := x.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	return nil
}

// callText is the text a tools/call of name with args answers, and whether it
// was an error.
func callText(t *testing.T, e *env, tok, name, args string) (bool, string) {
	t.Helper()
	_, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	c, ok := dig(v, "result", "content").([]any)
	if !ok || len(c) != 1 {
		t.Fatalf("%s: %v", name, v)
	}
	return dig(v, "result", "isError") == true, c[0].(map[string]any)["text"].(string)
}

// listToolSummaries reads GET /api/v0/tools with the main token: the list, as
// facts only.
func listToolSummaries(t *testing.T, e *env) []ToolSummary {
	t.Helper()
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools: %d %s", resp.StatusCode, body)
	}
	var out ToolList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Tools
}

// getToolDef reads GET /api/v0/tools/{name}: one tool's full definition.
func getToolDef(t *testing.T, e *env, name string) toolreg.Tool {
	t.Helper()
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools/"+name, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools/%s: %d %s", name, resp.StatusCode, body)
	}
	var def toolreg.Tool
	if err := json.Unmarshal([]byte(body), &def); err != nil {
		t.Fatal(err)
	}
	return def
}

// B3's done-when: a built-in removed and re-added as a custom tool behaves the
// same. The tool's list entry (schema, annotations, description) and a call's
// result are byte-identical before removing it and after re-adding it.
func TestDefaultRemovedAndReaddedBehavesTheSame(t *testing.T) {
	e := mcpEnv(t)
	// The built-ins are served to the main token: the 13 of B3 plus the two
	// context-view reads, turn_context and session_context.
	if got := len(listToolSummaries(t, e)); got != 15 {
		t.Fatalf("%d built-in tools, want 15", got)
	}
	before := toolEntry(t, e, token, "apply_turn")
	if before == nil {
		t.Fatal("apply_turn not listed")
	}
	_, beforeCall := callText(t, e, token, "apply_turn", `{"turn":"t_x"}`)

	// The stored definition of apply_turn, from the single tool route.
	def := getToolDef(t, e, "apply_turn")
	if def.Run.Method != "POST" || def.Run.Path != "/api/v0/turns/{id}/apply" || def.Effect != toolreg.EffectDoes {
		t.Fatalf("apply_turn definition: %+v", def)
	}

	// Remove it: it stops being served.
	resp, body := do(t, e.req(t, "DELETE", "/api/v0/tools/apply_turn", nil))
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE apply_turn: %d %s", resp.StatusCode, body)
	}
	if toolEntry(t, e, token, "apply_turn") != nil {
		t.Fatal("apply_turn still listed after removal")
	}

	// Re-add the same definition as a custom tool.
	b, _ := json.Marshal(def)
	resp, body = do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader(b)))
	if resp.StatusCode != 201 {
		t.Fatalf("POST apply_turn: %d %s", resp.StatusCode, body)
	}

	after := toolEntry(t, e, token, "apply_turn")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("list entry changed after re-add:\nbefore %v\nafter  %v", before, after)
	}
	if _, afterCall := callText(t, e, token, "apply_turn", `{"turn":"t_x"}`); afterCall != beforeCall {
		t.Errorf("call result changed: %q vs %q", beforeCall, afterCall)
	}
}

// The list carries facts only (the 0.6 rule): each entry has a name, a
// description, an effect and where it runs, and never the inputs schema or the
// run. A tool's whole definition is read from GET /api/v0/tools/{id}, which a
// turn token cannot reach and which 404s on a name not served.
func TestToolListFactsOnly(t *testing.T) {
	e := mcpEnv(t)
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools: %d %s", resp.StatusCode, body)
	}
	var raw struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Tools) == 0 {
		t.Fatal("no tools listed")
	}
	for _, m := range raw.Tools {
		for _, k := range []string{"name", "description", "effect", "where"} {
			if _, ok := m[k]; !ok {
				t.Errorf("list entry has no %q: %v", k, m)
			}
		}
		for _, k := range []string{"inputs", "run"} {
			if _, ok := m[k]; ok {
				t.Errorf("list entry carries %q; the list is facts only: %v", k, m)
			}
		}
	}

	// The single tool route carries the full definition.
	def := getToolDef(t, e, "apply_turn")
	if def.Name != "apply_turn" || def.Run.Path != "/api/v0/turns/{id}/apply" || def.Inputs == nil {
		t.Fatalf("single tool not full: %+v", def)
	}

	// A name not served is a 404, with the same words as remove.
	if r, b := do(t, e.req(t, "GET", "/api/v0/tools/no_such", nil)); r.StatusCode != 404 {
		t.Errorf("GET missing tool: %d %s", r.StatusCode, b)
	}

	// A turn token cannot read a single tool, as it cannot list them.
	r := e.req(t, "GET", "/api/v0/tools/apply_turn", nil)
	r.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	if resp, _ := do(t, r); resp.StatusCode == 200 {
		t.Errorf("turn token read a single tool: %d", resp.StatusCode)
	}
}

// Add refuses a run that names no route, a duplicate name, and a body that is
// not a tool; remove refuses a name that is not served; and a turn token
// cannot reach the management routes at all.
func TestToolsRoutesGuardrails(t *testing.T) {
	e := mcpEnv(t)
	post := func(body string) (int, string) {
		resp, b := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(body))))
		return resp.StatusCode, b
	}
	// A run may name only a route a built-in tool exposes. Each of these is a
	// route a custom tool cannot be: a path with no route, a real route that
	// is not a tool (POST /api/v0/agents), a {call} path, and a settings write
	// that needs an If-Match header.
	for _, run := range []string{
		`{"kind":"api","method":"GET","path":"/api/v0/nope"}`,
		`{"kind":"api","method":"POST","path":"/api/v0/agents","body":true}`,
		`{"kind":"api","method":"GET","path":"/api/v0/turns/{id}/calls/{call}","id_input":"turn"}`,
		`{"kind":"api","method":"PUT","path":"/api/v0/settings/instructions","body":true}`,
	} {
		body := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{}},"effect":"reads","run":` + run + `}`
		if code, b := post(body); code != 400 || !bytes.Contains([]byte(b), []byte("a built-in tool already exposes")) {
			t.Errorf("non-tool route %s: %d %s", run, code, b)
		}
	}
	// A property that is not a schema object.
	badSchema := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{"session":17}},"effect":"reads","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(badSchema); code != 400 {
		t.Errorf("malformed schema: %d", code)
	}
	// A name already served.
	dup := `{"name":"list_turns","description":"x","inputs":{"type":"object","properties":{}},"effect":"reads","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(dup); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	// A bad effect.
	bad := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{}},"effect":"maybe","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(bad); code != 400 {
		t.Errorf("bad effect: %d", code)
	}
	// Remove a name that is not served.
	if resp, _ := do(t, e.req(t, "DELETE", "/api/v0/tools/no_such", nil)); resp.StatusCode != 404 {
		t.Errorf("remove missing: %d", resp.StatusCode)
	}
	// A turn token cannot manage tools: the route is the main token's.
	r := e.req(t, "GET", "/api/v0/tools", nil)
	r.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	if resp, _ := do(t, r); resp.StatusCode == 200 {
		t.Errorf("turn token reached /api/v0/tools: %d", resp.StatusCode)
	}
}

// Reusing a built-in's name for a different definition advertises the new
// definition's hints, not the old name's: list_turns (reads) replaced by a
// does-something tool is no longer read-only.
func TestReplacedBuiltinUsesItsOwnHints(t *testing.T) {
	e := mcpEnv(t)
	if a := toolEntry(t, e, token, "list_turns")["annotations"].(map[string]any); a["readOnlyHint"] != true {
		t.Fatalf("built-in list_turns not read-only: %v", a)
	}
	if resp, body := do(t, e.req(t, "DELETE", "/api/v0/tools/list_turns", nil)); resp.StatusCode != 204 {
		t.Fatalf("DELETE list_turns: %d %s", resp.StatusCode, body)
	}
	custom := `{"name":"list_turns","description":"starts work now","inputs":{"type":"object","properties":{}},"effect":"does-something","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if resp, body := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(custom)))); resp.StatusCode != 201 {
		t.Fatalf("POST list_turns: %d %s", resp.StatusCode, body)
	}
	if a := toolEntry(t, e, token, "list_turns")["annotations"].(map[string]any); a["readOnlyHint"] != false {
		t.Errorf("replaced list_turns still read-only: %v", a)
	}
}

// A custom tool whose id is optional only when it says so: with own_session
// off, a missing id is refused, not filled from the caller's session, so the
// tool cannot act on the caller's session by leaving the id out.
func TestCustomToolNoSilentOwnSession(t *testing.T) {
	e := mcpEnv(t)
	// A tool on the rename route (a built-in's route) with own_session off and
	// a schema that does not require the id.
	def := `{"name":"touch_session","description":"renames a session","inputs":{"type":"object","properties":{"session":{"type":"string"},"title":{"type":"string"}},"required":["title"]},"effect":"does-something","run":{"kind":"api","method":"PATCH","path":"/api/v0/sessions/{id}","id_input":"session","body":true}}`
	if resp, body := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(def)))); resp.StatusCode != 201 {
		t.Fatalf("POST touch_session: %d %s", resp.StatusCode, body)
	}
	// Called by a turn of session s1 with no session: refused, never filled in
	// with s1.
	isErr, text := callText(t, e, mcpWriteTok, "touch_session", `{"title":"renamed"}`)
	if !isErr || !bytes.Contains([]byte(text), []byte("session is required")) {
		t.Errorf("missing id was not refused: isErr=%v text=%q", isErr, text)
	}
}

// Many tool changes at once all land in the served set: no rebuild overwrites
// a newer one with a stale snapshot. Run under -race.
func TestConcurrentToolChangesAllLand(t *testing.T) {
	e := mcpEnv(t)
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var wg sync.WaitGroup
	for _, l := range letters {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			body := `{"name":"` + name + `","description":"x","inputs":{"type":"object","properties":{}},"effect":"reads","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
			if resp, b := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(body)))); resp.StatusCode != 201 {
				t.Errorf("POST %s: %d %s", name, resp.StatusCode, b)
			}
		}("tool_name_" + l)
	}
	wg.Wait()
	served := toolNames(t, e, token)
	for _, l := range letters {
		if !slices.Contains(served, "tool_name_"+l) {
			t.Errorf("tool_name_%s missing from served set %v", l, served)
		}
	}
}
