package gateway

import "strings"

// B6: the gate's second checkpoint. The command gate (command.go) decides a
// shell command the model wrote; DecideTool decides a tool call the agent made
// to the AgentBus tool server (api/mcp.go). Both are the ONE decision — his
// call, 2026-11-18: one rules list and one decision function, no second copy,
// so a rule gives the same answer at both checkpoints.
//
// A tool name is one word, so DecideTool matches it through the SAME matchAny
// the command gate uses (decideWords): a forbid, ask or allow entry of
// permissions.toml decides a tool call and a shell command alike. His decision,
// 2026-11-18 (DESIGN_0_6_tools.md #9): a tool is decided by NAME alone, against
// the same rules; nothing about the tool is trusted, so there is no effect and
// no per-tool default. A tool on no list is held for a person (Ask); AgentBus
// ships its read-only built-ins as ordinary allow rules, so they match and run.

// DecideTool decides one tool call by the person's rules and the tool's name.
// A forbid, ask or allow entry naming the tool wins; with no rule the call is
// held for a person (Ask), never allowed silently. rule is the ask or forbid
// entry that drove an Ask or a Refuse, "" when no rule matched.
func DecideTool(r Rules, name string) (Decision, string) {
	// The SAME core the command gate uses, with the no-rule default fixed at Ask
	// for a tool. A tool name has no spaces, so it is one word and matchWords
	// compares it whole (by base name) against each rule's words: an exact-name
	// match, never a command's prefix match by accident.
	rr := r
	rr.Unlisted = Ask
	return decideWords(rr, []string{name}, false)
}

// MatchingRules are every entry of list the tool name matches, by the SAME
// matcher the gate decides with (matchWords on the one-word name). "Always
// allow this tool" moves all of them from ask to allow, so an ask entry in any
// form — a bare name, a full path, several equivalents — is cleared, not just
// the first; otherwise one left behind keeps matching and the next call asks
// again. The order is list's.
func MatchingRules(list []string, name string) []string {
	var out []string
	for _, r := range list {
		if matchWords(strings.Fields(r), []string{name}) {
			out = append(out, r)
		}
	}
	return out
}
