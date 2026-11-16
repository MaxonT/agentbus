package gateway

// Ask: a tool call whose command matches an ask entry of permissions.toml is
// refused by the gateway, like a forbid entry (rename + a refusal in the tool
// result), with a message that a person must approve it, and recorded as an
// approval.requested event (guard.go registerApproval). The turn ends normally;
// nothing is held — no connection held open, no pings, no time limit, no
// goroutine.
//
// The gateway keeps no approval state. Control derives the pending approvals
// from the record (an approval.requested with no approval.decided for the same
// id) and writes approval.decided when a person answers: an allow moves the
// matched ask entries to allow and continues the session so the agent can run
// them; a refusal records the decision. So a refusal on a separate runner's
// gateway is listed and answered on control, which holds the record.
//
// This replaced a design that held the model-call connection open until a
// person answered. Four review rounds found life-cycle defects that all came
// from holding the connection (disconnect not seen, events after settlement,
// streamed vs unstreamed drift). Refusing and recording removes the whole
// class: the refusal path is the forbid path, already reviewed sound.

import "strconv"

// askRefusal is what the model is told in place of an ask command's output.
func askRefusal(command string) string {
	return "Held by the AgentBus gateway: waiting for a person to approve " + strconv.Quote(command) +
		". The command did not run. A person may allow it, which continues this session so you can run it."
}
