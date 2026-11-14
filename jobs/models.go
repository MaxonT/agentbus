package jobs

// A listing of models on a subscription, with no turn: control runs the
// unmodified harness (codex app-server) in a clean home of its own, wired
// as a relayed turn is, and the harness asks its vendor for the models its
// sign-in may use. Its one call to the vendor comes to the relay with a
// token of its own (ModelsToken), and the relay adds the subscription kept
// on control to that call only: the harness's own list-models call
// (adapter.Call Models) on its own vendor's upstream. Any other call with
// the token is refused in the vendor's place and recorded. Every call of a
// listing is recorded under ModelsSession, with no turn.
//
// A listing token is random, kept in memory while the listing runs, and
// stops with it (the func ModelsToken returns), across nothing: a restart
// of control ends every listing.

import (
	"crypto/rand"
	"errors"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/gateway"
)

// ModelsSession is the record's session of control's listings of models.
const ModelsSession = "models"

// ListingPrefix marks a listing token.
const ListingPrefix = "abm_"

// Why a listing's call gets no credential.
const (
	notListing = "a listing of models gets a credential only on its harness's own list-models call"
	noListing  = "control relays no listing of models for this harness"
)

// listings are the tokens of the listings running now, by token: the
// harness each lists.
type listings struct {
	mu sync.Mutex
	m  map[string]string
}

// ModelsToken starts a listing of harness's models on its subscription.
// base is the relay's path for its vendor, such as /relay/chatgpt, under
// which the harness sends its list-models call, and token is its
// placeholder credential, which works until end is called. ok is false
// when the relay has no upstream for harness's subscription.
func (s *Server) ModelsToken(harness string) (base, token string, end func(), ok bool) {
	name := ""
	for n, up := range s.cfg.Relay {
		if up.Access == credentials.AccessSubscription && up.Harness == harness && up.Under == "" {
			name = n
			break
		}
	}
	if name == "" {
		return "", "", nil, false
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", nil, false
	}
	token = ListingPrefix + hex.EncodeToString(b[:])
	s.listings.mu.Lock()
	if s.listings.m == nil {
		s.listings.m = map[string]string{}
	}
	s.listings.m[token] = harness
	s.listings.mu.Unlock()
	end = func() {
		s.listings.mu.Lock()
		delete(s.listings.m, token)
		s.listings.mu.Unlock()
	}
	return RelayPrefix + name, token, end, true
}

// listing is the harness of the listing tok belongs to, while it runs.
func (s *Server) listing(tok string) (string, bool) {
	s.listings.mu.Lock()
	defer s.listings.mu.Unlock()
	h, ok := s.listings.m[tok]
	return h, ok
}

// relayListing records and forwards a call of a listing of harness's models
// on up, with the subscription kept on control on the harness's own
// list-models call only.
func (s *Server) relayListing(w http.ResponseWriter, r *http.Request, up Upstream, harness, path string) {
	rl := gateway.Relayed{Session: ModelsSession, Path: path}
	g := gatewayFor(up.Gateways, r.Method, path)
	rl.Prepare = func(rl *gateway.Relayed) {
		switch {
		case up.Access != credentials.AccessSubscription || up.Harness != harness:
			rl.Refuse = refusal(http.StatusForbidden, noListing)
		case !g.ListsModels(r.Method, path):
			rl.Refuse = refusal(http.StatusForbidden, notListing)
		default:
			rl.Header, rl.Signin, rl.Refuse = s.listingCredential(up)
		}
	}
	g.ServeRelayed(w, r, rl)
}

// listingCredential is the subscription kept on control for up's harness,
// or the refusal.
func (s *Server) listingCredential(up Upstream) (func(http.Header), gateway.Signin, *gateway.Refusal) {
	if s.cfg.Credentials == nil {
		return nil, gateway.Signin{}, refusal(http.StatusForbidden, "control keeps no credentials for runners")
	}
	cred, err := s.cfg.Credentials.Credential(credentials.Need{Access: credentials.AccessSubscription, Harness: up.Harness})
	var missing *credentials.Missing
	switch {
	case errors.As(err, &missing):
		return nil, gateway.Signin{}, refusal(http.StatusForbidden, missing.Why)
	case err != nil:
		if s.Logf != nil {
			s.Logf("relay.credential_failed session=%s harness=%s", ModelsSession, up.Harness)
		}
		return nil, gateway.Signin{}, refusal(http.StatusInternalServerError, "control could not read its credentials")
	}
	return credentialHeader(cred), gateway.Signin{Subscription: up.Harness, Connection: cred.Connection}, nil
}
