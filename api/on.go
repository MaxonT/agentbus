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
	// IdempotencyKey is the same mechanism POST /api/v0/turns takes: a key
	// already used returns the on it made, so a retry does not add a second.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"An optional idempotency key, as POST /api/v0/turns takes: the same key returns the same on, so a retry registers no second delivery."`
}

// OnView is a registered on, as the API shows it. The queue's job id stays out.
type OnView struct {
	ID      string `json:"id" doc:"The on's id (on_...), to remove it with DELETE /api/v0/ons/{id}."`
	Event   string `json:"event" doc:"The event it reacts to. For now, turn.completed."`
	Turn    string `json:"turn" doc:"The turn it watches."`
	Session string `json:"session" doc:"The session its action delivers to."`
}

const (
	turnOnRefused       = "a turn token may register an on only for a turn its own turn started, delivering to its own session"
	turnOnRemoveRefused = "a turn token may remove only an on its own turn registered"
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
	// The same key validator as POST /turns (his call, 2026-11-19): a key with a
	// '/' is refused here too, so no caller can land on an internal delivery/
	// result/ or apply/ key and collide with work the core registers itself.
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
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
	// B15 (his call, 2026-11-19): an on may watch ANY turn the caller's token
	// may reach — core does not single out asked work as a kind. What stays is
	// credential scoping, not a kind check:
	//   - a turn token may watch only a turn its own turn started (its request
	//     tree), delivering to its own session. This still implies asked work,
	//     but as a consequence of the token's reach, not a rule about kinds.
	//   - the main token may watch any turn and deliver to any session it names;
	//     it can already start a turn in any session. A main-token on over a
	//     person's turn is made safe by the self-retire checks (B13b), not by a
	//     refusal here.
	session := body.Session
	var maker queue.Maker
	if g := grantOf(r); g != nil {
		if j.Links.Chain == nil || j.Links.Chain.By != g.Turn {
			writeError(w, http.StatusForbidden, turnOnRefused)
			return
		}
		session = g.Session
		maker = queue.Maker{Session: g.Session, Turn: g.Turn}
	} else if session == "" {
		writeError(w, http.StatusBadRequest, "session is required: where to deliver the turn's reply")
		return
	}
	on, err := a.cfg.Queue.RegisterOn(queue.DefaultTeam, queue.OnTurnCompleted, body.Turn, session, body.IdempotencyKey, maker)
	switch {
	case errors.Is(err, queue.ErrUnknownEvent):
		writeError(w, http.StatusBadRequest, "that event is not known")
		return
	case errors.Is(err, queue.ErrBadState):
		writeError(w, http.StatusBadRequest, "turn and session are required")
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key was already used to register an on for a different turn or session; send a new key")
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
