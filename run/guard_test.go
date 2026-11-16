package run

import (
	"slices"
	"testing"

	"github.com/staysup/agentbus/gateway"
)

func TestTurnRules(t *testing.T) {
	e := &Engine{}
	// t_1: forbid git push, allow ls, allow git fetch once, refuse unlisted.
	e.turnRules.set("s1", "t_1", []string{"ls"}, nil, []string{"git push"}, []string{"git fetch origin"}, true)
	// t_2: every list empty, the ask default for a command on no list.
	e.turnRules.set("s1", "t_2", nil, nil, nil, nil, false)
	if got := e.Forbid("s1", "t_1"); !slices.Equal(got, []string{"git push"}) {
		t.Errorf("Forbid = %q", got)
	}
	if got := e.Allow("s1", "t_1"); !slices.Equal(got, []string{"ls"}) {
		t.Errorf("Allow = %q", got)
	}
	if got := e.AllowOnce("s1", "t_1"); !slices.Equal(got, []string{"git fetch origin"}) {
		t.Errorf("AllowOnce = %q", got)
	}
	if got := e.Unlisted("s1", "t_1"); got != gateway.Refuse {
		t.Errorf("Unlisted t_1 = %v, want Refuse", got)
	}
	// A live turn with no refuse flag asks for a command on no list.
	if got := e.Unlisted("s1", "t_2"); got != gateway.Ask {
		t.Errorf("Unlisted t_2 = %v, want Ask", got)
	}
	if got := e.Forbid("s1", "t_2"); got != nil {
		t.Errorf("a turn without forbid got %q", got)
	}
	// A turn that is not live is not guarded: a command on no list runs.
	if got := e.Unlisted("s1", "gone"); got != gateway.Allow {
		t.Errorf("Unlisted of an unknown turn = %v, want Allow", got)
	}
	e.turnRules.clear("s1", "t_1")
	if got := e.Forbid("s1", "t_1"); got != nil {
		t.Errorf("after clear: %q", got)
	}
	if got := e.Unlisted("s1", "t_1"); got != gateway.Allow {
		t.Errorf("after clear, Unlisted = %v, want Allow", got)
	}
}
