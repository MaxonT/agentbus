package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claude is Claude Code.
type claude struct{}

func (claude) Info() Info {
	return Info{
		Name:    "claude",
		Label:   "Claude Code",
		Product: "Claude Code",
		// Answers with network access denied (Claude Code 2.1.318,
		// 2026-11-05, run under a sandbox profile that denies the network).
		StatusArgs:         []string{"auth", "status"},
		OwnEndpointEnv:     "ANTHROPIC_BASE_URL",
		Access:             standardAccess("Claude subscription", []string{"sonnet", "opus", "haiku"}, "Anthropic API key"),
		Replay:             FormatAnthropic,
		ReplayOnOpenRouter: true,
		// Claude Code keeps its settings, memory, plugins, transcripts and
		// .claude.json under CLAUDE_CONFIG_DIR when it is set.
		ConfigEnv: "CLAUDE_CONFIG_DIR", ConfigDir: ".claude",
		ToolServer: true,
		// With the tool server, Claude Code's own agents are hidden, so
		// AgentBus agents are the only agents the turn sees (his call,
		// 2026-11-10). Claude Code 2.1.321 (2026-11-11, read from the tool
		// list a haiku -p turn prints): Task starts a subagent (Agent in
		// newer versions), ListAgents lists other Claude sessions on the
		// machine and SendMessage writes to them. A turn asked "list other
		// agents" used ListAgents and answered there were none (2026-11-10).
		ToolsHidden: slices.Clone(claudeHidden),
		// claude setup-token prints a long-lived OAuth token "for CI
		// pipelines, scripts, or other environments", valid one year, which
		// Claude Code reads from CLAUDE_CODE_OAUTH_TOKEN
		// (code.claude.com/docs/en/authentication, read 2026-11-08). The
		// token's shape is not yet checked against a live login: the
		// person's first connect is that check.
		//
		// A wrong code (2.1.322, 2026-11-10, in a terminal) prints "OAuth
		// error: That code wasn't accepted. ..." and "Press Enter to
		// retry.", and waits: it does not exit.
		Login: &Login{Args: []string{"setup-token"}, Prefix: "sk-ant-oat", Token: `sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{20,}`,
			ValidDays: 365, Refused: `OAuth\s*error`},
		// Anthropic's API takes a key in x-api-key
		// (docs.anthropic.com/en/api/overview, read 2026-11-13).
		APIKey: &APIKey{Name: "anthropic", Label: "Anthropic API key", URL: "https://api.anthropic.com", Header: "X-Api-Key", Format: FormatAnthropic},
	}
}

// Wire: Claude Code reads ANTHROPIC_BASE_URL (2.1.318, 2026-11-05).
func (claude) Wire(baseURL string) (env, args []string) {
	return []string{"ANTHROPIC_BASE_URL=" + baseURL}, nil
}

// WireRelay: as Wire, with the placeholder in CLAUDE_CODE_OAUTH_TOKEN, where
// Claude Code reads a setup-token's token from
// (code.claude.com/docs/en/authentication, read 2026-11-08).
func (c claude) WireRelay(baseURL, placeholder string) (env, args []string) {
	env, args = c.Wire(baseURL)
	return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+placeholder), args
}

// WireOpenRouter: ANTHROPIC_BASE_URL at the service, the key in
// ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY empty (OpenRouter's Claude Code
// guide says it must be), every model alias set to the turn's model so a
// subagent or a small fast call uses it too, and no experimental betas, which
// a non-Anthropic upstream refuses.
func (claude) WireOpenRouter(w OpenRouterWire) (env, args []string) {
	env = []string{
		"ANTHROPIC_BASE_URL=" + w.Base,
		"ANTHROPIC_AUTH_TOKEN=" + w.Key,
		"ANTHROPIC_API_KEY=",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1",
	}
	if w.Model != "" {
		for _, v := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
			env = append(env, v+"="+w.Model)
		}
	}
	return env, nil
}

// WireAPI: ANTHROPIC_BASE_URL at the service, the placeholder in
// ANTHROPIC_AUTH_TOKEN, which Claude Code sends as "Authorization: Bearer"
// (as for OpenRouter), and ANTHROPIC_API_KEY empty, so no key of the
// machine's is sent. Control's relay puts the key set in Settings in
// x-api-key in its place. Claude Code names Anthropic's own models, so no
// alias is set: --model is enough.
func (claude) WireAPI(w APIWire) (env, args []string) {
	return []string{
		"ANTHROPIC_BASE_URL=" + w.Base,
		"ANTHROPIC_AUTH_TOKEN=" + w.Placeholder,
		"ANTHROPIC_API_KEY=",
	}, nil
}

// Edit: Claude Code's rules hold paths; a name that cannot go in one gets no
// edit rule, and a protected file that cannot be denied leaves edits out.
func (claude) Edit(folders, protect []string) ([]string, string) {
	for _, f := range folders {
		if !ruleSafe(f) {
			return nil, "folder_shape"
		}
	}
	for _, p := range protect {
		if !ruleSafe(p) {
			return nil, "protected_file_shape"
		}
	}
	return folders, ""
}

// OwnInstructions: --append-system-prompt adds to the person's own system
// prompt, never replaces it, so the hint always goes in.
// Refuse: every setting of permissions.toml has a flag on claude, or the
// gateway's guard.
func (claude) Refuse(Turn) string { return "" }

func (claude) OwnInstructions([]string, string) string { return "" }

// OwnTools says the person's own MCP servers (OwnServers) name one as the
// tool server, so the turn gets no tool server and its instructions name
// agentbus-api: their entry wins, as on codex. An edit turn loads their
// servers beside the --mcp-config one, and two of one name would leave which
// one Claude Code keeps to it.
func (c claude) OwnTools(env []string, _ string) string {
	if slices.Contains(c.OwnServers(env), ToolServer) {
		return "claude_mcp_server_set"
	}
	return ""
}

// OwnServers are the names in the mcpServers of the .claude.json Claude Code
// reads: $CLAUDE_CONFIG_DIR/.claude.json, which a session's home sets (Home
// files merges ~/.claude.json's mcpServers there), else $HOME/.claude.json
// (Claude Code 2.1.323). Names only: a server's settings are never kept. A
// missing file, or one that does not parse, names none: Claude Code then
// loads none of it either.
func (claude) OwnServers(env []string) []string {
	path := ""
	if d := envValue(env, "CLAUDE_CONFIG_DIR"); d != "" {
		path = filepath.Join(d, ".claude.json")
	} else if h := envValue(env, "HOME"); h != "" {
		path = filepath.Join(h, ".claude.json")
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	names := make([]string, 0, len(v.MCPServers))
	for n := range v.MCPServers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Native: Claude Code reads the folder's CLAUDE.md and .claude/CLAUDE.md,
// and a file such a CLAUDE.md imports with a line "@<file>". With neither
// CLAUDE.md, it reads the folder's AGENTS.md instead; with one, it does not
// (Claude Code 2.1.318 and 2.1.321, 2026-11-10, checked live: a folder with
// AGENTS.md alone, with a CLAUDE.md beside it, with .claude/CLAUDE.md beside
// it, and with a CLAUDE.md that imports @AGENTS.md). So it reads name when no
// CLAUDE.md is there and name is AGENTS.md, when a CLAUDE.md is name itself
// (a symlink to it), or when the folder's CLAUDE.md imports it by a line
// "@name" or "@./name". Any other way of pointing at it is not seen, and the
// file is then given again.
func (claude) Native(folder *os.Root, name string) bool {
	want, err := folder.Stat(name)
	if err != nil {
		return false
	}
	own := false
	for _, f := range []string{"CLAUDE.md", ".claude/CLAUDE.md"} {
		if _, err := folder.Lstat(f); err != nil {
			continue
		}
		own = true
		if fi, err := folder.Stat(f); err == nil && os.SameFile(fi, want) {
			return true
		}
	}
	if !own {
		return name == "AGENTS.md"
	}
	f, err := OpenFile(folder, "CLAUDE.md")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l == "@"+name || l == "@./"+name {
			return true
		}
	}
	return false
}

// ResumeID is the session_id of the "type":"result" object that
// --output-format json prints.
func (claude) ResumeID(line []byte, current string) string {
	if v, ok := decodeLine(line); ok && v.Type == "result" && resumeIDShape.MatchString(v.SessionID) {
		return v.SessionID
	}
	return current
}

func (claude) Args(t Turn) []string {
	// --output-format json prints one result object with the session_id.
	a := []string{"-p", "--output-format", "json"}
	// The hint goes at the end of Claude Code's system prompt. Own replay
	// keeps each request's system prompt as the agent sent it, and the
	// text is the same on every turn of the session, so the main thread
	// is still recognised and the prompt cache still holds.
	if t.Hint != "" {
		a = append(a, "--append-system-prompt", t.Hint)
	}
	if t.ResumeID != "" {
		a = append(a, "--resume", t.ResumeID)
	}
	// One --allowedTools, comma-joined (Claude Code 2.1.318,
	// 2026-11-05): what the tools may do without asking.
	allowed := readRules(t.ReadFolders)
	// edit: Edit(<folder>/**) lets Edit and Write change files there
	// without asking; one Edit rule covers every built-in tool that
	// edits files. Without it a -p turn cannot edit at all: nobody
	// is there to approve.
	allowed = append(allowed, editRules(t.Writes.Edit)...)
	// The AgentBus gateway decides every shell command of the turn — allow,
	// ask, forbid, and a command on no list — acting on the model's reply
	// before the agent runs the call (gateway/guard.go). So the whole Bash
	// tool is allowed here and the gateway is the gate: Claude Code's own
	// literal Bash check must not refuse a command first. That check split
	// a chain at ";" and did not match an env prefix, so it refused an
	// allowed command with nowhere to approve it (his finding, turn
	// t_f088dacca881acf7, 2026-11-16). The person's config is never edited;
	// this is a per-turn flag. File edit rules above are unchanged.
	allowed = append(allowed, "Bash")
	allowed = append(allowed, claudeTools(t.Tools)...)
	// The person's own MCP servers load (no --strict-mcp-config), and
	// a -p turn has nobody to approve a call: mcp__<server> lets every
	// tool of each run without asking, as for the tool server. A name
	// of another shape gets no rule: Claude Code rewrites it in its
	// tool names, so a rule could miss or widen; its tools then run
	// only as the person's own settings allow.
	for _, n := range t.OwnServers {
		if r := "mcp__" + n; serverNameShape.MatchString(n) && !slices.Contains(allowed, r) {
			allowed = append(allowed, r)
		}
	}
	if len(allowed) > 0 {
		a = append(a, "--allowedTools", strings.Join(allowed, ","))
	}
	// The tool server comes in by --mcp-config on every turn that has it.
	if t.Tools != "" {
		a = append(a, "--mcp-config", claudeMCPConfig(t.Tools))
	}
	// A review is a normal turn now (read-only retired, B12): it loads the
	// person's own servers like any turn, in a session's home from Home
	// files copied in (his decision, 2026-11-14). Calls are recorded as
	// tool.requested under their mcp__ names.
	// --no-chrome: Claude in Chrome is not an MCP server of the person's
	// settings, so --strict-mcp-config leaves it on (measured 2026-11-14,
	// research/2026-11-14-use-my-own-tools-design.md), and it cannot work
	// with a sign-in AgentBus holds. Off on every turn.
	a = append(a, "--no-chrome")
	// forbid is the gateway's: it renames a forbidden call in the reply before
	// the agent runs it, for claude and codex alike, in the shapes a
	// Bash(<cmd>:*) rule misses — git -C repo push, env prefixes, chains
	// (gateway/guard.go). No Bash deny here: with the whole Bash tool allowed
	// above, the agent decides nothing about shell; the one gate does. The data
	// folder's own files are still never edited: a deny on each, which wins over
	// the edit rule and the person's settings alike.
	var denied []string
	denied = append(denied, protectRules(t.Writes.Protect)...)
	// No edit rule: a deny on every tool that edits files, so the
	// person's own settings (an acceptEdits mode, an Edit allow
	// rule) cannot approve one either. A deny wins over any allow.
	if len(t.Writes.Edit) == 0 {
		denied = append(denied, "Edit", "Write", "NotebookEdit")
	}
	// subagents = false: a deny on each tool that starts a subagent. Task
	// is its name in Claude Code 2.1.319 (2026-11-09, read from the tool
	// list a haiku turn prints, and gone from it with this deny); Agent is
	// its name in newer versions. Workflow runs scripts that start
	// subagents.
	if t.NoSubagents {
		denied = append(denied, "Task", "Agent", "Workflow")
	}
	// With the tool server, the tools AgentBus tools replace are denied
	// (Info.ToolsHidden).
	if t.Tools != "" {
		for _, n := range claudeHidden {
			if !slices.Contains(denied, n) {
				denied = append(denied, n)
			}
		}
	}
	if len(denied) > 0 {
		a = append(a, "--disallowedTools", strings.Join(denied, ","))
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	return append(a, "--", t.Prompt)
}

// claudeHidden is claude's Info.ToolsHidden.
var claudeHidden = []string{"Task", "Agent", "ListAgents", "SendMessage"}

// claudeMCPConfig is the --mcp-config value of a turn with the tool server at
// url: inline JSON, so nothing is written. Claude Code expands
// ${AGENTBUS_TOKEN} from its own environment when it loads the server
// (2.1.321, 2026-11-11, checked live), so the token is in no argument.
func claudeMCPConfig(url string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{ToolServer: map[string]any{
		"type": "http", "url": url,
		"headers": map[string]string{"Authorization": "Bearer ${AGENTBUS_TOKEN}"}}}})
	return string(b)
}

// claudeTools is the --allowedTools rule that lets the tool server's tools run
// without asking: a -p turn has nobody to ask. What a call may do is the
// turn token's to say.
func claudeTools(url string) []string {
	if url == "" {
		return nil
	}
	return []string{"mcp__" + ToolServer}
}

// readRules is Claude Code's rules that let Read, Grep and Glob reach each
// folder and everything under it without asking: Read(//<folder>/**), with
// "//" marking an absolute path. They grant reading only. --add-dir is not
// used: it makes the folder a working folder, which under the person's
// acceptEdits mode lets a turn write there too (Claude Code 2.1.319,
// 2026-11-07, checked live, found in review). A Read rule alone does not
// cover Grep, so each tool gets its own.
func readRules(folders []string) []string {
	var rules []string
	for _, f := range folders {
		p := "/" + strings.TrimSuffix(f, "/") + "/**"
		rules = append(rules, "Read("+p+")", "Grep("+p+")", "Glob("+p+")")
	}
	return rules
}

// ruleSafe says p can go inside a Claude Code rule as it is: a comma or a
// parenthesis would end the rule, and a glob character would widen it.
func ruleSafe(p string) bool {
	return !strings.ContainsAny(p, ",()*?[]{}") && !strings.ContainsFunc(p, isControl)
}

// editRules is Claude Code's rule that lets the tools that edit files change
// anything under each folder without asking: Edit(//<folder>/**).
func editRules(folders []string) []string {
	var rules []string
	for _, f := range folders {
		rules = append(rules, "Edit(/"+strings.TrimSuffix(f, "/")+"/**)")
	}
	return rules
}

// protectRules is the deny rule for each protected file: Edit(//<file>).
func protectRules(files []string) []string {
	var rules []string
	for _, f := range files {
		rules = append(rules, "Edit(/"+f+")")
	}
	return rules
}
