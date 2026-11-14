package models

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeServer answers the lines of in on out as answer says, one JSON
// object a line, until in ends; it returns the requests it read.
func fakeServer(t *testing.T, answer func(id int, method string, params json.RawMessage) []string) (io.Writer, io.Reader, func() []string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	got := make(chan []string, 1)
	// Written apart from the reading, as an OS pipe's buffer lets a real
	// server write while its client writes.
	lines := make(chan string, 100)
	go func() {
		for line := range lines {
			if _, err := io.WriteString(outW, line+"\n"); err != nil {
				break
			}
		}
		outW.Close()
	}()
	go func() {
		var reqs []string
		sc := bufio.NewScanner(inR)
		for sc.Scan() {
			reqs = append(reqs, sc.Text())
			var r struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			json.Unmarshal(sc.Bytes(), &r)
			for _, line := range answer(r.ID, r.Method, r.Params) {
				lines <- line
			}
		}
		close(lines)
		got <- reqs
	}()
	t.Cleanup(func() { inW.Close(); outR.Close() })
	return inW, outR, func() []string { inW.Close(); return <-got }
}

// codex-cli 0.164.0's answers (2026-11-14): initialize's result, a
// notification, then model/list's pages. Only the names and labels are
// kept; a name that is not one is left out.
func TestAppServerLists(t *testing.T) {
	in, out, reqs := fakeServer(t, func(id int, method string, params json.RawMessage) []string {
		switch method {
		case "initialize":
			return []string{`{"id":1,"result":{"userAgent":"agentbus/0.164.0","codexHome":"/h","platformFamily":"unix","platformOs":"macos"}}`,
				`{"method":"remoteControl/status/changed","emittedAtMs":1,"params":{}}`}
		case "model/list":
			if !strings.Contains(string(params), `"p2"`) {
				// A request of the server's own, with an id, is not an answer.
				return []string{`{"id":2,"method":"account/chatgptAuthTokens/refresh","params":{}}`,
					`{"id":2,"result":{"data":[{"id":"gpt-6.1-sol","model":"gpt-6.1-sol","displayName":"GPT-6.1-Sol","hidden":false,"isDefault":true,"description":"someone@example.com"}],"nextCursor":"p2"}}`}
			}
			return []string{`{"id":3,"result":{"data":[{"id":"x","model":"gpt-6-luna","displayName":"GPT-6-Luna"},{"id":"y","model":"bad name"}],"nextCursor":null}}`}
		}
		return nil
	})
	ms, err := AppServer(context.Background(), in, out, "agentbus", "0.5.7")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0] != (Model{ID: "gpt-6.1-sol", Label: "GPT-6.1-Sol"}) || ms[1] != (Model{ID: "gpt-6-luna", Label: "GPT-6-Luna"}) {
		t.Fatalf("%+v", ms)
	}
	want := []string{`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"agentbus","version":"0.5.7"}}}`,
		`{"method":"initialized"}`, `{"id":2,"method":"model/list","params":{}}`, `{"id":3,"method":"model/list","params":{"cursor":"p2"}}`}
	if got := reqs(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An error answer, a shape that is not a list, or a server that ends is
// ErrServer, in fixed words: nothing the server said is passed on.
func TestAppServerFails(t *testing.T) {
	for name, answer := range map[string]func(int, string, json.RawMessage) []string{
		"list refused": func(id int, m string, _ json.RawMessage) []string {
			if m == "initialize" {
				return []string{`{"id":1,"result":{}}`}
			}
			if m == "model/list" {
				return []string{`{"id":2,"error":{"code":-32603,"message":"someone@example.com is not allowed"}}`}
			}
			return nil
		},
		"initialize refused": func(id int, m string, _ json.RawMessage) []string {
			return []string{`{"id":1,"error":{"code":-32600,"message":"no"}}`}
		},
		"not a list": func(id int, m string, _ json.RawMessage) []string {
			if m == "initialize" {
				return []string{`{"id":1,"result":{}}`}
			}
			if m == "model/list" {
				return []string{`{"id":2,"result":{"models":[]}}`}
			}
			return nil
		},
		"not JSON": func(int, string, json.RawMessage) []string { return []string{"Error: someone@example.com"} },
		"ends":     func(int, string, json.RawMessage) []string { return nil },
	} {
		in, out, reqs := fakeServer(t, answer)
		if name == "ends" {
			go reqs()
		}
		ms, err := AppServer(context.Background(), in, out, "agentbus", "dev")
		if !errors.Is(err, ErrServer) || ms != nil || strings.Contains(err.Error(), "example.com") {
			t.Errorf("%s: %v %v", name, ms, err)
		}
	}
}

// A server that never answers is given up on when ctx ends.
func TestAppServerWaitsForCtx(t *testing.T) {
	in, out, _ := fakeServer(t, func(int, string, json.RawMessage) []string { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AppServer(ctx, in, out, "agentbus", "dev"); !errors.Is(err, ErrServer) {
		t.Fatal(err)
	}
}
