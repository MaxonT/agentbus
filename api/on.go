package api

// POST /api/v0/ons and DELETE /api/v0/ons/{id}: register and remove an on, the
// standing instruction "when this event happens, do this" (queue/on.go). B13
// has one event, a turn completing, and one action, deliver that turn's reply
// to a session as its next turn. It is the public form of what ask_agent
// registers for itself (runs.go): the primitive every later reaction is built
// on (schedule, gate), one row on control, nothing waiting.
//
// The caller's token decides what it may do. A turn token may register an on
// only for a turn its request tree started, and its result is delivered to its
// own session; it may remove only an on its own turn made. The main token may
// register an on for any turn, naming the session to deliver to, and remove
// any on.

import (
	"errors"
	"net/http"

	"github.com/staysup/agentbus/queue"
)

// OnRequest is the body of POST /api/v0/ons.
type OnRequest struct {
	Turn    string `json:"turn" doc:"The turn to watch: when it completes, its reply is delivered. A turn token may name only a turn its request tree started."`
	Session string `json:"session,omitempty" doc:"The session the reply is delivered to as its next turn. A turn token always delivers to its own session, so it leaves this out; the main token must set it."`
}

// OnView is a registered on, as the API shows it. The queue's job id stays out.
type OnView struct {
	ID      string `json:"id" doc:"The on's id (on_...), to remove it with DELETE /api/v0/ons/{id}."`
	Event   string `json:"event" doc:"The event it reacts to. For now, turn.completed."`
	Turn    string `json:"turn" doc:"The turn it watches."`
	Session string `json:"session" doc:"The session its action delivers to."`
}

const (
	turnOnRefused       = "a turn token may register an on only for a turn its own turn started (asked work), delivering to its own session"
	turnOnRemoveRefused = "a turn token may remove only an on its own turn registered"
	onNotAsked          = "an on may watch only asked work: a turn an agent started (ask_agent), which has a request tree"
	onTreeRefused       = "an on delivers only to a session in the watched turn's request tree"
)

// POST /api/v0/ons
func (a *API) registerOn(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	var body OnRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with a turn to watch")
		return
	}
	if body.Turn == "" {
		writeError(w, http.StatusBadRequest, "turn is required: the turn to watch")
		return
	}
	// The subject turn must exist, whoever the caller is: a nonexistent one is
	// 404, never a reaction that can never fire.
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, body.Turn)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noTurn)
		return
	case err != nil:
		a.internal(w)
		return
	}
	// An on reacts to asked work only (a turn an agent started), so it sits in a
	// request tree. Delivery stays inside that tree, the invariant the old
	// delivery kept, so a reply cannot pull an unrelated session into the tree
	// or reset its scope.
	if j.Links.Chain == nil || j.Links.Chain.By == "" {
		writeError(w, http.StatusForbidden, onNotAsked)
		return
	}
	root := j.Links.Chain.Root
	session := body.Session
	var maker queue.Maker
	if g := grantOf(r); g != nil {
		// A turn token may watch only a turn its own turn started (Chain.By is
		// this turn), never its own or another's, so an on cannot become a
		// self-continuation that dodges the asked-work caps. Its own session is
		// in that tree, so the reply is delivered there.
		if j.Links.Chain.By != g.Turn {
			writeError(w, http.StatusForbidden, turnOnRefused)
			return
		}
		session = g.Session
		maker = queue.Maker{Session: g.Session, Turn: g.Turn}
	} else {
		// The main token names the delivery session; it must be in the watched
		// turn's tree.
		if session == "" {
			writeError(w, http.StatusBadRequest, "session is required: where to deliver the turn's reply")
			return
		}
		inTree := session == root
		if !inTree {
			sr, err := a.cfg.Queue.SessionRoot(queue.DefaultTeam, session)
			if err != nil {
				a.internal(w)
				return
			}
			inTree = sr == root
		}
		if !inTree {
			writeError(w, http.StatusForbidden, onTreeRefused)
			return
		}
	}
	on, err := a.cfg.Queue.RegisterOn(queue.DefaultTeam, queue.OnTurnCompleted, body.Turn, session, maker)
	switch {
	case errors.Is(err, queue.ErrUnknownEvent):
		writeError(w, http.StatusBadRequest, "that event is not known")
		return
	case errors.Is(err, queue.ErrBadState):
		writeError(w, http.StatusBadRequest, "turn and session are required")
		return
	case err != nil:
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusCreated, OnView{ID: on.ID, Event: on.Event, Turn: on.Subject, Session: on.Session})
}

// DELETE /api/v0/ons/{id}
func (a *API) removeOn(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noOn)
		return
	}
	if g := grantOf(r); g != nil {
		on, err := a.cfg.Queue.GetOn(queue.DefaultTeam, id)
		switch {
		case errors.Is(err, queue.ErrNotFound):
			writeError(w, http.StatusNotFound, noOn)
			return
		case err != nil:
			a.internal(w)
			return
		}
		if on.Maker.Turn != g.Turn || on.Maker.Session != g.Session {
			writeError(w, http.StatusForbidden, turnOnRemoveRefused)
			return
		}
	}
	switch err := a.cfg.Queue.RemoveOn(queue.DefaultTeam, id); {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noOn)
	case err != nil:
		a.internal(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

const noOn = "no such on"
