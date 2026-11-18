package api

import (
	"context"
	"sync"
)

// sessionGate serialises work per session key: one holder at a time, the rest
// waiting in turn. It is used to run a session's preview once when several GETs
// of its changes arrive together, so they do not run two previews and race to
// store them. A gate is kept only while a session has a holder or a waiter, so
// the map does not grow with every session ever seen.
type sessionGate struct {
	mu sync.Mutex
	m  map[string]*gateEntry
}

type gateEntry struct {
	ch   chan struct{} // capacity 1: holding it is the lock
	refs int           // holder plus waiters; the entry is dropped at zero
}

func newSessionGate() *sessionGate {
	return &sessionGate{m: map[string]*gateEntry{}}
}

// enter blocks until this session's gate is free, then returns a leave func to
// call when done. It gives up and returns false if ctx is done first (the
// request was cancelled or timed out), having left nothing to release.
func (g *sessionGate) enter(ctx context.Context, session string) (leave func(), ok bool) {
	if g == nil {
		// No gate (an API built without New, in a test): do not serialise.
		return func() {}, true
	}
	g.mu.Lock()
	e := g.m[session]
	if e == nil {
		e = &gateEntry{ch: make(chan struct{}, 1)}
		g.m[session] = e
	}
	e.refs++
	g.mu.Unlock()

	drop := func() {
		g.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(g.m, session)
		}
		g.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() {
			<-e.ch
			drop()
		}, true
	case <-ctx.Done():
		drop()
		return nil, false
	}
}
