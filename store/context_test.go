package store

import (
	"testing"

	"github.com/staysup/agentbus/event"
)

// respondedIn is a model.responded in a given turn with a usage object.
func respondedIn(session, turn string, u event.Usage) event.Event {
	e := ev(event.ModelResponded, session)
	e.Turn = turn
	e.Usage = &u
	return e
}

// TestSessionContext checks a session's per-turn context: the context size is
// the max over a turn's calls of input+cache_read+cache_write, cache and fresh
// are summed, and the session totals take the peak over its turns.
func TestSessionContext(t *testing.T) {
	s := newStore(t)
	// Turn t1: two calls. The context grows between them as the cache fills.
	mustAppend(t, s, respondedIn("sess", "t1", event.Usage{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 0, CacheReadTokens: 0}))
	mustAppend(t, s, respondedIn("sess", "t1", event.Usage{InputTokens: 20, OutputTokens: 5, CacheWriteTokens: 30, CacheReadTokens: 100}))
	// Turn t2: one bigger call.
	mustAppend(t, s, respondedIn("sess", "t2", event.Usage{InputTokens: 40, OutputTokens: 8, CacheWriteTokens: 10, CacheReadTokens: 200}))
	// A responded with no usage object is ignored.
	mustAppend(t, s, respondedIn("sess", "t2", event.Usage{}))
	noUsage := ev(event.ModelResponded, "sess")
	noUsage.Turn = "t2"
	mustAppend(t, s, noUsage)
	// Another session's turn does not leak in.
	mustAppend(t, s, respondedIn("other", "x", event.Usage{InputTokens: 9999}))
	// A model call with no turn (a startup probe) is not a turn and is excluded.
	probe := respondedIn("sess", "", event.Usage{InputTokens: 5000})
	mustAppend(t, s, probe)

	got, err := s.SessionContext("sess", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns != 2 {
		t.Fatalf("turns = %d, want 2 (the no-turn probe is excluded)", got.Turns)
	}
	for _, g := range got.Page {
		if g.Turn == "" {
			t.Errorf("a growth row has an empty turn: the no-turn probe leaked in")
		}
	}
	// t1 context: max(100, 20+30+100=150) = 150. t2 context: 40+10+200 = 250.
	// Peak = 250.
	if got.Peak != 250 {
		t.Errorf("peak = %d, want 250", got.Peak)
	}
	// cache_read summed over all calls: 0+100+200 = 300.
	if got.CacheRead != 300 {
		t.Errorf("cache_read = %d, want 300", got.CacheRead)
	}
	// fresh = sum(input)+sum(cache_write) = (100+20+40)+(0+30+10) = 200.
	if got.Fresh != 200 {
		t.Errorf("fresh = %d, want 200", got.Fresh)
	}
	if got.Output != 23 {
		t.Errorf("output = %d, want 23", got.Output)
	}
	// The page is newest turn first: t2 then t1.
	if len(got.Page) != 2 || got.Page[0].Turn != "t2" || got.Page[1].Turn != "t1" {
		t.Fatalf("page = %+v, want t2 then t1", got.Page)
	}
	t2 := got.Page[0]
	if t2.Context != 250 || t2.CacheRead != 200 || t2.Fresh != 50 || t2.Output != 8 || t2.Calls != 2 {
		t.Errorf("t2 = %+v, want context 250 cache 200 fresh 50 output 8 calls 2", t2)
	}
	t1 := got.Page[1]
	if t1.Context != 150 || t1.CacheRead != 100 || t1.Fresh != 150 || t1.Calls != 2 {
		t.Errorf("t1 = %+v, want context 150 cache 100 fresh 150 calls 2", t1)
	}
}

// TestSessionContextPaging walks the turns a page at a time, newest first.
func TestSessionContextPaging(t *testing.T) {
	s := newStore(t)
	for _, turn := range []string{"a", "b", "c"} {
		mustAppend(t, s, respondedIn("s", turn, event.Usage{InputTokens: 10}))
	}
	first, err := s.SessionContext("s", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Page) != 2 || first.Page[0].Turn != "c" || first.Page[1].Turn != "b" {
		t.Fatalf("first page = %+v, want c then b", first.Page)
	}
	if first.NextBefore == nil {
		t.Fatal("first page has no next_before, want one")
	}
	if first.Turns != 3 {
		t.Errorf("turns total = %d, want 3", first.Turns)
	}
	second, err := s.SessionContext("s", *first.NextBefore, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Page) != 1 || second.Page[0].Turn != "a" {
		t.Fatalf("second page = %+v, want a", second.Page)
	}
	if second.NextBefore != nil {
		t.Errorf("second page next_before = %v, want nil", second.NextBefore)
	}
}

// TestTurnContextTokens: every figure comes from the replies directly in one
// pass, so a reply whose model.requested event is gone still counts.
func TestTurnContextTokens(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, respondedIn("s", "t", event.Usage{InputTokens: 100, OutputTokens: 10}))
	// A bigger reply, and no model.requested was ever recorded for it.
	big := respondedIn("s", "t", event.Usage{InputTokens: 20, OutputTokens: 5, CacheReadTokens: 200, CacheWriteTokens: 30})
	big.Exchange = "x-big"
	mustAppend(t, s, big)
	got, err := s.TurnContextTokens("s", "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Calls != 2 {
		t.Errorf("calls = %d, want 2", got.Calls)
	}
	if got.Peak != 250 { // 20 + 200 + 30
		t.Errorf("peak = %d, want 250", got.Peak)
	}
	if got.CacheRead != 200 {
		t.Errorf("cache_read = %d, want 200", got.CacheRead)
	}
	if got.Fresh != 150 { // (100+20) input + (0+30) cache_write
		t.Errorf("fresh = %d, want 150", got.Fresh)
	}
	if got.Output != 15 {
		t.Errorf("output = %d, want 15", got.Output)
	}
	if got.Exchange != "x-big" {
		t.Errorf("exchange = %q, want x-big", got.Exchange)
	}
}

// TestSessionContextEmpty: a session with no calls is empty, not an error.
func TestSessionContextEmpty(t *testing.T) {
	s := newStore(t)
	got, err := s.SessionContext("nobody", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns != 0 || got.Peak != 0 || len(got.Page) != 0 || got.NextBefore != nil {
		t.Errorf("empty session = %+v, want zero", got)
	}
}
