//go:build integration

package main

import (
	"testing"

	"github.com/staysup/agentbus/sandbox"
)

// A runner with homes reports what the machine can (0.5.1), and so does
// agentbus up, whose sessions have homes since its credentials come from
// control too. sandbox.Can starts a child, so this runs in make gate only.
func TestSandboxReportIsTheMachines(t *testing.T) {
	wantCan, wantWhy := sandbox.Can()
	for _, c := range []config{{mode: modeRunner}, {mode: modeUp}} {
		if can, why := c.canSandbox(); can != wantCan || why != wantWhy {
			t.Errorf("mode %q: can %v %q, want the machine's %v %q", c.mode, can, why, wantCan, wantWhy)
		}
	}
}
