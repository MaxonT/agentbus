package store

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// LatestPreview is the session's newest changes.previewed event, with the seq
// it was stored at. A later preview supersedes an earlier one, and a session
// with none is not found.
func TestLatestPreview(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	if _, _, found, _ := s.LatestPreview("s1"); found {
		t.Fatal("a session with no preview found one")
	}
	if _, _, err := s.Append(event.Event{ID: "evt_p1", Kind: event.ChangesPreviewed, Session: "s1", Time: now,
		Previewed: &event.Previewed{Turn: "t1", Applies: 0, Count: 3}}); err != nil {
		t.Fatal(err)
	}
	e, seq, found, err := s.LatestPreview("s1")
	if err != nil || !found || e.ID != "evt_p1" || seq < 1 || e.Previewed.Turn != "t1" {
		t.Fatalf("latest: %q seq %d found %v err %v", e.ID, seq, found, err)
	}
	// A newer preview becomes the latest, with a higher seq.
	if _, _, err := s.Append(event.Event{ID: "evt_p2", Kind: event.ChangesPreviewed, Session: "s1", Time: now,
		Previewed: &event.Previewed{Turn: "t2", Applies: 1, Count: 1}}); err != nil {
		t.Fatal(err)
	}
	e2, seq2, _, _ := s.LatestPreview("s1")
	if e2.ID != "evt_p2" || seq2 <= seq || e2.Previewed.Applies != 1 {
		t.Fatalf("latest after the second: %q seq %d applied %d (first seq %d)", e2.ID, seq2, e2.Previewed.Applies, seq)
	}
	// Another session's preview is not this one's.
	if _, _, found, _ := s.LatestPreview("other"); found {
		t.Fatal("another session found a preview")
	}
}
