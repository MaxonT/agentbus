package jobs

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// A runner's plan.usage is taken only for its own machine's subscription: it
// must name the runner's own connector as its connect (jobs/record.go). A
// forged reading for a subscription control holds carries no such connector,
// so the batch is refused whole and the record holds none. Before the guard a
// runner's codex reading on a claude turn was taken (200) and became the shown
// value. The runner here has no connector, so even its own-named reading is
// refused; TestRunnerPlanUsageOwnMachine covers the accepted case.
func TestRunnerPlanUsageRefused(t *testing.T) {
	e := newOutcomeEnv(t)
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	forged, _ := json.Marshal(event.Event{ID: "e-forged-1", Kind: event.PlanUsage, Session: j.Session, Turn: j.Turn, Vendor: "openai",
		Plan: &event.Plan{Subscription: "codex", Window: "primary", WindowMinutes: 300, UsedPercent: 99}})
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{forged}}, nil); code != 400 {
		t.Fatalf("a runner's plan.usage: %d, want 400", code)
	}
	if k := e.kinds(t, j.Session); k[event.PlanUsage] != 0 {
		t.Fatalf("the record holds %v", k)
	}
	if plans, err := e.st.PlanUsage(); err != nil || len(plans) != 0 {
		t.Fatalf("plan usage: %+v %v", plans, err)
	}
}

// A runner sends only the kinds it writes for a turn (RunnerKinds). A
// control-only kind, a program's x.* kind and a retired kind are refused
// whole with 400, and the record holds none of them; every listed kind is
// taken.
func TestRunnerRecordAllowList(t *testing.T) {
	e := newOutcomeEnv(t)
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	// plan.usage is a RunnerKind now, but only for the runner's own machine
	// (TestRunnerPlanUsageOwnMachine), so it is not in this bare-event sweep.
	refused := []event.Kind{event.ToolsListed, event.SessionRenamed, event.EventsDropped, event.TurnChanged,
		event.TurnApplied, event.SettingsChanged, event.RunnerEnrolled, event.SessionRegistered, event.CommandRan, "x.review.verdict", "made.up"}
	for i, k := range refused {
		ev, _ := json.Marshal(event.Event{ID: "e-refused-" + strconv.Itoa(i), Kind: k, Session: j.Session, Turn: j.Turn})
		if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{ev}}, nil); code != 400 {
			t.Errorf("a runner's %s: %d, want 400", k, code)
		}
	}
	var all []json.RawMessage
	n := 0
	for k := range RunnerKinds {
		if k == event.SessionClosed {
			continue // a turn's end settles it, tested with the outcome
		}
		if k == event.PlanUsage {
			continue // only the runner's own machine's, tested apart with a connector
		}
		ev, _ := json.Marshal(event.Event{ID: "e-taken-" + strconv.Itoa(n), Kind: k, Session: j.Session, Turn: j.Turn})
		all = append(all, ev)
		n++
	}
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: all}, nil); code != 200 {
		t.Fatalf("the runner's own kinds: %d, want 200", code)
	}
	got := e.kinds(t, j.Session)
	for _, k := range refused {
		if got[k] != 0 {
			t.Errorf("the record holds %s", k)
		}
	}
	for k := range RunnerKinds {
		if k != event.SessionClosed && k != event.PlanUsage && got[k] == 0 {
			t.Errorf("the record lacks %s", k)
		}
	}
}

// A -gateway local runner reports its own machine's plan usage: a plan.usage
// whose connect is the runner's connector is taken and stored. One that names
// another connector, or none, cannot be its own machine's, so the batch is
// refused whole (the forgery guard of TestRunnerPlanUsageRefused).
func TestRunnerPlanUsageOwnMachine(t *testing.T) {
	e := newOutcomeEnv(t)
	e.tok = enrollConnector(t, e.srv) // a runner whose connector is testConnector
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	own, _ := json.Marshal(event.Event{ID: "e-own-1", Kind: event.PlanUsage, Session: j.Session, Turn: j.Turn, Vendor: "anthropic",
		Plan: &event.Plan{Subscription: "anthropic", Connection: testConnector, Window: "7d", WindowMinutes: 7 * 24 * 60, UsedPercent: 42}})
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{own}}, nil); code != 200 {
		t.Fatalf("the runner's own machine plan.usage: %d, want 200", code)
	}
	if k := e.kinds(t, j.Session); k[event.PlanUsage] != 1 {
		t.Fatalf("the record holds %v, want one plan.usage", k)
	}
	if plans, err := e.st.PlanUsage(); err != nil || len(plans) != 1 || plans[0].Plan == nil || plans[0].Plan.Connection != testConnector {
		t.Fatalf("plan usage: %+v %v", plans, err)
	}
	// Another connector, or none, is not this runner's machine: refused whole.
	for i, conn := range []string{"", "con_fedcba9876543210"} {
		forged, _ := json.Marshal(event.Event{ID: "e-forge-" + strconv.Itoa(i), Kind: event.PlanUsage, Session: j.Session, Turn: j.Turn,
			Vendor: "anthropic", Plan: &event.Plan{Subscription: "claude", Connection: conn, Window: "7d", WindowMinutes: 7 * 24 * 60, UsedPercent: 99}})
		if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{forged}}, nil); code != 400 {
			t.Errorf("plan.usage with connector %q: %d, want 400", conn, code)
		}
	}
	if k := e.kinds(t, j.Session); k[event.PlanUsage] != 1 {
		t.Fatalf("after the forged readings the record holds %v, want one plan.usage", k)
	}
}
