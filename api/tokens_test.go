package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A turn whose folder is on a machine other than this control plane's own
// (a remote runner): control holds the folder by reference only and no path
// of it. askChecks must still let a sub-agent name the session's own folder
// by reference (no path check needed), filling in the folder's machine; a
// request that needs a path check on that machine is refused here, since
// control cannot answer it.
func TestAskChecksRemoteFolderByReference(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	// Control's own folders are on a different machine than the session's.
	fp := filepath.Join(dir, "folders.json")
	if err := os.WriteFile(fp, []byte(`{"connector":"con_0123456789abcdef","folders":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fr, err := folders.Open(fp)
	if err != nil {
		t.Fatal(err)
	}
	const remote = "con_aaaaaaaaaaaaaaaa" // the session's machine, not control's own
	const sessRef = "fld_fedcba9876543210"
	lim := queue.Limits{MaxDepth: 2, MaxPerTurn: 10, MaxPerTree: 30}
	j, _, err := q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Harness: "claude", FolderRef: sessRef, Machine: remote, Prompt: "hi", Isolate: true}, queue.Links{}, lim)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{cfg: Config{Queue: q, Store: st, Folders: fr}}
	g := run.Grant{Session: j.Session, Root: j.Session, Turn: j.Turn}
	perm := settings.DefaultPolicy()
	// check runs askChecks for a new sub-session and returns the refusal
	// status (0 when allowed) and the body the request was filled with.
	check := func(body *TurnRequest) (int, *TurnRequest) {
		w := httptest.NewRecorder()
		if _, ok := a.askChecks(w, g, perm, body, false); ok {
			return 0, body
		}
		return w.Code, body
	}

	// The session's own folder by reference, on its machine: allowed, kept.
	if code, b := check(&TurnRequest{FolderRef: sessRef, Machine: remote, Prompt: "go"}); code != 0 ||
		b.FolderRef != sessRef || b.Machine != remote {
		t.Fatalf("same ref on the remote runner refused: %d (ref %q, connector %q)", code, b.FolderRef, b.Machine)
	}
	// Its reference without a connector: allowed, the folder's machine filled in.
	if code, b := check(&TurnRequest{FolderRef: sessRef, Prompt: "go"}); code != 0 || b.Machine != remote {
		t.Fatalf("ref without connector: %d, connector %q, want %q", code, b.Machine, remote)
	}
	// No folder at all: the session's own folder by reference, on its machine.
	if code, b := check(&TurnRequest{Prompt: "go"}); code != 0 || b.FolderRef != sessRef || b.Machine != remote {
		t.Fatalf("no folder: %d (ref %q, connector %q)", code, b.FolderRef, b.Machine)
	}
	// A reference on another machine than the session's: refused.
	if code, _ := check(&TurnRequest{FolderRef: sessRef, Machine: "con_bbbbbbbbbbbbbbbb", Prompt: "go"}); code != http.StatusForbidden {
		t.Fatalf("a folder on another machine was not refused: %d", code)
	}
	// A path, which only the folder's machine can check: refused here, since
	// the folder is remote and control holds no path of it.
	if code, _ := check(&TurnRequest{Folder: dir, Prompt: "go"}); code != http.StatusForbidden {
		t.Fatalf("a path on the remote runner was not refused: %d", code)
	}
}

// A turn whose child cannot start leaves no token behind.
func TestTurnTokenRevokedWhenChildDoesNotStart(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: filepath.Join(t.TempDir(), "missing")}, nil
	})
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"x"}`)))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if n := e.eng.LiveTokens(); n != 0 {
		t.Errorf("%d live tokens after a child that did not start", n)
	}
}

// GET /api/v0/sessions/{id}/search refuses a bad query, and pages.
func TestSearchRoute(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	for i, text := range []string{"one needle", "two needle", "three needle"} {
		if _, _, err := e.store.Append(event.Event{ID: "e" + string(rune('0'+i)), Kind: event.TurnStarted, Time: now,
			Session: "s1", Turn: "t_" + string(rune('a'+i)), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) (int, store.SearchResult, string) {
		resp, out := do(t, e.req(t, "GET", "/api/v0/sessions/s1/search"+q, nil))
		var res store.SearchResult
		_ = json.Unmarshal([]byte(out), &res)
		return resp.StatusCode, res, out
	}
	for _, q := range []string{"", "?q=", "?q=a%00b", "?q=%ff", "?q=" + strings.Repeat("a", 257), "?q=a&q=b",
		"?q=a&limit=0", "?q=a&limit=201", "?q=a&before=x", "?q=a&before=1"} {
		if code, _, out := get(q); code != 400 {
			t.Errorf("%q: %d %s, want 400", q, code, out)
		}
	}
	code, res, out := get("?q=NEEDLE&limit=2")
	if code != 200 || len(res.Hits) != 2 || res.Hits[0].Snippet != "three needle" || res.NextBefore == nil {
		t.Fatalf("page 1: %d %s", code, out)
	}
	code, res, out = get("?q=NEEDLE&limit=2&before=" + url.QueryEscape(*res.NextBefore))
	if code != 200 || len(res.Hits) != 1 || res.Hits[0].Snippet != "one needle" || res.NextBefore != nil {
		t.Fatalf("page 2: %d %s", code, out)
	}
	if code, res, _ := get("?q=haystack"); code != 200 || len(res.Hits) != 0 || res.NextBefore != nil {
		t.Errorf("no match: %d %+v", code, res)
	}
}

// inTurn stores the turn's turn.started and a model.requested of the call in
// it, as a run and the gateway do: GET /api/v0/turns/{id}/calls/{call}
// answers only a turn's own calls, in the turn's own session.
func inTurn(t *testing.T, e *env, session, turn, exchange string) {
	t.Helper()
	if _, err := e.store.AppendOnce(event.Event{ID: "ts-" + session + "-" + turn, Kind: event.TurnStarted, Time: time.Now().UTC(),
		Session: session, Turn: turn}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Append(event.Event{ID: "req-" + exchange, Kind: event.ModelRequested, Time: time.Now().UTC(),
		Session: session, Turn: turn, Exchange: exchange}); err != nil {
		t.Fatal(err)
	}
}

// GET /api/v0/turns/{id}/calls/{call}/pieces/{n} answers the stored bytes,
// unchanged, for a call of that turn only.
func TestCallPieceRoute(t *testing.T) {
	e := newEnv(t)
	inTurn(t, e, "s1", "t_1", "x-1")
	inTurn(t, e, "s1", "t_2", "x-2")
	msg := `{"role":"user", "content":"<a> & é"}`
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{
			{Kind: body.System, Label: "text", JSON: json.RawMessage(`"Be brief."`)},
			{Kind: body.Message, Label: "user", JSON: json.RawMessage(msg)}}}}); err != nil {
		t.Fatal(err)
	}
	resp, out := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/1", nil))
	sum := sha256.Sum256([]byte(msg))
	h := resp.Header
	if resp.StatusCode != 200 || out != msg || h.Get("Content-Type") != "application/json" ||
		h.Get("X-Piece-Hash") != hex.EncodeToString(sum[:]) || h.Get("X-Piece-Kind") != "message" ||
		h.Get("X-Piece-Label") != "user" || h.Get("X-Piece-Count") != "2" {
		t.Fatalf("piece 1: %d %q %v", resp.StatusCode, out, h)
	}
	if resp, out := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/0", nil)); resp.StatusCode != 200 || out != `"Be brief."` {
		t.Errorf("piece 0: %d %q", resp.StatusCode, out)
	}
	for _, p := range []string{"/api/v0/turns/t_1/calls/x-1/pieces/2", "/api/v0/turns/t_1/calls/x-1/pieces/reply",
		"/api/v0/turns/t_1/calls/x-2/pieces/0", "/api/v0/turns/t_2/calls/x-1/pieces/0", "/api/v0/turns/t_1/calls/x-3/pieces/0",
		"/api/v0/turns/t_1/calls/x-1/pieces/01", "/api/v0/turns/t_1/calls/x-1/pieces/-1"} {
		if resp, out := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != 404 {
			t.Errorf("%s: %d %s, want 404", p, resp.StatusCode, out)
		}
	}
	reply := `{"role":"assistant","content":[{"type":"text","text":"ok"}]}`
	if err := e.store.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "end_turn", Time: time.Now(), Message: json.RawMessage(reply)}); err != nil {
		t.Fatal(err)
	}
	if resp, out := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/reply", nil)); resp.StatusCode != 200 || out != reply ||
		resp.Header.Get("X-Piece-Label") != "assistant" {
		t.Errorf("reply: %d %q", resp.StatusCode, out)
	}
}

// With an origin configured, a piece's headers are exposed to that origin's
// browser cockpit; without one, nothing is exposed.
func TestCallPieceExposesHeadersToOrigin(t *testing.T) {
	e := newEnv(t) // configured with origin
	inTurn(t, e, "s1", "t_1", "x-1")
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	r := e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/0", nil)
	r.Header.Set("Origin", origin)
	resp, _ := do(t, r)
	exp := resp.Header.Get("Access-Control-Expose-Headers")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("%d, allow-origin %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	for _, h := range []string{"X-Piece-Kind", "X-Piece-Hash", "X-Piece-Count", "X-Piece-Label"} {
		if !strings.Contains(exp, h) {
			t.Errorf("Access-Control-Expose-Headers %q lacks %s", exp, h)
		}
	}
	r = e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/0", nil)
	r.Header.Set("Origin", "http://evil.example")
	if resp, _ := do(t, r); resp.Header.Get("Access-Control-Expose-Headers") != "" {
		t.Error("headers exposed to an origin that is not allowed")
	}
}

// Without -origin, every origin gets the piece's headers exposed.
func TestCallPieceExposesHeadersToAnyOrigin(t *testing.T) {
	e := newEnvOrigin(t, filepath.Join(t.TempDir(), "events.db"), "")
	inTurn(t, e, "s1", "t_1", "x-1")
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	r := e.req(t, "GET", "/api/v0/turns/t_1/calls/x-1/pieces/0", nil)
	r.Header.Set("Origin", "http://example.test")
	resp, _ := do(t, r)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "X-Piece-Hash") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
}
