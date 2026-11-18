package gateway

import "testing"

// The hold lifecycle is the single source of truth for a held reply, with its
// transitions in one place. The declared path is none -> pending -> active ->
// done, and the properties the earlier hold needed four separate flags for fall
// out of it: finish is a single winner, a never-held reply cannot finish (so
// closing an inline refusal records nothing), and no move skips or reverses.
func TestHoldLifecycle(t *testing.T) {
	var h holdLifecycle
	if h.state() != holdNone {
		t.Fatalf("a fresh hold is %d, want holdNone", h.state())
	}
	h.begin()
	if h.state() != holdPending {
		t.Fatal("begin did not move none -> pending")
	}
	h.begin() // a second Ask call of the same reply: idempotent
	if h.state() != holdPending {
		t.Fatal("a second begin moved past pending")
	}
	h.activate()
	if h.state() != holdActive {
		t.Fatal("activate did not move pending -> active")
	}
	if !h.finish() {
		t.Fatal("finish did not finalise an active hold")
	}
	if h.state() != holdDone {
		t.Fatal("finish did not move active -> done")
	}

	// finish is a single winner: the first of {a decision, the timeout, a closed
	// connection} wins; a later caller finds it already done. This is the rule
	// the earlier hold broke when a second answer released an unapproved call.
	if h.finish() {
		t.Fatal("a second caller won a done hold")
	}

	// A never-held (holdNone) reply cannot finish, so closing an inline refusal
	// records no decision.
	var none holdLifecycle
	if none.finish() {
		t.Fatal("a never-held reply finished")
	}

	// A pending hold with nothing clean to wait on finishes once (startHold's
	// inline-refuse path), and only once.
	var p holdLifecycle
	p.begin()
	if !p.finish() {
		t.Fatal("a pending hold could not finish")
	}
	if p.finish() {
		t.Fatal("a done hold finished twice")
	}

	// No move skips pending or runs backwards: activate from none, or any move
	// out of done, is rejected rather than corrupting the state.
	var skip holdLifecycle
	skip.activate()
	if skip.state() != holdNone {
		t.Fatal("activate skipped pending from none")
	}
}

// The hold timing falls back to the measured defaults, and SetHoldTiming
// overrides only the fields it is given.
func TestHoldTimingDefaultsAndOverride(t *testing.T) {
	var g Gateway
	if g.holdTimeoutDur() != HoldTimeout || g.pingDur() != PingInterval || g.pollDur() != PollInterval {
		t.Fatal("a zero gateway does not fall back to the default hold timing")
	}
	g.SetHoldTiming(0, 0, 0) // zero keeps the defaults
	if g.holdTimeoutDur() != HoldTimeout || g.pingDur() != PingInterval {
		t.Fatal("zero changed a default")
	}
	g.SetHoldTiming(30, 7, 3)
	if g.holdTimeoutDur() != 30 || g.pingDur() != 7 || g.pollDur() != 3 {
		t.Fatalf("override not applied: %v %v %v", g.holdTimeoutDur(), g.pingDur(), g.pollDur())
	}
}
