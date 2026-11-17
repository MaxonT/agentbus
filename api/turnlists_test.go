package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// The model calls list pages with the turn cursor and no call is cut: a page
// holds the newest limit calls, next_before walks back to the first, and the
// pages together are every call the diff summary counts.
func TestTurnCallsPageEveryCall(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	if _, _, err := e.store.Append(event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Text: "go", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	const n = 5
	for i := 1; i <= n; i++ {
		ex := "ex_" + strconv.Itoa(i)
		if _, _, err := e.store.Append(event.Event{ID: "evt_r" + strconv.Itoa(i), Kind: event.ModelRequested,
			Time: now.Add(time.Duration(i) * time.Second), Session: "s1", Turn: "t_1", Exchange: ex, Model: "claude-x"}); err != nil {
			t.Fatal(err)
		}
	}

	// The diff summary counts them all and embeds no list.
	var d TurnDiff
	if getJSON(t, e, "/api/v0/turns/t_1/diff", &d) != http.StatusOK || d.ModelCalls.Count != n {
		t.Fatalf("diff count %d, want %d", d.ModelCalls.Count, n)
	}

	// Walk the pages from the newest, two at a time, and collect every call.
	seen := []string{}
	before := int64(0)
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("paging did not reach the first call")
		}
		path := "/api/v0/turns/t_1/calls?limit=2"
		if before > 0 {
			path += "&before=" + strconv.FormatInt(before, 10)
		}
		var p TurnCallsPage
		if getJSON(t, e, path, &p) != http.StatusOK {
			t.Fatalf("calls page %s", path)
		}
		if p.Count != n {
			t.Fatalf("page count %d, want %d", p.Count, n)
		}
		if len(p.Calls) == 0 || len(p.Calls) > 2 {
			t.Fatalf("page holds %d calls", len(p.Calls))
		}
		got := []string{}
		for _, c := range p.Calls {
			got = append(got, c.Exchange)
		}
		seen = append(got, seen...) // earlier pages prepend, so seen stays oldest-first
		if p.NextBefore == nil {
			break
		}
		before = *p.NextBefore
	}
	want := []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5"}
	if len(seen) != n {
		t.Fatalf("paged %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged %v, want %v", seen, want)
		}
	}
}

// The tool calls list pages the same way and no tool call is cut.
func TestTurnToolsPageEveryTool(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	if _, _, err := e.store.Append(event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Text: "go", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	const n = 5
	for i := 1; i <= n; i++ {
		id := "tool_" + strconv.Itoa(i)
		if _, _, err := e.store.Append(event.Event{ID: "evt_t" + strconv.Itoa(i), Kind: event.ToolRequested,
			Time: now.Add(time.Duration(i) * time.Second), Session: "s1", Turn: "t_1", Vendor: "anthropic", ToolID: id, ToolName: "Read"}); err != nil {
			t.Fatal(err)
		}
	}
	var d TurnDiff
	if getJSON(t, e, "/api/v0/turns/t_1/diff", &d) != http.StatusOK || d.Tools.Count != n {
		t.Fatalf("diff tools count %d, want %d", d.Tools.Count, n)
	}
	seen := 0
	before := int64(0)
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("paging did not reach the first tool call")
		}
		path := "/api/v0/turns/t_1/tools?limit=2"
		if before > 0 {
			path += "&before=" + strconv.FormatInt(before, 10)
		}
		var p TurnToolsPage
		if getJSON(t, e, path, &p) != http.StatusOK || p.Count != n {
			t.Fatalf("tools page %s: count %d", path, p.Count)
		}
		if len(p.Calls) == 0 || len(p.Calls) > 2 {
			t.Fatalf("page holds %d tool calls", len(p.Calls))
		}
		seen += len(p.Calls)
		if p.NextBefore == nil {
			break
		}
		before = *p.NextBefore
	}
	if seen != n {
		t.Fatalf("paged %d tool calls, want %d", seen, n)
	}
}
