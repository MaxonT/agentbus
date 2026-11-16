package api

// GET /api/v0/approvals and POST /api/v0/approvals/{id}: the commands a person
// must answer. The gateway refuses a tool call whose command matches an ask
// entry of permissions.toml (gateway/approvals.go) and records it as an
// approval.requested event; the turn ends. Control derives the pending list
// from the record — a requested with no approval.decided for the same id — so
// the gateway that refused may run on a separate runner, as long as its event
// reached control's store. Allow means "allow from now on": the matched ask
// entries move to allow in the settings, control writes approval.decided, and a
// continuation turn runs in the same session so the agent can carry on. Refuse
// writes approval.decided only. No token or header value is in the answer.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// HeldCall is one held reply in GET /api/v0/approvals.
type HeldCall struct {
	ID        string    `json:"id" doc:"Answer it with POST /api/v0/approvals/{id}."`
	Session   string    `json:"session" doc:"The session whose turn asked."`
	Turn      string    `json:"turn,omitempty" doc:"The turn the ask came from; an allow continues this session on that turn's agent."`
	Agent     string    `json:"agent,omitempty" doc:"The session's agent (harness) that asked, such as claude; a cockpit shows its product name. Absent for a session no run started."`
	ToolID    string    `json:"tool_id,omitempty" doc:"The model's id for the tool call."`
	ToolName  string    `json:"tool_name,omitempty" doc:"The shell tool's name, such as Bash."`
	Command   string    `json:"command" doc:"Every held command in the reply, one per line, for a person to read at a glance."`
	Commands  []string  `json:"commands" doc:"Every held command in the reply, in order: a cockpit shows one card with a row per command. One answer (once, always or refuse) covers them all."`
	Rule      string    `json:"rule" doc:"An entry an \"always\" would add to the allow list: the ask entry the first command matched, or one suggested from a command on no list (such as git push). Empty when nothing clean can be suggested (a run-time command)."`
	Requested time.Time `json:"requested" doc:"When the command was refused and recorded."`
}

// HeldCalls is the answer to GET /api/v0/approvals.
type HeldCalls struct {
	Approvals []HeldCall `json:"approvals" doc:"The commands waiting for you, oldest first. Empty when none waits."`
}

// Decision is the body of POST /api/v0/approvals/{id}.
type Decision struct {
	Decision string `json:"decision" doc:"once runs a continuation turn that may run exactly these commands, this turn only, and writes nothing to the settings; always moves each matched command from ask to allow in the settings and runs a continuation turn; refuse records the decision only and leaves the settings unchanged. (allow is the old name for always.)"`
}

// heldReply is what control reads back from an approval.requested event to list
// and to answer it.
type heldReply struct {
	id        string
	session   string
	turn      string
	toolID    string
	toolName  string
	commands  []string
	rules     []string
	requested time.Time
}

func heldReplyOf(e event.Event) heldReply {
	return heldReply{
		id: e.Approval, session: e.Session, turn: e.Turn, toolID: e.ToolID,
		toolName: e.ToolName, commands: e.Commands, rules: e.Rules, requested: e.Time,
	}
}

// heldCall builds the listed approval. agent is the session's harness (such as
// claude), which the person sees as the agent that asked; it is not the shell
// tool the command came from (that stays in ToolName).
func (h heldReply) heldCall(agent string) HeldCall {
	rule := ""
	if len(h.rules) > 0 {
		rule = h.rules[0]
	}
	return HeldCall{
		ID: h.id, Session: h.session, Turn: h.turn, Agent: agent,
		ToolID: h.toolID, ToolName: h.toolName, Command: strings.Join(h.commands, "\n"),
		Commands: h.commands, Rule: rule, Requested: h.requested.UTC(),
	}
}

// pending reads the record and returns the approvals not yet decided, oldest
// first. decided holds the ids already answered, for the existence check.
func (a *API) pendingApprovals() (replies []heldReply, decided map[string]bool, err error) {
	evs, err := a.cfg.Store.ApprovalEvents()
	if err != nil {
		return nil, nil, err
	}
	decided = map[string]bool{}
	for _, e := range evs {
		if e.Kind == event.ApprovalDecided {
			decided[e.Approval] = true
		}
	}
	for _, e := range evs {
		if e.Kind != event.ApprovalRequested || e.Approval == "" || decided[e.Approval] {
			continue
		}
		replies = append(replies, heldReplyOf(e))
	}
	return replies, decided, nil
}

// GET /api/v0/approvals
func (a *API) listApprovals(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	if s := r.URL.Query().Get("state"); s != "" && s != "pending" {
		writeError(w, http.StatusBadRequest, `state must be "pending" or left out; decided commands are in the events`)
		return
	}
	replies, _, err := a.pendingApprovals()
	if err != nil {
		a.internal(w)
		return
	}
	out := []HeldCall{}
	harnessOf := map[string]string{} // one lookup per session, not per command
	for _, h := range replies {
		agent, ok := harnessOf[h.session]
		if !ok {
			agent, err = a.cfg.Store.SessionHarness(h.session)
			if err != nil {
				a.internal(w)
				return
			}
			harnessOf[h.session] = agent
		}
		out = append(out, h.heldCall(agent))
	}
	writeJSON(w, http.StatusOK, HeldCalls{Approvals: out})
}

// POST /api/v0/approvals/{id}
func (a *API) decideApproval(w http.ResponseWriter, r *http.Request, id string) {
	var body Decision
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, `the body must be one JSON object with decision: "once", "always" or "refuse"`)
		return
	}
	// once: run the commands this continuation turn only, settings unchanged.
	// always: move the matched entries to allow (from now on) and continue.
	// refuse: record only. ("allow" is the old name for "always".)
	var once, always bool
	decision := body.Decision
	switch decision {
	case "once":
		once = true
	case "always", "allow":
		always, decision = true, "always"
	case "refuse":
	default:
		writeError(w, http.StatusBadRequest, `decision must be "once", "always" or "refuse"`)
		return
	}
	// Claim it first, so two answers at once (two tabs, a retry) do not both
	// move the settings or start two continuations. The claim is in-process;
	// approval.decided in the record is the durable "answered" mark.
	if !a.claimApproval(id) {
		writeError(w, http.StatusConflict, "that command is already being answered")
		return
	}
	done := false
	defer func() {
		if !done {
			a.releaseApproval(id)
		}
	}()
	// Read the held reply from the record, and whether it was answered already.
	h, exists, decided, err := a.approvalByID(id)
	if err != nil {
		a.internal(w)
		return
	}
	if !exists || decided {
		writeError(w, http.StatusNotFound, "no command by that id, or it was answered already")
		return
	}
	// Do the work before recording the answer, so a failure leaves it pending
	// to try again rather than losing it. always moves the matched entries to
	// allow; once leaves the settings untouched and lets the continuation turn
	// alone run the commands.
	if always {
		if err := a.allowCommands(h.rules); err != nil {
			writeError(w, http.StatusInternalServerError, "the approval could not be written to the settings")
			return
		}
	}
	if once || always {
		// Both carry the commands as allowed once, so the continuation turn runs
		// them even for a command "always" could not add to the allow list (one
		// built at run time, with no clean rule). "always" additionally moved the
		// matched entries to allow above, for the turns after this one.
		if err := a.continueAfterApproval(h, h.commands); err != nil {
			writeError(w, http.StatusInternalServerError, "the continuation turn could not be started")
			return
		}
	}
	if err := a.recordDecision(h, decision, "a person"); err != nil {
		a.internal(w)
		return
	}
	done = true
	a.releaseApproval(id)
	w.WriteHeader(http.StatusNoContent)
}

// approvalByID reads one held reply from the record by its id, and whether a
// decision for it was recorded already.
func (a *API) approvalByID(id string) (h heldReply, exists, decided bool, err error) {
	evs, err := a.cfg.Store.ApprovalEvents()
	if err != nil {
		return heldReply{}, false, false, err
	}
	for _, e := range evs {
		if e.Approval != id {
			continue
		}
		switch e.Kind {
		case event.ApprovalRequested:
			exists = true
			h = heldReplyOf(e)
		case event.ApprovalDecided:
			decided = true
		}
	}
	return h, exists, decided, nil
}

// recordDecision writes approval.decided for a held reply: Approval ties it to
// its request, and the text says how it ended (once, always or refuse) and who
// answered.
func (a *API) recordDecision(h heldReply, decision, by string) error {
	e := event.Event{
		Kind:     event.ApprovalDecided,
		Session:  h.session,
		Turn:     h.turn,
		Approval: h.id,
		ToolID:   h.toolID,
		Rule:     firstOf(h.rules),
		Text:     decisionText(decision, by),
	}
	e.ID, e.Time = newEventID(), a.cfg.Now().UTC()
	_, _, err := a.cfg.Store.Append(e)
	return err
}

// claimApproval reserves an id for one in-flight answer; false means another
// answer holds it (a 409). releaseApproval frees it.
func (a *API) claimApproval(id string) bool {
	a.decidingMu.Lock()
	defer a.decidingMu.Unlock()
	if a.deciding[id] {
		return false
	}
	if a.deciding == nil {
		a.deciding = map[string]bool{}
	}
	a.deciding[id] = true
	return true
}

func (a *API) releaseApproval(id string) {
	a.decidingMu.Lock()
	delete(a.deciding, id)
	a.decidingMu.Unlock()
}

// allowCommands moves each matched ask entry (the rule, a valid permissions
// entry, not the raw command) from the ask list to the allow list, the same
// change PATCH /api/v0/settings/permissions makes. So the runner reads the
// command as allowed from the next turn, and nothing is left in ask to refuse
// the continuation again.
func (a *API) allowCommands(rules []string) error {
	if a.cfg.Settings == nil || len(rules) == 0 {
		return nil
	}
	// Retry on a concurrent change (another allow, or a settings edit): read,
	// move, write, and read again if the version moved under us.
	for try := 0; try < 8; try++ {
		p, v, err := a.cfg.Settings.Policy()
		if err != nil {
			return err
		}
		change := settings.Policy{Allow: union(p.Allow, rules), Ask: without(p.Ask, rules)}
		if _, _, err = settings.ChangePolicy(a.cfg.Settings, change, v); !errors.Is(err, settings.ErrChanged) {
			return err
		}
	}
	return settings.ErrChanged
}

// continueAfterApproval starts a turn in the approval's session, on the agent
// of the turn the ask came from, so the agent can run the now-allowed commands.
// Like every other continuation path, it names only the session (not a folder),
// so the runner resumes the session's own workspace. allowOnce are the command
// lines the continuation turn's gateway lets run this turn only
// (queue.Spec.AllowOnce). Both "once" and "always" pass the commands, so the
// turn runs even one "always" could not add to the allow list (a command built
// at run time); "always" has also moved the matched entries to allow, for the
// turns after this one.
func (a *API) continueAfterApproval(h heldReply, allowOnce []string) error {
	if a.cfg.Queue == nil || h.session == "" || h.turn == "" {
		return nil
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, h.turn)
	if errors.Is(err, queue.ErrNotFound) {
		// The turn that asked is gone (an old approval, another runner). The
		// commands are allowed from now on; there is just no turn to continue.
		return nil
	}
	if err != nil {
		return err
	}
	src := j.Spec
	spec := queue.Spec{
		Harness: src.Harness, AgentID: src.AgentID, Access: src.Access, KeyEnv: src.KeyEnv,
		Session: h.session, Model: src.Model, ModelFromAgent: src.ModelFromAgent,
		AgentModel: src.AgentModel, ReadOnly: src.ReadOnly, AllowOnce: allowOnce,
		Prompt: "A person approved: you may now run " + strings.Join(h.commands, ", ") + ". Run them and continue.",
	}
	links := queue.Links{ContinuesTurn: h.turn, Chain: j.Links.Chain}
	_, _, err = a.cfg.Queue.CreateTurnLinked(queue.DefaultTeam, "", spec, links, queue.Limits{})
	var removed *queue.RunnerRemovedError
	if errors.Is(err, queue.ErrSessionClosed) || errors.Is(err, queue.ErrSessionEnded) || errors.As(err, &removed) {
		// The session cannot take another turn (closed, ended, or its machine
		// removed). The commands are allowed from now on; there is just no turn
		// to run.
		return nil
	}
	return err
}

// decisionText says how an approval ended, for approval.decided: allowed once,
// allowed from now on, or refused, and by whom.
func decisionText(decision, by string) string {
	verb := "refused"
	switch decision {
	case "once":
		verb = "allowed once"
	case "always":
		verb = "allowed from now on"
	}
	if by != "" {
		return verb + " by " + by
	}
	return verb
}

func firstOf(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// without is list with every entry in drop removed.
func without(list, drop []string) []string {
	out := []string{}
	for _, s := range list {
		if !contains(drop, s) {
			out = append(out, s)
		}
	}
	return out
}

// union is list with every entry of add it does not already hold appended.
func union(list, add []string) []string {
	out := append([]string{}, list...)
	for _, s := range add {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
