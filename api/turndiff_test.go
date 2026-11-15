package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/workspace"
)

// A turn's files say what truncated means once, the same in the diff as in
// turn.changed, and the number they name is the cap the code keeps
// (0.5.1 F1 P3s, evt_899557aacfd814fe).
func TestTurnFilesTruncatedSaysWhy(t *testing.T) {
	doc := func(v any, field string) string {
		f, _ := reflect.TypeOf(v).FieldByName(field)
		return f.Tag.Get("doc")
	}
	diff, changed := doc(TurnFiles{}, "Truncated"), doc(event.TurnChanges{}, "Truncated")
	if !strings.Contains(diff, "past "+strconv.Itoa(workspace.MaxChanges)+",") || !strings.Contains(diff, "count misses some too") {
		t.Errorf("TurnFiles.truncated: %s", diff)
	}
	if strings.TrimPrefix(diff, "changes") != strings.TrimPrefix(changed, "files") {
		t.Errorf("the diff and turn.changed differ:\n%s\n%s", diff, changed)
	}
	if c := doc(TurnFiles{}, "Count"); c != doc(event.TurnChanges{}, "Count") || strings.HasPrefix(c, "Every") {
		t.Errorf("count: %s", c)
	}
}

// The gateway stores a call's tool.refused before its tool.requested: the
// diff still names the rule on that call, and its command.
func TestTurnDiffKeepsARefusalStoredFirst(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	for _, ev := range []event.Event{
		{ID: "evt_1", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Text: "push it", Harness: "claude"},
		{ID: "evt_2", Kind: event.ToolRefused, Time: now, Session: "s1", Turn: "t_1", Vendor: "anthropic", ToolID: "toolu_1",
			ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`), Rule: "git push"},
		{ID: "evt_3", Kind: event.ToolRequested, Time: now, Session: "s1", Turn: "t_1", Vendor: "anthropic", ToolID: "toolu_1",
			ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`)},
	} {
		if _, _, err := e.store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := do(t, e.req(t, "GET", "/api/v0/turns/t_1/diff", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var d TurnDiff
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Tools.Count != 1 || len(d.Tools.Calls) != 1 || d.Tools.Calls[0].RefusedRule != "git push" ||
		len(d.Commands) != 1 || d.Commands[0] != "git push" || d.Files != nil || d.Apply.State != "none" {
		t.Fatalf("diff: %s", body)
	}
}
