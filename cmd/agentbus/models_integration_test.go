//go:build integration

package main

// The models a codex agent's sign-in may use, from codex's own model/list:
// control runs codex app-server (here the test binary, speaking its
// JSON-RPC lines) in a clean home of its own, its one call for the list
// goes through control's relay, which adds the ChatGPT sign-in kept on
// control and records the call, and only the models' names and labels are
// kept.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/store"
)

// fakeAppServer is codex app-server as codex-cli 0.164.0 answered on
// 2026-11-14 (models/appserver.go): initialize, then model/list, for which
// it GETs its provider's model_catalog_url with the bearer its env_key
// names, and answers in two pages when that call got 200, with a
// notification before each answer. What it saw goes to
// AGENTBUS_FAKE_APP_OUT as JSON.
func fakeAppServer() {
	var catalog, keyEnv string
	for _, a := range os.Args[1:] {
		if _, v, ok := strings.Cut(a, `.model_catalog_url="`); ok {
			catalog = strings.TrimSuffix(v, `"`)
		}
		if _, v, ok := strings.Cut(a, `.env_key="`); ok {
			keyEnv = strings.TrimSuffix(v, `"`)
		}
	}
	entries, _ := os.ReadDir(os.Getenv("CODEX_HOME"))
	seen := map[string]any{"pid": os.Getpid(), "args": os.Args[1:], "home": os.Getenv("HOME"), "codex_home": os.Getenv("CODEX_HOME"),
		"codex_home_empty": len(entries) == 0, "token_prefix": strings.SplitN(os.Getenv(keyEnv), "_", 2)[0] + "_"}
	save := func() {
		b, _ := json.Marshal(seen)
		os.WriteFile(os.Getenv("AGENTBUS_FAKE_APP_OUT"), b, 0o600)
	}
	save()
	in := bufio.NewScanner(os.Stdin)
	say := func(v any) {
		b, _ := json.Marshal(v)
		os.Stdout.Write(append(b, '\n'))
	}
	for in.Scan() {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		json.Unmarshal(in.Bytes(), &req)
		say(map[string]any{"method": "remoteControl/status/changed", "params": map[string]any{}})
		switch req.Method + req.Params.Cursor {
		case "initialize":
			say(map[string]any{"id": req.ID, "result": map[string]any{"userAgent": "fake", "codexHome": os.Getenv("CODEX_HOME")}})
		case "model/list":
			// The first page asks the catalog once; the next is from it.
			r, _ := http.NewRequest("GET", catalog+"?client_version=0.164.0", nil)
			r.Header.Set("Authorization", "Bearer "+os.Getenv(keyEnv))
			status := 0
			if resp, err := http.DefaultClient.Do(r); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				status = resp.StatusCode
			}
			seen["catalog_status"] = status
			save()
			if status != http.StatusOK {
				say(map[string]any{"id": req.ID, "error": map[string]any{"code": -32603, "message": "someone@example.com is refused"}})
				continue
			}
			say(map[string]any{"id": req.ID, "result": map[string]any{"data": []map[string]any{
				{"id": "gpt-6.1-sol", "model": "gpt-6.1-sol", "displayName": "GPT-6.1-Sol", "hidden": false, "isDefault": true,
					"description": "for someone@example.com"}}, "nextCursor": "p2"}})
		case "model/listp2":
			say(map[string]any{"id": req.ID, "result": map[string]any{"data": []map[string]any{
				{"id": "gpt-6-luna", "model": "gpt-6-luna", "displayName": "GPT-6-Luna"}, {"id": "bad", "model": "bad name"}}, "nextCursor": nil}})
		}
	}
	os.Exit(0)
}

func TestCodexModelsFromAppServer(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "app.json")
	t.Setenv("AGENTBUS_FAKE_APP_SERVER", "1")
	t.Setenv("AGENTBUS_FAKE_APP_OUT", out)
	var installed atomic.Bool
	installed.Store(true)
	look := func(string) (string, error) {
		if !installed.Load() {
			return "", errors.New("not found")
		}
		return self, nil
	}
	chat := newFakeVendor(t)
	home := t.TempDir()
	svc, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL(chat.URL), openrouter: mustURL("http://127.0.0.1:1"), getenv: os.Getenv, lookPath: look, logf: t.Logf,
		heartbeat: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)
	list := func(refresh bool) string {
		path := "/api/v0/harnesses/codex/models?access=subscription"
		if refresh {
			path += "&refresh=true"
		}
		code, raw := getAPI(t, svc, tok, path)
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, raw)
		}
		return raw
	}
	// Not connected: no run, and why.
	if raw := list(false); !strings.Contains(raw, `"models":[]`) || !strings.Contains(raw, "connect your codex subscription") {
		t.Fatalf("not connected: %s", raw)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("codex ran with no sign-in connected")
	}
	// A fake ChatGPT sign-in connected on control.
	if err := subscriptionStore(home).Keep("codex", credentials.Signin{Token: "fake-chatgpt-access-for-tests",
		Refresh: "fake-chatgpt-refresh-for-tests", Account: "fake-account-1", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	raw := list(true)
	if !strings.Contains(raw, `"models":[{"id":"gpt-6.1-sol","label":"GPT-6.1-Sol"},{"id":"gpt-6-luna","label":"GPT-6-Luna"}]`) ||
		!strings.Contains(raw, `"source":"command"`) || !strings.Contains(raw, `"fetched_at":`) || strings.Contains(raw, "example.com") {
		t.Fatalf("listed: %s", raw)
	}
	// The call reached the vendor with the sign-in kept on control in place
	// of the listing's token, and its account.
	if got := chat.seen(); len(got) != 1 || got[0] != "/backend-api/codex/models Bearer fake-chatgpt-access-for-tests" {
		t.Fatalf("the vendor saw %v", got)
	}
	chat.mu.Lock()
	accounts := append([]string(nil), chat.accounts...)
	chat.mu.Unlock()
	if len(accounts) != 1 || accounts[0] != "fake-account-1" {
		t.Fatalf("accounts %v", accounts)
	}
	// What codex saw: a clean home of its own in the data folder, the
	// listing's token, wired as a relayed turn with the catalog.
	var seen struct {
		PID            int      `json:"pid"`
		Args           []string `json:"args"`
		Home           string   `json:"home"`
		CodexHome      string   `json:"codex_home"`
		CodexHomeEmpty bool     `json:"codex_home_empty"`
		Token          string   `json:"token_prefix"`
		CatalogStatus  int      `json:"catalog_status"`
	}
	b, _ := os.ReadFile(out)
	json.Unmarshal(b, &seen)
	args := strings.Join(seen.Args, " ")
	if !strings.HasPrefix(seen.Home, filepath.Join(home, modelHomesDir)+string(filepath.Separator)) || seen.CodexHome != filepath.Join(seen.Home, ".codex") ||
		!seen.CodexHomeEmpty || seen.Token != "abm_" || seen.CatalogStatus != 200 ||
		!strings.HasSuffix(args, " app-server") || !strings.Contains(args, `model_catalog_url="http://`+svc.addr+`/relay/chatgpt/backend-api/codex/models"`) ||
		!strings.Contains(args, `cli_auth_credentials_store="file"`) {
		t.Fatalf("codex saw %s", b)
	}
	// Stopped, and its home gone.
	if err := syscall.Kill(seen.PID, 0); err == nil {
		t.Fatalf("the app-server %d still runs", seen.PID)
	}
	if left, _ := os.ReadDir(filepath.Join(home, modelHomesDir)); len(left) != 0 {
		t.Fatalf("homes left: %v", left)
	}
	// The call is in control's record, by its status, in no session: a
	// listing is control's own, as a subscription connected is.
	waitFor(t, "the listing's call in the record", func() bool {
		recs, _ := svc.store.After(0, store.Filter{Session: "", Only: true}, 1000)
		for _, r := range recs {
			var e struct{ Call, Session, Turn string }
			if r.Kind == "vendor.called" && json.Unmarshal(r.Data, &e) == nil && e.Call == "GET /backend-api/codex/models" {
				return e.Session == "" && e.Turn == ""
			}
		}
		return false
	})
	// And so it is no session of the person's: GET /api/v0/sessions lists
	// none (0.5.7 listed one named "models").
	if code, raw := getAPI(t, svc, tok, "/api/v0/sessions"); code != http.StatusOK || !strings.Contains(raw, `"sessions":[]`) {
		t.Fatalf("sessions: %d %s", code, raw)
	}
	// Within the hour the list kept is used: codex is not run again.
	os.Remove(out)
	if raw := list(false); !strings.Contains(raw, "gpt-6-luna") {
		t.Fatalf("kept: %s", raw)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("codex ran again within the hour")
	}
	// The vendor refuses: the list kept stays, with why, in fixed words.
	chat.mu.Lock()
	chat.ctype = "text/html"
	chat.mu.Unlock()
	if raw := list(true); !strings.Contains(raw, "gpt-6-luna") || !strings.Contains(raw, `"error":"the harness could not list its models just now"`) ||
		strings.Contains(raw, "example.com") {
		t.Fatalf("refused: %s", raw)
	}
	// Not installed where control runs.
	installed.Store(false)
	if raw := list(true); !strings.Contains(raw, `"error":"Codex is not installed where control runs`) {
		t.Fatalf("not installed: %s", raw)
	}
}

// fakeAgyModels is agy models as agy 1.5.9 printed it on 2026-11-14 with the
// gemini provider and a placeholder key: "Fetching available models..." to
// stderr and one "slug<TAB>label" a line to stdout, and nothing on stdout
// without GEMINI_API_KEY. What it saw goes to AGENTBUS_FAKE_AGY_MODELS as
// JSON.
func fakeAgyModels() {
	entries, _ := os.ReadDir(os.Getenv("HOME"))
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "args": os.Args[1:], "home": os.Getenv("HOME"), "home_empty": len(entries) == 0,
		"key": os.Getenv("GEMINI_API_KEY"), "base": os.Getenv("GOOGLE_GEMINI_BASE_URL")})
	os.WriteFile(os.Getenv("AGENTBUS_FAKE_AGY_MODELS"), b, 0o600)
	os.Stderr.WriteString("Fetching available models...\n")
	if os.Getenv("GEMINI_API_KEY") == "" {
		os.Stderr.WriteString("no key\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("gemini-4-argon-medium\tGemini 4 Argon (Medium)\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n")
	os.Exit(0)
}

// agy on a Gemini API key lists the models agy takes as --model, from agy's
// own listing, not the Gemini API's catalogue (0.5.7 offered
// gemini-2.5-flash-preview-tts, which agy refused): control runs agy models
// unmodified in a clean home of its own with a placeholder key, never the
// person's, and keeps only the model lines.
func TestAgyAPIKeyModelsFromItsOwnListing(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "agy.json")
	t.Setenv("AGENTBUS_FAKE_AGY_MODELS", out)
	// The person's own key in control's environment never reaches agy.
	t.Setenv("GEMINI_API_KEY", "AIzaPersonsOwnKeyNeverPassed000000000")
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the Gemini API was asked %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(gemini.Close)
	home := t.TempDir()
	svc, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		openrouter: mustURL("http://127.0.0.1:1"), getenv: os.Getenv, lookPath: func(string) (string, error) { return self, nil },
		logf: t.Logf, heartbeat: 100 * time.Millisecond, apiKeys: map[string]*url.URL{"agy": mustURL(gemini.URL)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)
	path := "/api/v0/harnesses/agy/models?access=api"
	// No key set in Settings: no run, and why.
	if code, raw := getAPI(t, svc, tok, path); code != http.StatusOK || !strings.Contains(raw, "set your Gemini API key in Settings") {
		t.Fatalf("no key: %d %s", code, raw)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("agy ran with no key set")
	}
	if code, _ := sendAPI(t, svc, tok, "PUT", "/api/v0/subscriptions/gemini/key", map[string]any{"key": "AIzaFakeSettingsKey000011112222333344"}); code != http.StatusNoContent {
		t.Fatalf("set the key: %d", code)
	}
	code, raw := getAPI(t, svc, tok, path)
	if code != http.StatusOK || !strings.Contains(raw, `"models":[{"id":"gemini-4-argon-medium","label":"Gemini 4 Argon (Medium)"},{"id":"gemini-3.8-flash-low","label":"Gemini 3.8 Flash (Low)"}]`) ||
		!strings.Contains(raw, `"source":"command"`) || strings.Contains(raw, "Fetching") {
		t.Fatalf("listed: %d %s", code, raw)
	}
	var seen struct {
		PID       int      `json:"pid"`
		Args      []string `json:"args"`
		Home      string   `json:"home"`
		HomeEmpty bool     `json:"home_empty"`
		Key       string   `json:"key"`
		Base      string   `json:"base"`
	}
	b, _ := os.ReadFile(out)
	json.Unmarshal(b, &seen)
	if !strings.HasPrefix(seen.Home, filepath.Join(home, modelHomesDir)+string(filepath.Separator)) || !seen.HomeEmpty ||
		seen.Key != keyListingPlaceholder || !strings.HasPrefix(seen.Base, "http://127.0.0.1:") ||
		strings.Join(seen.Args, " ") != `--settings {"modelProvider":"gemini"} models` {
		t.Fatalf("agy saw %s", b)
	}
	if left, _ := os.ReadDir(filepath.Join(home, modelHomesDir)); len(left) != 0 {
		t.Fatalf("homes left: %v", left)
	}
}
