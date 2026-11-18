package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// call records one model call of a turn: its request and reply, with usage.
func call(t *testing.T, e *env, session, turn, exchange, model string, u event.Usage, pieces ...body.Piece) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := e.store.AppendOnce(event.Event{ID: "ts-" + session + "-" + turn, Kind: event.TurnStarted,
		Time: now, Session: session, Turn: turn}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Append(event.Event{ID: "req-" + exchange, Kind: event.ModelRequested,
		Time: now, Session: session, Turn: turn, Exchange: exchange, Model: model}); err != nil {
		t.Fatal(err)
	}
	usage := u
	if _, _, err := e.store.Append(event.Event{ID: "res-" + exchange, Kind: event.ModelResponded,
		Time: now, Session: session, Turn: turn, Exchange: exchange, Usage: &usage}); err != nil {
		t.Fatal(err)
	}
	if len(pieces) > 0 {
		size := 0
		for _, p := range pieces {
			size += len(p.JSON)
		}
		c := body.Call{Exchange: exchange, Session: session, Vendor: "anthropic", Time: now, Size: size,
			Request: body.Request{Top: json.RawMessage(`{"model":"` + model + `"}`)}}
		c.Pieces = pieces
		if err := e.store.KeepCall(c); err != nil {
			t.Fatal(err)
		}
	}
}

func piece(kind, label, text string) body.Piece {
	return body.Piece{Kind: kind, Label: label, JSON: json.RawMessage(text)}
}

// TestTurnContextRoute: the turn's context size is its biggest call, the cache
// split is summed, and made_of breaks the biggest prompt into its parts.
func TestTurnContextRoute(t *testing.T) {
	e := newEnv(t)
	// First call: small, no cache. Second call: bigger, with cache and pieces.
	call(t, e, "s1", "t1", "x1", "m", event.Usage{InputTokens: 100, OutputTokens: 10})
	call(t, e, "s1", "t1", "x2", "m", event.Usage{InputTokens: 20, OutputTokens: 5, CacheReadTokens: 200, CacheWriteTokens: 30},
		piece(body.System, "", `{"type":"text","text":"you are a careful agent and you help"}`),
		piece(body.Tool, "Read", `{"name":"Read","description":"read a file from disk here"}`),
		piece(body.Tool, "Bash", `{"name":"Bash","description":"run a shell command on host"}`),
		piece(body.Message, "user", `{"role":"user","content":"please read the main file now"}`),
		piece(body.Message, "assistant", `{"role":"assistant","content":"ok"}`))

	resp, bodyStr := do(t, e.req(t, "GET", "/api/v0/turns/t1/context", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, bodyStr)
	}
	var tc TurnContext
	if err := json.Unmarshal([]byte(bodyStr), &tc); err != nil {
		t.Fatal(err)
	}
	// context = max(100, 20+200+30=250) = 250.
	if tc.ContextTokens != 250 {
		t.Errorf("context_tokens = %d, want 250", tc.ContextTokens)
	}
	if tc.CacheReadTokens != 200 {
		t.Errorf("cache_read = %d, want 200", tc.CacheReadTokens)
	}
	// fresh = (100+20)+(0+30) = 150.
	if tc.FreshTokens != 150 {
		t.Errorf("fresh = %d, want 150", tc.FreshTokens)
	}
	if tc.OutputTokens != 15 || tc.Calls != 2 {
		t.Errorf("output=%d calls=%d, want 15 and 2", tc.OutputTokens, tc.Calls)
	}
	if tc.MadeOf == nil {
		t.Fatal("made_of is null, want the second call's pieces")
	}
	if tc.MadeOf.Exchange != "x2" || tc.MadeOf.Vendor != "anthropic" {
		t.Errorf("made_of of %q (%s), want x2 anthropic", tc.MadeOf.Exchange, tc.MadeOf.Vendor)
	}
	// Parts: system, tools (2 grouped as one), user, assistant = 4 parts.
	byKind := map[string]ContextPart{}
	for _, p := range tc.MadeOf.Parts {
		k := p.Kind + "/" + p.Label
		byKind[k] = p
	}
	if len(tc.MadeOf.Parts) != 4 {
		t.Fatalf("parts = %d, want 4: %+v", len(tc.MadeOf.Parts), tc.MadeOf.Parts)
	}
	if byKind["tool/"].Count != 2 {
		t.Errorf("tools grouped as %d pieces, want 2", byKind["tool/"].Count)
	}
	if byKind["message/user"].Count != 1 || byKind["message/assistant"].Count != 1 {
		t.Errorf("messages not split by role: %+v", tc.MadeOf.Parts)
	}
	if byKind["system/"].Count != 1 {
		t.Errorf("system part = %+v, want one", byKind["system/"])
	}
	if tc.MadeOf.Note == "" {
		t.Error("made_of has no note")
	}
}

// TestTurnContextReplyWithoutRequest: a kept reply whose model.requested event
// is gone still counts in the turn's call count and peak (read from replies,
// not request/reply pairs).
func TestTurnContextReplyWithoutRequest(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	if _, err := e.store.AppendOnce(event.Event{ID: "ts-sx-tx", Kind: event.TurnStarted, Time: now, Session: "sx", Turn: "tx"}); err != nil {
		t.Fatal(err)
	}
	// Only a model.responded, no model.requested.
	if _, _, err := e.store.Append(event.Event{ID: "res-lonely", Kind: event.ModelResponded, Time: now,
		Session: "sx", Turn: "tx", Exchange: "xl", Usage: &event.Usage{InputTokens: 100, OutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	resp, bodyStr := do(t, e.req(t, "GET", "/api/v0/turns/tx/context", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, bodyStr)
	}
	var tc TurnContext
	if err := json.Unmarshal([]byte(bodyStr), &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Calls != 1 || tc.ContextTokens != 100 || tc.OutputTokens != 10 {
		t.Errorf("calls=%d context=%d output=%d, want 1/100/10", tc.Calls, tc.ContextTokens, tc.OutputTokens)
	}
}

// TestSessionContextRoute: the session's totals and per-turn growth over HTTP.
func TestSessionContextRoute(t *testing.T) {
	e := newEnv(t)
	call(t, e, "s2", "ta", "xa", "m", event.Usage{InputTokens: 50, OutputTokens: 5})
	call(t, e, "s2", "tb", "xb", "m", event.Usage{InputTokens: 60, OutputTokens: 6, CacheReadTokens: 100})

	resp, bodyStr := do(t, e.req(t, "GET", "/api/v0/sessions/s2/context", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, bodyStr)
	}
	var sc SessionContext
	if err := json.Unmarshal([]byte(bodyStr), &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Turns != 2 {
		t.Errorf("turns = %d, want 2", sc.Turns)
	}
	// peak = max(50, 60+100=160) = 160.
	if sc.PeakContextTokens != 160 {
		t.Errorf("peak = %d, want 160", sc.PeakContextTokens)
	}
	if sc.CacheReadTokens != 100 || sc.FreshTokens != 110 || sc.OutputTokens != 11 {
		t.Errorf("totals cache=%d fresh=%d output=%d, want 100/110/11", sc.CacheReadTokens, sc.FreshTokens, sc.OutputTokens)
	}
	if len(sc.Growth) != 2 || sc.Growth[0].Turn != "tb" || sc.Growth[1].Turn != "ta" {
		t.Fatalf("growth = %+v, want tb then ta", sc.Growth)
	}
	if sc.Growth[0].ContextTokens != 160 {
		t.Errorf("tb context = %d, want 160", sc.Growth[0].ContextTokens)
	}
}
