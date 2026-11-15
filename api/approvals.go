package api

// GET /api/v0/approvals and POST /api/v0/approvals/{id}: the commands a person
// must answer. The gateway refuses a tool call whose command matches an ask
// entry of permissions.toml (gateway/approvals.go) and records it here; the
// turn ends. Allow means "allow from now on": the command moves from ask to
// allow in the settings, and a continuation turn runs in the same session so
// the agent can carry on. Refuse records the decision only. No token or header
// value is in the answer.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// ApprovalSource is where the API reads and answers held replies: the gateway.
// Decide records the answer and returns the reply's info, so an allow can start
// the continuation.
type ApprovalSource interface {
	Approvals() []gateway.ApprovalInfo
	// Claim marks a reply as being answered (so two concurrent answers do not
	// both act): info, whether this caller claimed it, and whether it exists.
	Claim(id string) (info gateway.ApprovalInfo, claimed, exists bool)
	// Release returns a claimed reply to pending after the answer's work failed.
	Release(id string)
	Decide(id string, allow bool, by string) (gateway.ApprovalInfo, bool)
}

// HeldCall is one held reply in GET /api/v0/approvals.
type HeldCall struct {
	ID        string    `json:"id" doc:"Answer it with POST /api/v0/approvals/{id}."`
	Session   string    `json:"session" doc:"The session whose turn asked."`
	Turn      string    `json:"turn,omitempty" doc:"The turn the ask came from; an allow continues this session on that turn's agent."`
	Agent     string    `json:"agent,omitempty" doc:"The agent's shell tool the command came from, such as Bash."`
	ToolID    string    `json:"tool_id,omitempty" doc:"The model's id for the tool call."`
	ToolName  string    `json:"tool_name,omitempty" doc:"The shell tool's name, such as Bash."`
	Command   string    `json:"command" doc:"Every ask command in the reply, one per line, for a person to read at a glance."`
	Commands  []string  `json:"commands" doc:"Every ask command in the reply, in order: a cockpit shows one card with a row per command. Allow or refuse covers them all."`
	Rule      string    `json:"rule" doc:"The ask entry of permissions.toml the first command matched, such as git push."`
	Requested time.Time `json:"requested" doc:"When the command was refused and recorded."`
}

// HeldCalls is the answer to GET /api/v0/approvals.
type HeldCalls struct {
	Approvals []HeldCall `json:"approvals" doc:"The commands waiting for you, oldest first. Empty when none waits."`
}

// Decision is the body of POST /api/v0/approvals/{id}.
type Decision struct {
	Decision string `json:"decision" doc:"allow moves each command from ask to allow in the settings and runs a continuation turn in the session; refuse records the decision only."`
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
	out := []HeldCall{}
	if a.cfg.Approvals != nil {
		for _, ap := range a.cfg.Approvals.Approvals() {
			out = append(out, HeldCall{ID: ap.ID, Session: ap.Session, Turn: ap.Turn, Agent: ap.Agent,
				ToolID: ap.ToolID, ToolName: ap.ToolName, Command: ap.Command, Commands: ap.Commands,
				Rule: ap.Rule, Requested: ap.Requested.UTC()})
		}
	}
	writeJSON(w, http.StatusOK, HeldCalls{Approvals: out})
}

// POST /api/v0/approvals/{id}
func (a *API) decideApproval(w http.ResponseWriter, r *http.Request, id string) {
	var body Decision
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, `the body must be one JSON object with decision, "allow" or "refuse"`)
		return
	}
	var allow bool
	switch body.Decision {
	case "allow":
		allow = true
	case "refuse":
		allow = false
	default:
		writeError(w, http.StatusBadRequest, `decision must be "allow" or "refuse"`)
		return
	}
	if a.cfg.Approvals == nil {
		writeError(w, http.StatusNotFound, "no command by that id, or it was answered already")
		return
	}
	// Claim it first, so two answers at once (two tabs, a retry) do not both
	// move the settings or start two continuations.
	info, claimed, exists := a.cfg.Approvals.Claim(id)
	if !exists {
		writeError(w, http.StatusNotFound, "no command by that id, or it was answered already")
		return
	}
	if !claimed {
		writeError(w, http.StatusConflict, "that command is already being answered")
		return
	}
	// Allow from now on: do the work before recording the answer, so a failure
	// returns it to pending to try again rather than losing it.
	if allow {
		if err := a.allowCommands(info.Rules); err != nil {
			a.cfg.Approvals.Release(id)
			writeError(w, http.StatusInternalServerError, "the approval could not be written to the settings")
			return
		}
		if err := a.continueAfterApproval(info); err != nil {
			a.cfg.Approvals.Release(id)
			writeError(w, http.StatusInternalServerError, "the continuation turn could not be started")
			return
		}
	}
	a.cfg.Approvals.Decide(id, allow, "a person")
	w.WriteHeader(http.StatusNoContent)
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
// so the runner resumes the session's own workspace.
func (a *API) continueAfterApproval(info gateway.ApprovalInfo) error {
	if a.cfg.Queue == nil || info.Session == "" || info.Turn == "" {
		return nil
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, info.Turn)
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
		Session: info.Session, Model: src.Model, ModelFromAgent: src.ModelFromAgent,
		AgentModel: src.AgentModel, ReadOnly: src.ReadOnly,
		Prompt: "A person approved: you may now run " + strings.Join(info.Commands, ", ") + ". Run them and continue.",
	}
	links := queue.Links{ContinuesTurn: info.Turn, Chain: j.Links.Chain}
	_, _, err = a.cfg.Queue.CreateTurnLinked(queue.DefaultTeam, "", spec, links, queue.Limits{})
	var removed *queue.RunnerRemovedError
	if errors.Is(err, queue.ErrSessionClosed) || errors.As(err, &removed) {
		// The session cannot take another turn (closed, or its machine removed).
		// The commands are allowed from now on; there is just no turn to run.
		return nil
	}
	return err
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
