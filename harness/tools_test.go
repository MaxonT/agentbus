package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const toolsURL = "http://127.0.0.1:8787/api/v0/mcp"

// flagValue is the value after flag in args, or "".
func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// A claude turn with the tool server loads it inline and only it, may call
// its tools without asking, and has none of Claude Code's own agent tools.
// The token is never in an argument: Claude Code fills ${AGENTBUS_TOKEN} in.
func TestClaudeToolServer(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		a := claude{}.Args(Turn{Tools: toolsURL, Writes: Writes{ReadOnly: readOnly}, Prompt: "hi"})
		var cfg struct {
			MCPServers map[string]struct {
				Type, URL string
				Headers   map[string]string
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(flagValue(a, "--mcp-config")), &cfg); err != nil {
			t.Fatalf("read_only %v: %q: %v", readOnly, a, err)
		}
		s := cfg.MCPServers[ToolServer]
		if len(cfg.MCPServers) != 1 || s.Type != "http" || s.URL != toolsURL || s.Headers["Authorization"] != "Bearer ${AGENTBUS_TOKEN}" {
			t.Errorf("read_only %v: config %+v", readOnly, cfg)
		}
		if slices.Contains(a, "--strict-mcp-config") != readOnly {
			t.Errorf("read_only %v: --strict-mcp-config on a read-only turn only: %q", readOnly, a)
		}
		if !slices.Contains(strings.Split(flagValue(a, "--allowedTools"), ","), "mcp__agentbus") {
			t.Errorf("read_only %v: tools not allowed: %q", readOnly, a)
		}
		denied := strings.Split(flagValue(a, "--disallowedTools"), ",")
		for _, n := range []string{"Task", "Agent", "ListAgents", "SendMessage"} {
			if !slices.Contains(denied, n) {
				t.Errorf("read_only %v: %s not denied: %q", readOnly, n, denied)
			}
		}
		if a[len(a)-1] != "hi" || a[len(a)-2] != "--" {
			t.Errorf("read_only %v: prompt not last: %q", readOnly, a)
		}
	}
	// subagents = false as well: each name is denied once.
	a := claude{}.Args(Turn{Tools: toolsURL, NoSubagents: true, Prompt: "hi"})
	denied := strings.Split(flagValue(a, "--disallowedTools"), ",")
	for _, n := range []string{"Task", "Agent", "Workflow", "ListAgents", "SendMessage"} {
		if c := strings.Count(","+strings.Join(denied, ",")+",", ","+n+","); c != 1 {
			t.Errorf("%s denied %d times: %q", n, c, denied)
		}
	}
}

// Without the tool server, nothing of it is on the command line, and Claude
// Code keeps its own agent tools. --no-chrome is there on every turn, and
// --strict-mcp-config on a read-only one, so no tool of the person's loads
// there.
func TestClaudeNoToolServer(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		turn := Turn{Prompt: "hi"}
		turn.Writes.ReadOnly = readOnly
		a := claude{}.Args(turn)
		if c := slices.Index(a, "--no-chrome"); c < 0 || c > slices.Index(a, "--") {
			t.Errorf("read_only %v: no --no-chrome before the prompt: %q", readOnly, a)
		}
		if c := slices.Index(a, "--strict-mcp-config"); (c >= 0 && c < slices.Index(a, "--")) != readOnly {
			t.Errorf("read_only %v: --strict-mcp-config: %q", readOnly, a)
		}
		all := strings.Join(a, " ")
		for _, s := range []string{"--mcp-config", "mcp__agentbus", "ListAgents", "Task"} {
			if strings.Contains(all, s) {
				t.Errorf("read_only %v: %s without the tool server: %q", readOnly, s, a)
			}
		}
	}
}

// A codex turn gets the tool server by -c for this run, its bearer read from
// AGENTBUS_TOKEN by codex itself, before the prompt and before resume.
func TestCodexToolServer(t *testing.T) {
	for _, resume := range []string{"", "r-1"} {
		a := codex{}.Args(Turn{Tools: toolsURL, ResumeID: resume, Prompt: "hi"})
		url := slices.Index(a, `mcp_servers.agentbus.url="`+toolsURL+`"`)
		bearer := slices.Index(a, `mcp_servers.agentbus.bearer_token_env_var="AGENTBUS_TOKEN"`)
		if url < 1 || bearer < 1 || a[url-1] != "-c" || a[bearer-1] != "-c" {
			t.Fatalf("resume %q: %q", resume, a)
		}
		r := slices.Index(a, "resume")
		if resume != "" && (r < 0 || r < bearer) {
			t.Errorf("resume %q: tool server after resume: %q", resume, a)
		}
	}
	if a := (codex{}).Args(Turn{Prompt: "hi"}); strings.Contains(strings.Join(a, " "), "mcp_servers") {
		t.Errorf("tool server without Tools: %q", a)
	}
}

// Which harnesses take the tool server: claude and codex, not agy yet.
func TestToolServerHarnesses(t *testing.T) {
	for name, want := range map[string]bool{"claude": true, "codex": true, "agy": false} {
		h, ok := Get(name)
		if !ok || h.Info().ToolServer != want {
			t.Errorf("%s: ToolServer %v, want %v", name, ok && h.Info().ToolServer, want)
		}
	}
}

// A codex config of the person's that defines the tool server's name wins:
// the turn gets none, since -c merges into their entry field by field. In
// $CODEX_HOME, in a .codex/config.toml above the folder, as a table or a
// dotted key; another server, or none, leaves it in. A file that does not
// parse is taken to define it.
func TestCodexOwnTools(t *testing.T) {
	for _, tc := range []struct {
		name, home, project string
		want                bool
	}{
		{"none", "", "", false},
		{"another server", "[mcp_servers.other]\ncommand = \"x\"\n", "", false},
		{"stdio entry", "[mcp_servers.agentbus]\ncommand = \"true\"\n", "", true},
		{"disabled http entry", "[mcp_servers.agentbus]\nurl = \"http://old\"\nenabled = false\n", "", true},
		{"dotted key", "mcp_servers.agentbus.url = \"http://old\"\n", "", true},
		{"project config", "", "[mcp_servers.agentbus]\nurl = \"http://old\"\n", true},
		{"broken file", "[mcp_servers\n", "", true},
	} {
		root := t.TempDir()
		home, folder := filepath.Join(root, "codex"), filepath.Join(root, "repo", "sub")
		for _, d := range []string{home, filepath.Join(root, "repo", ".codex"), folder} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if tc.home != "" {
			_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.home), 0o600)
		}
		if tc.project != "" {
			_ = os.WriteFile(filepath.Join(root, "repo", ".codex", "config.toml"), []byte(tc.project), 0o600)
		}
		why := codex{}.OwnTools([]string{"CODEX_HOME=" + home}, folder)
		if (why != "") != tc.want {
			t.Errorf("%s: %q, want left out %v", tc.name, why, tc.want)
		}
	}
	// claude: a server of the person's named as the tool server wins, in the
	// .claude.json Claude Code reads; another name, or none, leaves it in.
	for _, tc := range []struct {
		name, file string
		want       bool
	}{
		{"none", "", false},
		{"another server", `{"mcpServers":{"linear":{"type":"http","url":"http://x"}}}`, false},
		{"tool server's name", `{"mcpServers":{"agentbus":{"command":"x"}}}`, true},
	} {
		conf := t.TempDir()
		if tc.file != "" {
			_ = os.WriteFile(filepath.Join(conf, ".claude.json"), []byte(tc.file), 0o600)
		}
		why := claude{}.OwnTools([]string{"CLAUDE_CONFIG_DIR=" + conf}, t.TempDir())
		if (why != "") != tc.want {
			t.Errorf("claude %s: %q, want left out %v", tc.name, why, tc.want)
		}
	}
}

// claude reads the person's server names from the .claude.json it reads:
// $CLAUDE_CONFIG_DIR's, else $HOME's. Names only, sorted; a missing or
// broken file names none.
func TestClaudeOwnServers(t *testing.T) {
	home := t.TempDir()
	conf := filepath.Join(home, ".claude")
	_ = os.MkdirAll(conf, 0o700)
	_ = os.WriteFile(filepath.Join(conf, ".claude.json"), []byte(`{"numStartups":3,"mcpServers":{"posthog":{"command":"x","env":{"K":"v"}},"dune":{"type":"http","url":"http://d"}}}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"home":{"command":"y"}}}`), 0o600)
	if got := (claude{}).OwnServers([]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + conf}); !slices.Equal(got, []string{"dune", "posthog"}) {
		t.Errorf("config dir: %v", got)
	}
	if got := (claude{}).OwnServers([]string{"HOME=" + home}); !slices.Equal(got, []string{"home"}) {
		t.Errorf("home: %v", got)
	}
	_ = os.WriteFile(filepath.Join(conf, ".claude.json"), []byte(`{"mcpServers":`), 0o600)
	if got := (claude{}).OwnServers([]string{"CLAUDE_CONFIG_DIR=" + conf}); len(got) != 0 {
		t.Errorf("broken file: %v", got)
	}
	if got := (claude{}).OwnServers([]string{"CLAUDE_CONFIG_DIR=" + t.TempDir()}); len(got) != 0 {
		t.Errorf("no file: %v", got)
	}
	if got := OwnServers(agy{}, []string{"HOME=" + home}); len(got) != 0 {
		t.Errorf("agy: %v", got)
	}
}

// An edit turn loads the person's own servers (no --strict-mcp-config) and
// may call each without asking, beside the tool server; a name Claude Code
// would rewrite gets no rule. A read-only turn loads none and allows none.
func TestClaudeOwnServersArgs(t *testing.T) {
	own := []string{"dune", "posthog", "my.server"}
	for _, readOnly := range []bool{false, true} {
		for _, tools := range []string{"", toolsURL} {
			turn := Turn{Tools: tools, OwnServers: own, Prompt: "hi"}
			turn.Writes.ReadOnly = readOnly
			a := claude{}.Args(turn)
			allowed := strings.Split(flagValue(a, "--allowedTools"), ",")
			for _, r := range []string{"mcp__dune", "mcp__posthog"} {
				if slices.Contains(allowed, r) == readOnly {
					t.Errorf("read_only %v tools %q: %s allowed %v: %q", readOnly, tools, r, !readOnly, a)
				}
			}
			if strings.Contains(strings.Join(a, " "), "my.server") {
				t.Errorf("read_only %v tools %q: rule for a rewritten name: %q", readOnly, tools, a)
			}
			if slices.Contains(a, "--strict-mcp-config") != readOnly || !slices.Contains(a, "--no-chrome") {
				t.Errorf("read_only %v tools %q: %q", readOnly, tools, a)
			}
		}
	}
	// The tool server's own rule is not given twice.
	a := claude{}.Args(Turn{Tools: toolsURL, OwnServers: []string{ToolServer}, Prompt: "hi"})
	if c := strings.Count(flagValue(a, "--allowedTools"), "mcp__agentbus"); c != 1 {
		t.Errorf("mcp__agentbus %d times: %q", c, a)
	}
}

// codex reads the server names of $CODEX_HOME/config.toml, else
// $HOME/.codex/config.toml; names only, sorted.
func TestCodexOwnServers(t *testing.T) {
	home := t.TempDir()
	conf := filepath.Join(home, ".codex")
	_ = os.MkdirAll(conf, 0o700)
	_ = os.WriteFile(filepath.Join(conf, "config.toml"), []byte("model = \"x\"\n[mcp_servers.linear]\nurl = \"http://l\"\n[mcp_servers.dune]\ncommand = \"d\"\n[mcp_servers.dune.env]\nK = \"v\"\n"), 0o600)
	for _, env := range [][]string{{"CODEX_HOME=" + conf}, {"HOME=" + home}} {
		if got := (codex{}).OwnServers(env); !slices.Equal(got, []string{"dune", "linear"}) {
			t.Errorf("%v: %v", env, got)
		}
	}
	if got := (codex{}).OwnServers([]string{"CODEX_HOME=" + t.TempDir()}); len(got) != 0 {
		t.Errorf("no file: %v", got)
	}
}

// A read-only codex turn turns each of the person's servers off for the run,
// before resume; an edit turn leaves them as codex loads them. A name that
// cannot go in a -c key refuses a read-only turn only.
func TestCodexOwnServersArgs(t *testing.T) {
	own := []string{"dune", "linear"}
	for _, readOnly := range []bool{false, true} {
		for _, resume := range []string{"", "r-1"} {
			turn := Turn{OwnServers: own, ResumeID: resume, Prompt: "hi"}
			turn.Writes.ReadOnly = readOnly
			a := codex{}.Args(turn)
			end := slices.Index(a, "--")
			if r := slices.Index(a, "resume"); r >= 0 {
				end = r
			}
			for _, n := range own {
				i := slices.Index(a, "mcp_servers."+n+".enabled=false")
				if (i >= 0) != readOnly || (i >= 0 && (a[i-1] != "-c" || i > end)) {
					t.Errorf("read_only %v resume %q: %s: %q", readOnly, resume, n, a)
				}
			}
			if why := (codex{}).Refuse(turn); why != "" {
				t.Errorf("read_only %v: refused: %q", readOnly, why)
			}
		}
	}
	bad := Turn{OwnServers: []string{"ok", "my.server"}, Prompt: "hi"}
	if why := (codex{}).Refuse(bad); why != "" {
		t.Errorf("edit turn refused: %q", why)
	}
	bad.Writes.ReadOnly = true
	if why := (codex{}).Refuse(bad); why == "" {
		t.Errorf("read-only turn with an unsafe name not refused")
	}
}

// agy turns allow no MCP call at all, read-only or not: --settings replaces
// the person's permissions for the run, and headless agy refuses any call no
// rule allows (measured 2026-11-14, research/2026-11-14-use-my-own-tools-design.md).
func TestAgyNoMCPRule(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		turn := Turn{OwnServers: []string{"dune"}, Prompt: "hi"}
		turn.Writes.ReadOnly = readOnly
		if s := agySettings(turn); strings.Contains(s, "mcp") {
			t.Errorf("read_only %v: %s", readOnly, s)
		}
	}
}

// Every harness denies, with the tool server, the tools its Info says it
// hides (control names them in the turn's tools.listed), and keeps them
// without it.
func TestToolsHiddenAreDenied(t *testing.T) {
	for _, h := range All() {
		info := h.Info()
		if len(info.ToolsHidden) == 0 {
			continue
		}
		if !info.ToolServer {
			t.Errorf("%s hides tools but takes no tool server", info.Name)
		}
		with := strings.Join(h.Args(Turn{Tools: toolsURL, Prompt: "hi"}), " ")
		without := strings.Join(h.Args(Turn{Prompt: "hi"}), " ")
		for _, n := range info.ToolsHidden {
			if !strings.Contains(with, n) {
				t.Errorf("%s: %s hidden but not denied: %s", info.Name, n, with)
			}
			if strings.Contains(without, n) {
				t.Errorf("%s: %s denied without the tool server: %s", info.Name, n, without)
			}
		}
	}
	if got := (claude{}).Info().ToolsHidden; !slices.Equal(got, []string{"Task", "Agent", "ListAgents", "SendMessage"}) {
		t.Errorf("claude hides %v", got)
	}
	if got := (codex{}).Info().ToolsHidden; len(got) != 0 {
		t.Errorf("codex hides %v", got)
	}
}

// Every codex turn, read-only or not, with or without the tool server and on
// resume, runs with the person's ChatGPT apps and plugins off, before resume
// so the resumed turn gets them too.
func TestCodexOwnToolsOff(t *testing.T) {
	for _, tools := range []string{"", toolsURL} {
		for _, readOnly := range []bool{false, true} {
			for _, resume := range []string{"", "r-1"} {
				turn := Turn{Tools: tools, ResumeID: resume, Prompt: "hi"}
				turn.Writes.ReadOnly = readOnly
				a := codex{}.Args(turn)
				all := " " + strings.Join(a, " ") + " "
				end := strings.Index(all, " -- ")
				if r := strings.Index(all, " resume "); r >= 0 {
					end = r
				}
				d := strings.Index(all, " --disable apps --disable plugins ")
				if d < 0 || d > end {
					t.Errorf("tools %q read_only %v resume %q: %q", tools, readOnly, resume, a)
				}
			}
		}
	}
}
