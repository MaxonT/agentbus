package settings

// The instructions are the text every turn gets as an instruction of its
// agent's own: Claude Code's --append-system-prompt and codex's
// developer_instructions, never inside the prompt. The file store keeps them
// in instructions.md. {session} in them becomes the turn's session id, and
// {tools} how the turn calls AgentBus: ToolsText when it has the agentbus
// MCP tools, else APIText (package run), each saying whether the turn may
// apply as [agent_api] apply does. Keep the text the same across a
// session's turns: own replay and the vendor's prompt cache rely on it.

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxInstructions bounds the text, in bytes: it goes on the agent's command
// line.
const MaxInstructions = 64 << 10

// DefaultInstructions is the text of a missing file, and what the file store
// writes when the file is missing. An empty file gives no instructions.
const DefaultInstructions = `You work through AgentBus, in session {session}. A turn is one message to you, your reply and your changes.
Your folder is usually your session's own copy of the person's folder. Your edits stay in the copy. They reach the person's folder only when a turn is applied.
{tools}
`

// ToolsText is what {tools} becomes for a turn with the agentbus MCP tools.
// B12 retired read-only turns and the read-tool copies, so the tools now cover
// asking other agents (ask_agent); reading turns and diffs, searching, review,
// apply and stop go through agentbus-api. apply is [agent_api] apply: whether
// the turn may apply turns.
func ToolsText(apply bool) string {
	write := `To read turns and their diffs, search this session, or review or stop a turn, use agentbus-api (GET /api/v0/turns, GET /api/v0/turns/T/diff, POST /api/v0/turns/T/review, POST /api/v0/turns/T/stop).
The person applies turns to their folder; you cannot.`
	if apply {
		write = `To read turns and their diffs, search this session, or review, apply or stop a turn, use agentbus-api (GET /api/v0/turns, GET /api/v0/turns/T/diff, POST /api/v0/turns/T/review, POST /api/v0/turns/T/apply, POST /api/v0/turns/T/stop).`
	}
	return `Use the agentbus MCP tools to ask other saved agents (any vendor) for work. AgentBus agents are saved setups across vendors, not your harness's own agents or subagents.
` + write + `
Work you ask for comes back to you as a later turn: do not wait for it.`
}

// APIText is what {tools} becomes for a turn without them. apply is as for
// ToolsText.
func APIText(apply bool) string {
	line := "- The person applies turns to their folder; you cannot.\n"
	if apply {
		line = "- Apply a turn to the person's folder (its changes and the earlier turns' that are not applied yet): agentbus-api POST /api/v0/turns/T/apply\n"
	}
	return `Call AgentBus with agentbus-api METHOD PATH [JSON]. Replace T with a turn id.
- List this session's turns, with each id, number, state and cost: agentbus-api GET /api/v0/turns
` + line + `- Ask another agent to review a turn: agentbus-api POST /api/v0/turns/T/review '{"agent":"<agent id>"}'. Pick an agent other than the one that did the turn. List the agents: agentbus-api GET /api/v0/agents. These are AgentBus agents, not your harness's own agents or subagents.
- Read a turn, its state, cost and a review's verdict: agentbus-api GET /api/v0/turns/T
- Stop a turn: agentbus-api POST /api/v0/turns/T/stop
- Search this session's history: agentbus-api GET "/api/v0/sessions/{session}/search?q=<text>", with <text> URL-encoded.
Work you ask for comes back to you as a later turn: do not wait for it. More: agentbus-api GET /docs/llms.txt`
}

// ToolServerInstructions is the tool server's own instructions (POST
// /api/v0/mcp, initialize), which a harness may show its model. apply says
// whether the caller may apply turns ([agent_api] apply). B12 removed the read
// tool copies too (read-only turns retired), so reading the record, reviewing,
// applying and stopping all go through agentbus-api; the tools are the apps,
// such as ask_agent.
func ToolServerInstructions(apply bool) string {
	write := "To read turns, search history, review or stop a turn, use agentbus-api. The person applies turns to their folder."
	if apply {
		write = "To read turns, search history, review, apply or stop a turn, use agentbus-api."
	}
	return "AgentBus tools: ask other saved agents (any vendor) for work. " +
		"Your folder is your session's own copy. " + write + " Work you ask for comes back to your session as a later turn."
}

// CheckInstructions refuses text a turn cannot carry.
func CheckInstructions(b []byte) error {
	switch {
	case len(b) > MaxInstructions:
		return errors.New("it is over 64 KiB")
	case !utf8.Valid(b):
		return errors.New("it is not UTF-8 text")
	case strings.ContainsRune(string(b), 0):
		return errors.New("it holds a NUL byte")
	}
	return nil
}
