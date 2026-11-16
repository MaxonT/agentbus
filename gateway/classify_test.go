package gateway

import "testing"

// classify decides a whole command line against a turn's rules: forbid wins,
// then ask, then allow, then the unlisted default. Each part of a chain is
// decided, and the strictest wins; env and VAR= prefixes are stripped so an
// allowed command keeps its decision.
func TestClassify(t *testing.T) {
	r := Rules{
		Allow:    []string{"git fetch", "ls", "git status", "echo"},
		Ask:      []string{"git push"},
		Forbid:   []string{"git checkout", "rm -rf"},
		Unlisted: Ask,
	}
	for _, tc := range []struct {
		line string
		want Decision
		rule string // the forbid/ask entry, "" for allow or an unlisted ask
	}{
		// Allowed, plain and with arguments.
		{"git fetch", Allow, ""},
		{"git fetch origin main", Allow, ""},
		{"ls -la", Allow, ""},
		// The finding (turn t_f088dacca881acf7, 2026-11-16): an allowed command
		// behind an assignment or env prefix, chained, must not be refused.
		{"GIT_TERMINAL_PROMPT=0 git fetch origin", Allow, ""},
		{"env FOO=bar git fetch origin", Allow, ""},
		{"GIT_TERMINAL_PROMPT=0 git fetch origin && echo done", Allow, ""},
		// A chain whose other part is unlisted raises the card for that part.
		{`GIT_TERMINAL_PROMPT=0 git fetch origin; printf 'x'`, Ask, ""},
		// Ask entries.
		{"git push", Ask, "git push"},
		{"git push origin main", Ask, "git push"},
		{"git -C repo push", Ask, "git push"},
		// Forbid wins over allow and over ask, anywhere in a chain.
		{"git checkout .", Refuse, "git checkout"},
		{"rm -rf build", Refuse, "rm -rf"},
		{"ls && git checkout x", Refuse, "git checkout"},
		{"git push && git checkout x", Refuse, "git checkout"},
		{"ls && git push", Ask, "git push"},
		// Unlisted under the ask default: a command on no list.
		{"curl http://x", Ask, ""},
		{"make build", Ask, ""},
		// A shell runs its inner script: the inner governs.
		{"sh -c 'git fetch'", Allow, ""},
		{"sh -c 'rm -rf /'", Refuse, "rm -rf"},
		{"bash -lc 'git push origin main'", Ask, "git push"},
		// A shell with an unreadable (run-time) script asks, like an unlisted.
		{`sh -c "$CMD"`, Ask, ""},
		// A bare shell, with no script, asks.
		{"bash", Ask, ""},
	} {
		dec, rule := classify(r, tc.line)
		if dec != tc.want || rule != tc.rule {
			t.Errorf("classify(%q) = %v %q; want %v %q", tc.line, dec, rule, tc.want, tc.rule)
		}
	}
}

// A wrapper with no real command after it (only assignments, or nothing) runs
// as itself and is decided in full — it never slips through as a bare wrapper.
// (agy round-2 P1: env FOO=1 prints the environment, incl. secrets.)
func TestClassifyWrapperWithoutCommand(t *testing.T) {
	r := Rules{Allow: []string{"git fetch", "ls"}, Forbid: []string{"rm -rf"}, Unlisted: Ask}
	for _, tc := range []struct {
		line string
		want Decision
	}{
		{"env FOO=1", Ask},             // env runs with no command: not Allow
		{"env -i FOO=1", Ask},          // a flag before the assignment
		{"env -u HOME FOO=1", Ask},     // an option that takes a value
		{"sudo FOO=1", Ask},            // sudo with only an assignment after
		{"env", Ask},                   // a bare wrapper
		{"sudo", Ask},                  // a bare wrapper
		{"sh -c 'env FOO=1'", Ask},     // wrapped in a shell: still held
		{"env FOO=1 git fetch", Allow}, // a real command follows: it governs
		{"env FOO=1 ls", Allow},
		{"A=1 B=2", Allow},             // only assignments: no program runs
		{"env FOO=1 rm -rf /", Refuse}, // forbid still wins behind the prefix
	} {
		if dec, _ := classify(r, tc.line); dec != tc.want {
			t.Errorf("classify(%q) = %v; want %v", tc.line, dec, tc.want)
		}
	}
}

// With the unlisted default set to refuse, a command on no list is refused
// rather than held; forbid, ask and allow are decided first.
func TestClassifyUnlistedRefuse(t *testing.T) {
	r := Rules{Allow: []string{"ls"}, Ask: []string{"git push"}, Forbid: []string{"rm -rf"}, Unlisted: Refuse}
	for _, tc := range []struct {
		line string
		want Decision
	}{
		{"ls", Allow},
		{"git push", Ask},
		{"rm -rf x", Refuse},
		{"make build", Refuse}, // unlisted -> refuse
		{"ls && make build", Refuse},
	} {
		if dec, _ := classify(r, tc.line); dec != tc.want {
			t.Errorf("classify(%q) = %v; want %v", tc.line, dec, tc.want)
		}
	}
}

// You can still forbid or ask a wrapper itself (sudo, env), even though its
// inner command governs the unlisted default.
func TestClassifyWrapperRules(t *testing.T) {
	r := Rules{Allow: []string{"ls"}, Forbid: []string{"sudo"}, Ask: []string{"env"}, Unlisted: Ask}
	if dec, rule := classify(r, "sudo ls"); dec != Refuse || rule != "sudo" {
		t.Errorf("sudo ls = %v %q; want Refuse sudo", dec, rule)
	}
	if dec, rule := classify(r, "env FOO=1 ls"); dec != Ask || rule != "env" {
		t.Errorf("env FOO=1 ls = %v %q; want Ask env", dec, rule)
	}
}

// A suggested rule must pass the same shape the settings accept: no leading or
// trailing space, so "always allow" never fails writing it (agy round-2 P2).
func TestSuggestWordsShape(t *testing.T) {
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{"git", "fetch"}, "git fetch"},
		{[]string{"npm", "run", "build"}, "npm run"},
		{[]string{" foo "}, ""},       // leading/trailing space: rejected
		{[]string{"foo ", "arg"}, ""}, // trailing space on the program word
		{[]string{"a,b"}, ""},         // a parenthesis/comma: rejected
		{[]string{""}, ""},
	} {
		if got := suggestWords(tc.words); got != tc.want {
			t.Errorf("suggestWords(%q) = %q; want %q", tc.words, got, tc.want)
		}
	}
}
