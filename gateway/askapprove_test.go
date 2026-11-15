package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// anthropicAskStream is a reply with one tool_use call, Bash running "git push".
const anthropicAskStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicTwoAskStream is a reply with two tool_use calls, both ask-matched.
const anthropicTwoAskStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"npm publish\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicAskJSON is an unstreamed (non-SSE) reply with one tool_use call.
const anthropicAskJSON = `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git push"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`

// guardOut runs the guard over a reply with the ask rules and returns what the
// agent gets, the gateway and its record.
func guardOut(t *testing.T, stream string, ask []string) (string, *Gateway, *event.Memory) {
	t.Helper()
	mem := &event.Memory{}
	g := New(&url.URL{}, anthropic.Adapter{}, mem)
	b := g.guardReply(&exchange{id: "x1", session: "s1", turn: "t_1"}, nil, ask,
		io.NopCloser(strings.NewReader(stream)), func() {})
	raw, _ := io.ReadAll(b)
	b.Close()
	return string(raw), g, mem
}

func kinds(evs []event.Event) map[event.Kind]event.Event {
	m := map[event.Kind]event.Event{}
	for _, e := range evs {
		m[e.Kind] = e
	}
	return m
}

// An ask command is refused to the agent (renamed) with a "waiting for a person"
// message, and recorded as one approval the person can answer. Nothing is held.
func TestAskRefusesAndRecords(t *testing.T) {
	got, g, mem := guardOut(t, anthropicAskStream, []string{"git push"})
	if !strings.Contains(got, RefusedPrefix+"Bash") {
		t.Fatalf("the ask call was not refused to the agent:\n%s", got)
	}
	aps := g.Approvals()
	if len(aps) != 1 || len(aps[0].Commands) != 1 || aps[0].Commands[0] != "git push" {
		t.Fatalf("the approval did not record the command: %+v", aps)
	}
	if txt := g.refused.text("toolu_1"); !strings.Contains(txt, "waiting for a person to approve") || !strings.Contains(txt, "git push") {
		t.Fatalf("the refusal text is wrong: %q", txt)
	}
	g.Close()
	if _, ok := kinds(mem.Events())[event.ApprovalRequested]; !ok {
		t.Fatal("no approval.requested recorded")
	}
}

// Every ask command in a reply is listed in the one approval: none is hidden.
func TestAskListsEveryCommand(t *testing.T) {
	got, g, _ := guardOut(t, anthropicTwoAskStream, []string{"git push", "npm publish"})
	if n := strings.Count(got, RefusedPrefix+"Bash"); n != 2 {
		t.Fatalf("want both ask calls refused, got %d:\n%s", n, got)
	}
	aps := g.Approvals()
	if len(aps) != 1 || len(aps[0].Commands) != 2 ||
		aps[0].Commands[0] != "git push" || aps[0].Commands[1] != "npm publish" {
		t.Fatalf("the approval did not list both commands: %+v", aps)
	}
	// Each call's own refusal text names its own command.
	if txt := g.refused.text("toolu_2"); !strings.Contains(txt, "npm publish") {
		t.Fatalf("the second call's refusal did not name its own command: %q", txt)
	}
	g.Close()
}

// An unstreamed (JSON) reply is refused and recorded the same way.
func TestAskUnstreamedRefusesAndRecords(t *testing.T) {
	got, g, _ := guardOut(t, anthropicAskJSON, []string{"git push"})
	if !strings.Contains(got, RefusedPrefix+"Bash") {
		t.Fatalf("the unstreamed ask call was not refused:\n%s", got)
	}
	if aps := g.Approvals(); len(aps) != 1 || aps[0].Commands[0] != "git push" {
		t.Fatalf("the unstreamed reply did not record the command: %+v", aps)
	}
	g.Close()
}

// The approval records the matched ask RULE (a valid permissions entry), not
// the raw command with its arguments, so moving it to allow removes exactly the
// entry that refused and the continuation is not refused again.
func TestAskRecordsMatchedRuleNotRawCommand(t *testing.T) {
	g := New(&url.URL{}, anthropic.Adapter{}, &event.Memory{})
	b := g.guardReply(&exchange{id: "x1", session: "s1", turn: "t_1"}, nil, []string{"git push"},
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
	item := map[string]any{"name": "Bash", "input": map[string]any{"command": "git push origin main"}}
	b.decide("0", "toolu_1", "Bash", json.RawMessage(`{"command":"git push origin main"}`), item)
	b.registerApproval()
	aps := g.Approvals()
	if len(aps) != 1 {
		t.Fatalf("want one approval, got %d", len(aps))
	}
	if len(aps[0].Rules) != 1 || aps[0].Rules[0] != "git push" {
		t.Fatalf("the approval did not record the matched rule: %+v", aps[0].Rules)
	}
	if len(aps[0].Commands) != 1 || aps[0].Commands[0] != "git push origin main" {
		t.Fatalf("the approval lost the raw command: %+v", aps[0].Commands)
	}
	g.Close()
}

// A codex local_shell_call refused by an ask rule is told the ask message, not
// the forbid message (it is refused in place, not through fixHistory).
func TestAskLocalShellCallUsesAskText(t *testing.T) {
	g := New(&url.URL{}, openai.Adapter{}, &event.Memory{})
	b := g.guardReply(&exchange{id: "x1", session: "s1", turn: "t_1"}, nil, []string{"git push"},
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
	item := map[string]any{"type": "local_shell_call", "call_id": "c1",
		"action": map[string]any{"type": "exec", "command": []any{"/bin/zsh", "-lc", "git push"}}}
	b.decide("c1", "c1", "", nil, item)
	txt := b.refused["c1"]
	if !strings.Contains(txt, "waiting for a person to approve") {
		t.Fatalf("the ask call got the wrong refusal text: %q", txt)
	}
	refuseItem(item, txt)
	cmd := item["action"].(map[string]any)["command"].([]any)
	if cmd[len(cmd)-1].(string) != txt {
		t.Fatalf("the local_shell_call did not embed the ask text: %v", cmd)
	}
	g.Close()
}

// anthropicAskPartial is the ask reply without its terminal message_stop: the
// tool_use block completed, but the reply never ended.
const anthropicAskPartial = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`

// A reply that never reached its terminal event records no approval, whether it
// ended by a clean EOF or a connection error: the agent discards it and the
// gateway drops its tool.requested, so a command it never really asked must not
// appear.
func TestAskNoTerminalEventNoApproval(t *testing.T) {
	for name, src := range map[string]io.ReadCloser{
		"clean EOF": io.NopCloser(strings.NewReader(anthropicAskPartial)),
		"cut":       &cutReader{data: []byte(anthropicAskPartial)},
	} {
		g := New(&url.URL{}, anthropic.Adapter{}, &event.Memory{})
		b := g.guardReply(&exchange{id: "x1", session: "s1", turn: "t_1"}, nil, []string{"git push"}, src, func() {})
		io.ReadAll(b)
		b.Close()
		if n := len(g.Approvals()); n != 0 {
			t.Fatalf("%s: a reply with no terminal event recorded an approval: %d", name, n)
		}
		g.Close()
	}
}

// cutReader yields its bytes, then a non-EOF error, like a connection reset
// before the reply's clean end.
type cutReader struct {
	data []byte
	pos  int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errReset
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *cutReader) Close() error { return nil }

var errReset = fmt.Errorf("connection reset")

// Claim is exclusive: a second claim while the first is deciding is refused, so
// two allows at once cannot both act; a release makes it claimable again.
func TestApprovalClaimIsExclusive(t *testing.T) {
	g := New(&url.URL{}, anthropic.Adapter{}, &event.Memory{})
	id := g.recordApproval("s1", "t_1", "toolu_1", "Bash", []string{"git push"}, []string{"git push"}).ID
	if _, claimed, exists := g.Claim(id); !claimed || !exists {
		t.Fatal("the first claim did not take")
	}
	if _, claimed, exists := g.Claim(id); claimed || !exists {
		t.Fatal("a second claim took while the first was deciding")
	}
	g.Release(id)
	if _, claimed, _ := g.Claim(id); !claimed {
		t.Fatal("a released reply could not be claimed again")
	}
	if _, ok := g.Decide(id, true, "x"); !ok {
		t.Fatal("Decide did not complete the claimed reply")
	}
	if _, _, exists := g.Claim(id); exists {
		t.Fatal("a decided reply is still present")
	}
	g.Close()
}

// Decide records the answer, returns the reply's info for the API to act on,
// and forgets the approval. An unknown id is reported false.
func TestDecideRecordsAndReturns(t *testing.T) {
	_, g, mem := guardOut(t, anthropicAskStream, []string{"git push"})
	id := g.Approvals()[0].ID
	info, ok := g.Decide(id, true, "the tester")
	if !ok || len(info.Commands) != 1 || info.Commands[0] != "git push" || info.Session != "s1" || info.Turn != "t_1" {
		t.Fatalf("Decide did not return the approval info: ok=%v info=%+v", ok, info)
	}
	if len(g.Approvals()) != 0 {
		t.Fatal("the approval was not forgotten after it was answered")
	}
	if _, again := g.Decide(id, true, "the tester"); again {
		t.Fatal("a second Decide on the same id reported it answered")
	}
	g.Close()
	if d := kinds(mem.Events())[event.ApprovalDecided]; !strings.Contains(d.Text, "allowed by the tester") {
		t.Fatalf("approval.decided: %+v", d)
	}
}
