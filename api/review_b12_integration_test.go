//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// A review runs as a normal turn now (read-only retired, B12): its own token
// reads the reviewed turn and its diff through agentbus-api, because it joins
// the reviewed turn's request tree; and its edits never reach the person's
// folder. Checked end to end with codex and claude as the reviewer.
func TestReviewReadsReviewedTurnAndDoesNotApply(t *testing.T) {
	for _, reviewer := range []string{"codex", "claude"} {
		t.Run(reviewer, func(t *testing.T) {
			dir := t.TempDir()
			// The author turn writes its env and holds; a review writes its env to
			// env-review and holds, so the test can drive the review's own token.
			script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
				`case "$p" in "Review a change"*) env > "$D/env-review.tmp"; mv "$D/env-review.tmp" "$D/env-review"; ` +
				`i=0; while [ ! -e "$D/release-review" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done; exit 0;; esac; ` +
				`env > "$D/env-$p.tmp"; pwd > "$D/pwd-$p"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
				`i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done`
			wire := func(_, baseURL string, _ run.Access) (run.Launch, error) {
				return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
					Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
			}
			e := stackEnv(t, stack{wire: wire, maxRuns: 16})

			folder := t.TempDir()
			os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
			w := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"work"}`)
			// Wait for the author turn to be running (it writes its env after its
			// pwd) before reading pwd-work, so the read never races the child.
			tokenOf(t, dir, "work")
			// The author changes its own copy, never the person's folder.
			os.WriteFile(filepath.Join(pwdOf(t, dir, "work"), "a.txt"), []byte("two\n"), 0o644)
			release(dir, "work")
			waitEnded(t, e, w.Turn)

			// The person asks another agent to review it.
			code, out := as(t, e, token, "POST", "/api/v0/turns/"+w.Turn+"/review", `{"agent":"`+reviewer+`"}`)
			var rv Started
			if code != 202 || json.Unmarshal([]byte(out), &rv) != nil || rv.Turn == "" || rv.Session == w.Session {
				t.Fatalf("review: %d %s", code, out)
			}

			// The review reads the reviewed turn and its diff with its OWN token:
			// it is in the reviewed turn's request tree, so these are in scope.
			tok := reviewToken(t, dir)
			if c, o := as(t, e, tok, "GET", "/api/v0/turns/"+w.Turn+"?session="+w.Session, ""); c != 200 || !strings.Contains(o, `"work"`) {
				t.Errorf("the review could not read the reviewed turn with its own token: %d %s", c, o)
			}
			if c, o := as(t, e, tok, "GET", "/api/v0/turns/"+w.Turn+"/diff?session="+w.Session, ""); c != 200 {
				t.Errorf("the review could not read the reviewed turn's diff: %d %s", c, o)
			}
			// But a review is a leaf: it cannot ask for further work, so nothing
			// is ever delivered back to a copy that is torn down at the end.
			if c, o := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"do more"}`); c != 403 || !strings.Contains(o, "reviews the turn and reports a verdict") {
				t.Errorf("a review was allowed to ask for work: %d %s", c, o)
			}

			release(dir, "review")
			waitEnded(t, e, rv.Turn)

			// A review is a turn and ends with a verdict (unknown here: the fake
			// emits no block).
			var one OneTurn
			if c := getJSON(t, e, "/api/v0/turns/"+rv.Turn, &one); c != 200 || one.Turn.Kind != queue.KindReview ||
				one.Turn.Reviews != w.Turn || one.Turn.Verdict == nil {
				t.Fatalf("the review turn: %d %+v", c, one.Turn)
			}
			// The review's edits never reach the person's folder: it still holds
			// what the author left unapplied.
			if got, _ := os.ReadFile(filepath.Join(folder, "a.txt")); string(got) != "one\n" {
				t.Errorf("the person's folder changed after a review: %q", got)
			}
		})
	}
}

// reviewToken waits for the review turn's environment and returns its token.
func reviewToken(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "env-review")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(p); err == nil {
			for _, kv := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(kv, "AGENTBUS_TOKEN="); ok && v != "" {
					return v
				}
			}
			t.Fatalf("the review env has no token: %s", b)
		} else if time.Now().After(deadline) {
			t.Fatal("the review wrote no environment")
		}
	}
}
