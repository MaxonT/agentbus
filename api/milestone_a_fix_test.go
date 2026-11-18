package api

import (
	"fmt"
	"testing"

	"github.com/staysup/agentbus/event"
)

// buildActivity reads every page of a turn's events, so a model call after many
// tool events -- past where the old 200-page cap stopped -- is still counted
// and listed, not silently dropped from /calls and /tools.
func TestBuildActivityReadsEveryPage(t *testing.T) {
	e := newEnv(t)
	old := activityPageLimit
	activityPageLimit = 10
	defer func() { activityPageLimit = old }()
	// 200 pages of tool events (2000 at 10 a page), then a model call on page
	// 201: the cap read only 200 pages and dropped it.
	for i := 0; i < 2000; i++ {
		if _, _, err := e.store.Append(event.Event{ID: fmt.Sprintf("tr%d", i), Kind: event.ToolRequested,
			Session: "s1", Turn: "t1", ToolID: fmt.Sprintf("x%d", i), ToolName: "bash"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.store.Append(event.Event{ID: "mr", Kind: event.ModelRequested,
		Session: "s1", Turn: "t1", Exchange: "ex1", Model: "claude"}); err != nil {
		t.Fatal(err)
	}
	act, err := e.api.buildActivity("t1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(act.Calls) != 1 || act.Calls[0].Model != "claude" {
		t.Fatalf("the model call after many tool events was dropped: %+v", act.Calls)
	}
	if len(act.Tools) != 2000 {
		t.Fatalf("tools read: %d, want 2000", len(act.Tools))
	}
}
