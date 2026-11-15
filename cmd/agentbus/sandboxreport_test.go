package main

import (
	"testing"

	engine "github.com/staysup/agentbus/run"
)

// A process whose sessions get no home of their own (a runner with
// -gateway local) reports it cannot sandbox, with the reason a walled turn
// is refused with, so Settings can grey the switch out. What a runner with
// homes, and agentbus up, report is the machine's: that half starts a
// child, so it is in sandboxreport_integration_test.go.
func TestSandboxReportNeedsHomes(t *testing.T) {
	c := config{mode: modeRunner, gatewayMode: gatewayLocal}
	if can, why := c.canSandbox(); can || why != engine.NoHomes {
		t.Errorf("mode %q gateway %q: can %v, %q", c.mode, c.gatewayMode, can, why)
	}
}
