package jobs

// A runner's record of a turn, sent to the control plane while the turn
// runs and once more at its end, so the control plane's record holds the
// turn as a turn on its own machine does: what a cockpit reads (GET
// /api/v0/sessions, /turns, /calls), while the turn runs. Each event keeps
// its id, so a batch sent twice is stored once. No runner path reaches
// the control plane: an event's folder (the copy's, on the runner) is
// dropped; its folder_ref stays.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// RecordBatch is the body of POST /api/v0/runner/attempts/{id}/record: events of
// the attempt's turn, as the runner's own record stored them.
type RecordBatch struct {
	Number int               `json:"number" doc:"The attempt's number."`
	Events []json.RawMessage `json:"events" doc:"The turn's events, at most 500, each with its id and the job's turn id as turn."`
}

// RunnerKinds are the event kinds a runner may send in a RecordBatch: those
// its run engine (run/), its gateway and adapters, and its replay write for
// a turn. Every other kind is control's own and is refused: events.dropped
// (the store's, marking other sessions'), turn.changed and turn.applied
// (control's record of a job's end, outcome.go), plan.usage (control's
// reading of a subscription it holds, gateway/plan.go), tools.listed
// (control's tool server), session.renamed (the API), control's operations
// in no session, the retired kinds, and a program's x.* kinds.
var RunnerKinds = map[event.Kind]bool{
	event.TurnStarted: true, event.ModelRequested: true, event.ModelResponded: true,
	event.ToolRequested: true, event.ToolCompleted: true, event.TurnCompleted: true,
	event.SessionOpened: true, event.SessionClosed: true, event.ContextAdded: true,
	event.NetworkRefused: true, event.SchemaUnexpected: true, event.ReplayFailed: true,
	event.ToolRefused: true, event.CallRefused: true, event.VendorCalled: true,
	event.ApprovalRequested: true,
}

// RecordAck answers a RecordBatch: how many events were new.
type RecordAck struct {
	Stored int `json:"stored"`
}

// Recorder is the control plane's record, for a runner's turn.
type Recorder interface {
	AppendOnce(e event.Event) (bool, error)
	CallSession(exchange string) (session string, found bool, err error)
	OtherTurns(exchange, session, turn string) (bool, error)
	Continuation(session string) (store.Continuation, error)
	Finder
}

func (s *Server) record(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Record == nil {
		fail(w, http.StatusNotFound, "this service keeps no record")
		return
	}
	var b RecordBatch
	if !decode(r, &b) || len(b.Events) > MaxBatch {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number and at most 500 events")
		return
	}
	j, err := s.cfg.Queue.Current(rn.ID, r.PathValue("id"), b.Number)
	if err != nil {
		s.queueError(w, err)
		return
	}
	turn := j.Turn
	evs := make([]event.Event, 0, len(b.Events))
	for _, raw := range b.Events {
		var e event.Event
		if json.Unmarshal(raw, &e) != nil || e.ID == "" || e.Kind == "" || len(e.Kind) > 64 || turn == "" || e.Turn != turn || e.Session == "" {
			fail(w, http.StatusBadRequest, "every event needs an id, a kind, a session, and the job's turn as turn")
			return
		}
		// Only a kind a runner writes for its turn is taken: anything else
		// (control's own record, a program's x.* kinds, a kind added later)
		// is refused until it is listed in RunnerKinds.
		if !RunnerKinds[e.Kind] {
			fail(w, http.StatusBadRequest, string(e.Kind)+" is not an event a runner sends")
			return
		}
		e.Seq, e.Folder = 0, ""
		evs = append(evs, e)
	}
	// One rule: every event is of the session control assigned the job.
	// A turn job's is its session; a review's is the one control named
	// when it made the job (Links.ReviewSession), never one the runner
	// names. Both are fixed when the job is made, so the check needs no
	// lock with the append. Any other job has no record to send.
	assigned := assignedSession(j)
	for _, e := range evs {
		if assigned == "" || e.Session != assigned {
			fail(w, http.StatusBadRequest, "every event must be of the session control assigned the attempt's job")
			return
		}
	}
	n := 0
	for _, e := range evs {
		// The turn's end is stored after the calls the relay carried for it
		// are: a reader that sees the end sees all of them. One the relay
		// could not confirm leaves the turn marked unsettled, as the runner
		// marks one its own gateway could not.
		if e.Kind == event.SessionClosed && e.Turn != "" && !s.settle(e.Session, e.Turn) {
			e.Unsettled = true
		}
		stored, err := s.cfg.Record.AppendOnce(e)
		if err != nil {
			s.internal(w)
			return
		}
		if stored {
			n++
		}
	}
	writeJSON(w, http.StatusOK, RecordAck{Stored: n})
}

// CloseLost stores session.closed, unsettled, for each hold the record has
// open whose turn's attempt lease ran out. Its runner stopped
// during the turn, so nothing else closes it: the cockpit would show the
// turn running, and the session would take no next turn. It is what
// store.CloseStaleHolds does at start for this process's own turns.
func CloseLost(q *queue.Queue, st *store.Store, now time.Time) (int, error) {
	holds, err := st.OpenHolds(st.Last())
	if err != nil || len(holds) == 0 {
		return 0, err
	}
	var turns []string
	for _, h := range holds {
		if h.Turn != "" {
			turns = append(turns, h.Turn)
		}
	}
	lost, err := q.Lost(turns)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range holds {
		if !lost[h.Turn] {
			continue
		}
		if _, _, err := st.Append(event.Event{ID: queue.NewID("evt_"), Kind: event.SessionClosed, Time: now,
			Session: h.Session, Turn: h.Turn, Hold: h.Hold, Unsettled: true, State: store.TurnFailed}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
