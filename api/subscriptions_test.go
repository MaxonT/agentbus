package api

import "testing"

// The cockpit's Connect step, which the CLI's sign-in guidance points to
// (cmd/agentbus's connectRemoved and gatewayLocalRemoved), is backed by a
// real route: POST /api/v0/subscriptions/{id}/connect. This fails if that
// route is renamed or removed, so the guidance cannot come to point at
// nothing.
func TestConnectRouteExists(t *testing.T) {
	const method, path = "POST", "/api/v0/subscriptions/{id}/connect"
	for _, r := range subscriptionRoutes {
		if r.method == method && r.path == path {
			if r.op != "connectSubscription" {
				t.Fatalf("connect route op is %q, want connectSubscription", r.op)
			}
			return
		}
	}
	t.Fatalf("no %s %s route; the CLI's sign-in guidance names the cockpit Connect step, which this route backs", method, path)
}
