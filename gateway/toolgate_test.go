package gateway

import "testing"

// A rule gives the SAME answer at both checkpoints (his decision, 2026-11-18:
// one decision function, no second copy of the logic). The command gate
// classifies a name as a one-command line; the tool gate decides it as a tool
// call. For every name that matches a forbid, ask or allow entry, the two agree
// — the rule, not the no-rule default, decides, so the effect default passed to
// DecideTool cannot change the answer.
func TestToolAndCommandAgreeOnARule(t *testing.T) {
	rules := Rules{
		Forbid:   []string{"apply_turn"},
		Ask:      []string{"review_turn"},
		Allow:    []string{"list_turns"},
		Unlisted: Ask,
	}
	for _, name := range []string{"apply_turn", "review_turn", "list_turns"} {
		cmdDec, _ := classify(rules, name)
		toolDec, _ := DecideTool(rules, name)
		if toolDec != cmdDec {
			t.Errorf("%s: command gate %v, tool gate %v; a rule must decide both the same", name, cmdDec, toolDec)
		}
	}
}

// With no rule for the tool, the call is held (Ask); nothing about the tool
// grants it. A named rule overrides: a forbid refuses it, an allow runs it.
func TestDecideToolDefaultsAndOverrides(t *testing.T) {
	none := Rules{}
	if d, _ := DecideTool(none, "get_turn"); d != Ask {
		t.Errorf("a tool with no rule: %v, want Ask", d)
	}
	if d, _ := DecideTool(none, "ask_agent"); d != Ask {
		t.Errorf("a tool with no rule: %v, want Ask", d)
	}
	// AgentBus ships its read-only built-ins as allow rules: an allowed tool runs.
	allow := Rules{Allow: []string{"get_turn"}}
	if d, _ := DecideTool(allow, "get_turn"); d != Allow {
		t.Errorf("an allowed tool: %v, want Allow", d)
	}
	// A forbid entry refuses it.
	forbid := Rules{Forbid: []string{"ask_agent"}}
	if d, rule := DecideTool(forbid, "ask_agent"); d != Refuse || rule != "ask_agent" {
		t.Errorf("a forbidden tool: %v %q, want Refuse ask_agent", d, rule)
	}
}

// A command rule of several words never matches a one-word tool name, and a
// tool rule never swallows a different tool (names are compared whole).
func TestDecideToolDoesNotCrossMatch(t *testing.T) {
	rules := Rules{Forbid: []string{"git push"}, Ask: []string{"apply_turn"}, Allow: []string{"list_turns"}}
	if d, _ := DecideTool(rules, "apply_turn"); d != Ask {
		t.Errorf("apply_turn: %v, want Ask", d)
	}
	// "git push" (two words) cannot match the one-word name "git": no rule, so Ask.
	if d, _ := DecideTool(rules, "git"); d != Ask {
		t.Errorf("git (no rule): %v, want Ask", d)
	}
	// An allowed tool runs.
	if d, _ := DecideTool(rules, "list_turns"); d != Allow {
		t.Errorf("list_turns: %v, want Allow", d)
	}
}
