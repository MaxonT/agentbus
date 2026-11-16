package main

import (
	"sync"

	"github.com/staysup/agentbus/gateway"
)

// gatewayApprovals gathers the held calls across this process's gateways, for
// the API (api.ApprovalSource): the gateway that served a turn's model call is
// the one that holds its ask command. The gateways are set once at startup,
// before the service serves, so a person's list and answer reach whichever one
// holds the call.
//
// Only the gateways of this process are reached. On a separate control, a call
// held on a runner's gateway is not surfaced here yet; agentbus up holds and
// answers in the one process.
type gatewayApprovals struct {
	mu  sync.Mutex
	gws []*gateway.Gateway
}

func (a *gatewayApprovals) use(gws []*gateway.Gateway) {
	a.mu.Lock()
	a.gws = gws
	a.mu.Unlock()
}

func (a *gatewayApprovals) list() []*gateway.Gateway {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gws
}

func (a *gatewayApprovals) Approvals() []gateway.ApprovalInfo {
	var out []gateway.ApprovalInfo
	for _, g := range a.list() {
		out = append(out, g.Approvals()...)
	}
	// Each gateway returns its own oldest first; order the whole the same.
	gateway.SortApprovals(out)
	return out
}

func (a *gatewayApprovals) Claim(id string) (gateway.ApprovalInfo, bool, bool) {
	for _, g := range a.list() {
		if info, claimed, exists := g.Claim(id); exists {
			return info, claimed, true
		}
	}
	return gateway.ApprovalInfo{}, false, false
}

func (a *gatewayApprovals) Release(id string) {
	for _, g := range a.list() {
		g.Release(id)
	}
}

func (a *gatewayApprovals) Decide(id string, allow bool, by string) (gateway.ApprovalInfo, bool) {
	for _, g := range a.list() {
		if info, ok := g.Decide(id, allow, by); ok {
			return info, true
		}
	}
	return gateway.ApprovalInfo{}, false
}
