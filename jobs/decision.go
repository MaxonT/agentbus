package jobs

// GET /api/v0/runner/attempts/{id}/decision: the runner-protocol read a holding
// gateway uses to learn whether a person has answered an Ask it is holding (H1,
// gateway/hold.go). The guard runs on the runner's OWN gateway, which cannot
// see control's in-memory state, so it polls this: control's record is the
// truth, and the same poll serves agentbus up's built-in runner and a remote
// one (one gateway path, 0.5.10). It is scoped to the runner's own attempt — a
// runner's token is the runner protocol's, not the client API's, so it may read
// only the approvals of an attempt that is its own and current, never an
// arbitrary approval. Added in 0.6.1; protocol stays 5 (an older runner never
// calls it, and since 3c18fe0 a runner refuses a control older than itself, so
// a holding runner always meets a control that serves this).

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// The three states GET .../decision reports for a held approval. The runner's
// Decider reads them back (runner/decide.go), so they are exported: one
// spelling shared by the control handler and the runner that polls it.
const (
	DecisionPending = "pending"
	DecisionAllowed = "allowed"
	DecisionRefused = "refused"
)

// AttemptDecision is GET /api/v0/runner/attempts/{id}/decision's answer: the
// state of the one held approval the query names. pending keeps the hold going
// (and pinging); allowed releases the held call so the agent runs it in the
// same turn; refused refuses it.
type AttemptDecision struct {
	State string `json:"state" doc:"pending, allowed or refused."`
}

// decision answers a holding gateway's poll for one approval of its own current
// attempt. number and approval are query values: number identifies the current
// attempt (as every attempt call does), approval the held reply's id.
func (s *Server) decision(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Record == nil {
		fail(w, http.StatusNotFound, "this service keeps no record")
		return
	}
	approval := r.URL.Query().Get("approval")
	if approval == "" {
		fail(w, http.StatusBadRequest, "approval is required")
		return
	}
	number, err := strconv.Atoi(r.URL.Query().Get("number"))
	if err != nil || number <= 0 {
		fail(w, http.StatusBadRequest, "number is required")
		return
	}
	// The attempt must be this runner's and current, the same check every
	// attempt call makes.
	if _, err := s.cfg.Queue.Current(rn.ID, r.PathValue("id"), number); err != nil {
		s.queueError(w, err)
		return
	}
	// Bind the polled id to this authenticated attempt — the SAME binding the
	// record route applies when it stores an approval — so a runner reads only
	// an approval of its own attempt: a foreign id binds to this attempt and
	// finds nothing, never another turn's answer.
	state, err := s.approvalState(scopeToAttempt(r.PathValue("id"), approval))
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, AttemptDecision{State: state})
}

// scopeToAttempt binds an approval id to the attempt that owns it, as
// <attempt>-<id>. It is idempotent: an id the gateway already scoped to this
// attempt is left as is; anything else — a legacy runner's raw id, or one a
// runner named for another attempt — is prefixed, so it lands owned by THIS
// attempt and unique to it. Control applies it wherever a runner writes or
// reads an approval (record.go and above), so an id is bound to the
// authenticated attempt whatever the runner sent: a legacy raw id is stored
// (its batch is not rejected), and a forged or foreign id cannot touch another
// turn's approval. The attempt id is t_<hex> (no '-'), so the prefix is
// unambiguous.
func scopeToAttempt(attemptID, approval string) string {
	prefix := attemptID + "-"
	if strings.HasPrefix(approval, prefix) {
		return approval
	}
	return prefix + approval
}

// approvalState reports an approval's state from the record. The id is scoped to
// the attempt that raised it (<attempt>.<local>, bound at the record route), so
// it names its one owner and reads by id alone: pending until the record holds
// an approval.decided for it, then allowed or refused. An id with no request in
// the record reads as pending, so a holding gateway keeps holding (and
// eventually times out) rather than act on an answer that is not there.
func (s *Server) approvalState(approval string) (string, error) {
	evs, err := s.cfg.Record.ApprovalEvents()
	if err != nil {
		return "", err
	}
	requested, decided := false, ""
	for _, e := range evs {
		if e.Approval != approval {
			continue
		}
		switch e.Kind {
		case event.ApprovalRequested:
			requested = true
		case event.ApprovalDecided:
			if approvalAllowed(e.Text) {
				decided = DecisionAllowed
			} else {
				decided = DecisionRefused
			}
		}
	}
	if requested && decided != "" {
		return decided, nil
	}
	return DecisionPending, nil
}

// approvalAllowed classifies a recorded approval.decided as an allow. Every
// allow is written by control's approvals API, whose decisionText begins
// "allowed" (allowed once / allowed from now on); the gateway's own decided (a
// timeout or a closed connection) is always a refusal and never starts with
// "allowed". So the one prefix tells an allow from a refusal.
func approvalAllowed(text string) bool { return strings.HasPrefix(text, "allowed") }
