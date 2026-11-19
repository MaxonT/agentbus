package queue

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deliveriesOf returns the result-carrying turns queued in session.
func deliveriesOf(t *testing.T, q *Queue, session string) []Job {
	t.Helper()
	all, err := q.List(DefaultTeam, Filter{Session: session}, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []Job
	for _, j := range all {
		if j.Links.ResultOf != nil {
			out = append(out, j)
		}
	}
	return out
}

// An `on turn.completed` registered for a turn fires once when that turn
// ends: it queues the delivery turn in the on's session, records the fire so
// a second sweep (or a control restart, which keeps no memory) never delivers
// twice, and is then off. The delivery is byte-for-byte what the old Deliver
// made.
func TestOnDelivers(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	on, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, Maker{Session: parent.Session, Turn: parent.Turn})
	if err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) {
		if session == child.Session && turn == child.Turn {
			return "the whole answer", nil
		}
		return "", nil
	})
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, nil, nil); err != nil || n != 0 {
		t.Fatalf("before the subject turn ends: %d %v", n, err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","folder":"/w/1",` +
		`"changes":{"count":1,"files":[{"path":"a.go","change":"modified"}]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.FireOns(DefaultTeam, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ds := deliveriesOf(t, q, parent.Session)
	if len(ds) != 1 {
		t.Fatalf("delivered %d times, want once", len(ds))
	}
	d := ds[0]
	if d.State != Queued || d.Spec.Session != parent.Session || d.Spec.Harness != "claude" ||
		d.Links.ResultOf.Job != child.ID || d.Links.ResultOf.Turn != child.Turn || d.Links.ResultOf.Agent != "claude" ||
		d.Links.Chain == nil || d.Links.Chain.Depth != 1 || d.Links.Chain.By != "" {
		t.Fatalf("delivery: %+v %+v", d.Spec, d.Links)
	}
	for _, want := range []string{"[AgentBus]", "Agent: claude", "Folder: /w/1", "Changed files: 1", "- a.go (modified)", "the whole answer"} {
		if !strings.Contains(d.Spec.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, d.Spec.Prompt)
		}
	}
	live, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range live {
		if o.ID == on.ID {
			t.Fatalf("the one-shot on is still live after firing: %+v", o)
		}
	}
}

// RemoveOn before the subject turn ends stops the delivery: the on is off, so
// FireOns ignores it.
func TestOnRemoved(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	on, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, Maker{Session: parent.Session, Turn: parent.Turn})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveOn(DefaultTeam, on.ID); err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, nil, nil); err != nil || n != 0 {
		t.Fatalf("a removed on delivered: %d %v", n, err)
	}
	if ds := deliveriesOf(t, q, parent.Session); len(ds) != 0 {
		t.Fatalf("a removed on delivered %d turns", len(ds))
	}
	// Removing an unknown on, or one already off, is not found / a no-op.
	if err := q.RemoveOn(DefaultTeam, "on_nope"); err == nil {
		t.Fatal("removing an unknown on should be ErrNotFound")
	}
}

// RegisterOn refuses an event it does not know, so a typo is caught at
// registration, not swallowed by a sweep that never fires.
func TestOnUnknownEvent(t *testing.T) {
	q, _ := open(t)
	if _, err := q.RegisterOn(DefaultTeam, "turn.exploded", "t_1", "s", Maker{Session: "s", Turn: "t_0"}); err == nil {
		t.Fatal("an unknown event should be refused")
	}
}

// An on with no maker turn (the main token registers one, naming no asking
// turn) still delivers: the reply runs on the agent that did the work, the
// subject's own.
func TestOnMainTokenDelivers(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	// No maker turn, as a main-token registration leaves it.
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, Maker{}); err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FireOns(DefaultTeam, nil, nil); err != nil {
		t.Fatal(err)
	}
	ds := deliveriesOf(t, q, parent.Session)
	if len(ds) != 1 {
		t.Fatalf("a main-token on delivered %d times, want once", len(ds))
	}
	if ds[0].Spec.Harness != "claude" {
		t.Fatalf("delivery ran on %q, want the subject's own agent", ds[0].Spec.Harness)
	}
}

// An ask registers its on in the same call that starts the turn (one
// transaction), so there is no window where the turn exists but its on does
// not: the on is live the moment the turn is made, and FireOns delivers.
func TestOnAtomicWithAsk(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"},
		links, testLimits, parent.Session, Maker{Session: parent.Session, Turn: parent.Turn})
	if err != nil {
		t.Fatal(err)
	}
	live, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range live {
		if o.Subject == child.Turn && o.Session == parent.Session {
			found = true
		}
	}
	if !found {
		t.Fatal("the ask did not register its on in the same call that started the turn")
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FireOns(DefaultTeam, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ds := deliveriesOf(t, q, parent.Session); len(ds) != 1 {
		t.Fatalf("delivered %d times, want once", len(ds))
	}
}

// The on lives in the record, not in memory, so a control restart delivers the
// result once and no more: FireOns on a fresh handle to the same store sees
// the on, delivers, and the on_fires row then stops any later fire.
func TestOnSurvivesRestart(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "jobs.db")
	q, err := Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	onFor(t, q, child, links)
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil { // control stops, before the on fired
		t.Fatal(err)
	}
	q2, err := Open(path, c.now) // control starts again on the same store
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()
	total := 0
	for range 3 {
		n, err := q2.FireOns(DefaultTeam, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 1 {
		t.Fatalf("after a restart the result was delivered %d times, want once", total)
	}
	if ds := deliveriesOf(t, q2, parent.Session); len(ds) != 1 {
		t.Fatalf("delivered %d turns, want one", len(ds))
	}
}
