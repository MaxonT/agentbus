package gateway

// Ask: a tool call whose command matches an ask entry of permissions.toml is
// refused by the gateway, like a forbid entry (rename + a refusal in the tool
// result), with a message that a person must approve it, and recorded as a
// pending approval. The turn ends normally; nothing is held — no connection
// held open, no pings, no time limit, no goroutine. A person answers through
// the API: an allow starts a fresh turn in the same session that may run the
// command; a refusal records the decision.
//
// This replaced a design that held the model-call connection open until a
// person answered. Four review rounds found life-cycle defects that all came
// from holding the connection (disconnect not seen, events after settlement,
// streamed vs unstreamed drift). Refusing and recording removes the whole
// class: the refusal path is the forbid path, already reviewed sound.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/event"
)

// askRefusal is what the model is told in place of an ask command's output.
func askRefusal(command string) string {
	return "Held by the AgentBus gateway: waiting for a person to approve " + strconv.Quote(command) +
		". The command did not run. A person may allow it, which continues this session so you can run it."
}

// approval is one held reply, recorded until a person answers. It lists every
// ask command in the reply and names the turn it refused, so an allow can start
// a continuation of that turn.
type approval struct {
	id        string
	session   string
	turn      string
	callID    string // the first held call, for the event's tool_id
	name      string
	commands  []string // the raw commands the agent asked to run, for a person to read
	rules     []string // the ask entries they matched, which an allow moves to allow
	requested time.Time
	deciding  bool // an allow is in progress; a second allow is refused (409)
}

// ApprovalInfo is a held reply as the API lists it, and what Decide returns so
// the API can start the continuation. It carries no token and no header value.
type ApprovalInfo struct {
	ID        string    `json:"id"`
	Session   string    `json:"session"`
	Turn      string    `json:"turn,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	ToolID    string    `json:"tool_id,omitempty"`
	ToolName  string    `json:"tool_name,omitempty"`
	Command   string    `json:"command"`
	Commands  []string  `json:"commands"`
	Rule      string    `json:"rule"`
	Rules     []string  `json:"rules"`
	Requested time.Time `json:"requested"`
}

func (ap *approval) info() ApprovalInfo {
	rule := ""
	if len(ap.rules) > 0 {
		rule = ap.rules[0]
	}
	return ApprovalInfo{
		ID: ap.id, Session: ap.session, Turn: ap.turn, Agent: ap.name,
		ToolID: ap.callID, ToolName: ap.name, Command: strings.Join(ap.commands, "\n"),
		Commands: ap.commands, Rule: rule, Rules: ap.rules, Requested: ap.requested,
	}
}

type approvalRegistry struct {
	mu sync.Mutex
	m  map[string]*approval
}

// recordApproval records a held reply's ask commands and returns its info.
func (g *Gateway) recordApproval(session, turn, callID, name string, commands, rules []string) ApprovalInfo {
	ap := &approval{
		id:        "ap-" + g.run + "-" + strconv.FormatUint(g.seq.Add(1), 10),
		session:   session,
		turn:      turn,
		callID:    callID,
		name:      name,
		commands:  commands,
		rules:     rules,
		requested: g.now(),
	}
	g.approvals.mu.Lock()
	if g.approvals.m == nil {
		g.approvals.m = map[string]*approval{}
	}
	g.approvals.m[ap.id] = ap
	g.approvals.mu.Unlock()
	return ap.info()
}

// Approvals is a snapshot of the held replies waiting for an answer, oldest
// first, for GET /api/v0/approvals.
func (g *Gateway) Approvals() []ApprovalInfo {
	g.approvals.mu.Lock()
	out := make([]ApprovalInfo, 0, len(g.approvals.m))
	for _, ap := range g.approvals.m {
		out = append(out, ap.info())
	}
	g.approvals.mu.Unlock()
	sortByRequested(out)
	return out
}

// Claim marks a held reply as being answered, so the API can do an allow's work
// (move to allow, start the continuation) before it is recorded, and a second
// concurrent allow does not start a second continuation. It returns the info,
// whether this caller claimed it, and whether it exists at all: exists false is
// a 404; exists true with claimed false (another allow has it) is a 409. On a
// failure the caller must Release it so a person can try again.
func (g *Gateway) Claim(id string) (info ApprovalInfo, claimed, exists bool) {
	g.approvals.mu.Lock()
	defer g.approvals.mu.Unlock()
	ap := g.approvals.m[id]
	if ap == nil {
		return ApprovalInfo{}, false, false
	}
	if ap.deciding {
		return ap.info(), false, true
	}
	ap.deciding = true
	return ap.info(), true, true
}

// Release returns a claimed reply to pending, after an allow's work failed.
func (g *Gateway) Release(id string) {
	g.approvals.mu.Lock()
	if ap := g.approvals.m[id]; ap != nil {
		ap.deciding = false
	}
	g.approvals.mu.Unlock()
}

// SortApprovals orders held replies oldest first, so a caller that gathers them
// from several gateways can order the whole.
func SortApprovals(a []ApprovalInfo) { sortByRequested(a) }

func sortByRequested(a []ApprovalInfo) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].Requested.Before(a[j-1].Requested); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// Decide records a person's answer to a held reply and forgets it, returning
// the reply's info so the API can start a continuation turn on an allow. ok is
// false when there is no such reply, or it was already answered. It settles
// nothing on the wire: the turn already ended when the call was refused.
func (g *Gateway) Decide(id string, allow bool, by string) (ApprovalInfo, bool) {
	g.approvals.mu.Lock()
	ap := g.approvals.m[id]
	if ap != nil {
		delete(g.approvals.m, id)
	}
	g.approvals.mu.Unlock()
	if ap == nil {
		return ApprovalInfo{}, false
	}
	g.emitDecided(ap, decisionText(allow, by))
	return ap.info(), true
}

// emitDecided records approval.decided for a held reply, outside any live
// exchange (the turn ended when the call was refused).
func (g *Gateway) emitDecided(ap *approval, text string) {
	g.queue.push(event.Event{
		ID:      fmt.Sprintf("e-%s-%d", g.run, g.seq.Add(1)),
		Kind:    event.ApprovalDecided,
		Time:    g.now(),
		Session: ap.session,
		Turn:    ap.turn,
		Vendor:  g.adapter.Vendor(),
		ToolID:  ap.callID,
		Rule:    firstOf(ap.rules),
		Text:    text,
	})
}

func firstOf(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func decisionText(allow bool, by string) string {
	switch {
	case allow && by != "":
		return "allowed by " + by
	case allow:
		return "allowed"
	case by != "":
		return "refused by " + by
	default:
		return "refused"
	}
}
