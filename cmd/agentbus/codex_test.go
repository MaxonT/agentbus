package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder is a fake vendor that records the paths it was sent.
type recorder struct {
	mu    sync.Mutex
	paths []string
	srv   *httptest.Server
}

func newRecorder(t *testing.T, reply string) *recorder {
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func startTwoVendors(t *testing.T, home, anthropicURL, chatgptURL string) *service {
	t.Helper()
	// agentbus up sends every call to its relay, which adds the credential
	// and records the call, since 0.5.1 (nosecrets and up_signin tests).
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(anthropicURL),
		chatgpt: mustURL(chatgptURL), logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

func post(t *testing.T, addr, path, body string) int {
	t.Helper()
	resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

const (
	claudeReply = `{"type":"message","model":"claude-x","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
	codexReply  = `{"model":"gpt-x","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`
)

// A path neither vendor claims is answered 404 here and sent nowhere, so no
// sign-in goes to a vendor that did not ask for it. Odd paths are still
// refused with 400 before routing.
func TestUnclaimedPathRefusedLocally(t *testing.T) {
	anth := newRecorder(t, claudeReply)
	cg := newRecorder(t, codexReply)
	svc := startTwoVendors(t, t.TempDir(), anth.srv.URL, cg.srv.URL)

	// GET / redirects to the docs (main.go route); every other method on it
	// is unclaimed.
	if code := post(t, svc.addr, "/", `{}`); code != 404 {
		t.Errorf("POST /: status %d, want 404", code)
	}
	for _, p := range []string{"/v2/messages", "/v1x", "/backend-api/other", "/backend-api/codexx/responses",
		"/s/abc/backend-api/wham/usage", "/s/abc", "/s/a.b/v1/messages", "/responses"} {
		if code := rawGet(t, svc.addr, p, ""); code != 404 {
			t.Errorf("%s: status %d, want 404", p, code)
		}
		if code := post(t, svc.addr, p, `{}`); code != 404 {
			t.Errorf("POST %s: status %d, want 404", p, code)
		}
	}
	if code := rawGet(t, svc.addr, "/backend-api/codex/../../v1/messages", ""); code != 400 {
		t.Errorf("a dot path: status %d, want 400", code)
	}
	if a, c := anth.seen(), cg.seen(); len(a)+len(c) != 0 {
		t.Fatalf("unclaimed paths reached a vendor: anthropic %v, chatgpt %v", a, c)
	}
}
