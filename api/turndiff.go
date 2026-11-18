package api

// A turn's diff summary and its apply: GET /api/v0/turns/{id}/diff and
// POST /api/v0/turns/{id}/apply. The summary is read from the record: the
// turn's turn.changed holds its file changes (listed at /changes), and its
// events its model calls (/calls) and tool calls (/tools); its turn.applied
// events hold its applies. A turn from before 0.5 has its changes and applies
// in its jobs' results. An apply is a job (queue.KindApply) that the runner
// holding the turn's session runs through its isolation backend.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// TurnDiff is the answer to GET /api/v0/turns/{id}/diff: the summary of what
// one turn did. Its lists each have their own paged route: the file changes
// at GET /api/v0/turns/{id}/changes, the model calls at .../calls, the tool
// calls at .../tools. No list is embedded here, so none is cut.
type TurnDiff struct {
	Turn       string        `json:"turn"`
	Session    string        `json:"session"`
	Agent      string        `json:"agent" doc:"The harness that ran the turn, from its turn.started."`
	AgentID    string        `json:"agent_id,omitempty" doc:"The saved agent that ran the turn, from its turn.started."`
	Model      string        `json:"model" doc:"The model the turn's last call asked for. Empty when it made no calls."`
	State      string        `json:"state" doc:"running, finished, failed or stopped, as GET /api/v0/turns/{id} says."`
	Started    time.Time     `json:"started"`
	Ended      *time.Time    `json:"ended,omitempty" doc:"Absent while running."`
	DurationMS *int64        `json:"duration_ms" doc:"From started to ended. Null while running."`
	Files      *FilesSummary `json:"files" doc:"What the turn changed, in summary: the whole list pages at GET /api/v0/turns/{id}/changes. Null when the record holds none: the turn is running, or this service did not run it."`
	ModelCalls CallsSummary  `json:"model_calls" doc:"The turn's model calls, in summary: the list pages at GET /api/v0/turns/{id}/calls."`
	Tools      ToolsSummary  `json:"tools" doc:"The turn's tool calls, in summary: the list pages at GET /api/v0/turns/{id}/tools, each tool call with the commands it ran."`
	Apply      ApplyState    `json:"apply" doc:"Whether the turn's changes were applied to the person's folder. state none when no apply was asked for."`
}

// FilesSummary is a turn's file changes in summary, from its turn.changed.
// The list is at GET /api/v0/turns/{id}/changes.
type FilesSummary struct {
	Isolated bool   `json:"isolated" doc:"The turn ran in an isolated copy, between two checkpoints: POST /api/v0/turns/{id}/apply can put its changes in the person's folder."`
	From     string `json:"from,omitempty" doc:"The checkpoint the turn started from. In an isolated copy only."`
	To       string `json:"to,omitempty" doc:"The checkpoint the turn ended with. The same as from: the turn changed nothing."`
	Kind     string `json:"kind" doc:"Always snapshot: the files were compared by their SHA-256."`
	Count    int    `json:"count" doc:"The files the turn added, modified or deleted."`
}

// FileChange is one changed file.
type FileChange struct {
	Path        string  `json:"path"`
	Change      string  `json:"change" doc:"added, modified or deleted."`
	Size        int64   `json:"size" doc:"The file at the end; for a deleted file, at the start."`
	SHA256      string  `json:"sha256" doc:"At the end; for a deleted file, at the start."`
	SHA256Prior string  `json:"sha256_before,omitempty" doc:"At the turn's start. Absent for an added file, and for turns from before 0.5."`
	Diff        *string `json:"diff,omitempty" doc:"The file's unified diff, from the turn's start to its end, for small text files whose earlier content is kept."`
	DiffLeftOut bool    `json:"diff_left_out,omitempty" doc:"The content changed and no diff is here: a binary file, a link, a file over 256 KiB, past the 1 MiB of all diffs (512 KiB before 0.5), or its earlier content is no longer kept."`
	Content     *string `json:"content,omitempty" doc:"The whole file at the end, when it is small UTF-8 text."`
}

// CallsSummary is a turn's model calls in summary. The list is at
// GET /api/v0/turns/{id}/calls.
type CallsSummary struct {
	Count        int          `json:"count" doc:"The turn's model.requested events."`
	Usage        *event.Usage `json:"usage" doc:"Summed over the replies that carried usage. Null when none did."`
	CostUSD      float64      `json:"cost_usd"`
	CostComplete bool         `json:"cost_complete" doc:"False when a reply lacks a complete cost."`
	Models       []ModelCount `json:"models" doc:"Calls per model, in the order each model was first asked for."`
}

// ModelCount is how many calls asked for one model.
type ModelCount struct {
	Model string `json:"model"`
	Calls int    `json:"calls"`
}

// ModelCall is one model call of a turn.
type ModelCall struct {
	Exchange  string       `json:"exchange" doc:"GET /api/v0/turns/{id}/calls/{exchange} shows what the call sent."`
	Model     string       `json:"model"`
	Status    int          `json:"status,omitempty" doc:"The HTTP status of the vendor's reply. Absent until it came."`
	Usage     *event.Usage `json:"usage,omitempty"`
	CostUSD   *float64     `json:"cost_usd,omitempty"`
	LatencyMS *int64       `json:"latency_ms,omitempty"`
}

// ToolsSummary is a turn's tool calls in summary. The list, each tool call
// with the commands it ran, is at GET /api/v0/turns/{id}/tools.
type ToolsSummary struct {
	Count int `json:"count" doc:"The turn's tool.requested events."`
}

// ToolCall is one tool call of a turn.
type ToolCall struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input,omitempty" doc:"The tool's input as the model sent it, when it is at most 4 KiB."`
	InputCut    bool            `json:"input_left_out,omitempty" doc:"The input is over 4 KiB: GET /api/v0/sessions/{id}/events?turn={turn} has it whole."`
	Commands    []string        `json:"commands,omitempty" doc:"The shell commands the call runs, when it runs any."`
	Error       *bool           `json:"error,omitempty" doc:"From the call's tool.completed. Absent until it came, or when the vendor did not say."`
	RefusedRule string          `json:"refused_rule,omitempty" doc:"The forbid entry the gateway refused the call by."`
}

// ApplyState is an apply of a turn: the answer to POST /api/v0/turns/{id}/apply,
// and apply in GET /api/v0/turns/{id}/diff.
type ApplyState struct {
	State     string                `json:"state" doc:"none (never asked for), queued, running, applied, refused (a file in your folder changed, or the turn cannot be applied: error says why), or failed (the apply did not finish)."`
	Turn      string                `json:"turn,omitempty"`
	Session   string                `json:"session,omitempty"`
	Turns     []string              `json:"turns,omitempty" doc:"Every turn whose changes the apply carried, oldest first: the session's turns after its previous apply, up to turn. Absent for an apply from before 0.5."`
	By        *event.By             `json:"by,omitempty" doc:"Who asked for the apply: the person or an agent. Absent for an apply from before 0.5."`
	Already   bool                  `json:"already,omitempty" doc:"The turn was applied before this request: nothing more was written, and files are what that apply wrote."`
	Files     []string              `json:"files,omitempty" doc:"The files the apply wrote or removed in the person's folder, sorted."`
	Count     int                   `json:"count,omitempty" doc:"How many files the apply wrote or removed. files may list fewer, with truncated."`
	Truncated bool                  `json:"truncated,omitempty" doc:"files or conflicts was cut to keep the answer small."`
	Pruned    bool                  `json:"pruned,omitempty" doc:"The checkpoints before the turn's end were removed."`
	Conflicts []queue.ApplyConflict `json:"conflicts,omitempty" doc:"On a refusal: each file in your folder that is neither as it was at the turn's start nor as the turn left it, with why. Nothing was written."`
	Error     string                `json:"error,omitempty" doc:"Why it was refused or failed."`
}

// maxToolInput: a tool call's input is left out past this, with input_left_out.
const maxToolInput = 4 << 10

// activityPageLimit is the events a page of buildActivity reads, 1000 in
// production. A test lowers it to reach many pages with few events, since
// buildActivity now reads every page, with no cap on their number.
var activityPageLimit = 1000

// GET /api/v0/turns/<id>/diff: one turn's summary; its lists page elsewhere.
func (a *API) turnDiff(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	d := TurnDiff{Turn: t.ID, Session: t.Session, Agent: t.Harness, AgentID: t.AgentID, Model: t.Model, State: t.State,
		Started: t.Started, Ended: t.Ended, Apply: ApplyState{State: "none"},
		ModelCalls: CallsSummary{Count: t.ModelCalls, Usage: t.Usage, CostUSD: t.CostUSD, CostComplete: t.CostComplete,
			Models: []ModelCount{}}}
	if t.Ended != nil {
		ms := t.Ended.Sub(t.Started).Milliseconds()
		d.DurationMS = &ms
	}
	if a.cfg.Queue != nil {
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		switch {
		case err == nil:
			res, _ := queue.ReadResult(j.Result)
			if j.Session == t.Session || res.Session == t.Session {
				if j.Ended != nil {
					c, err := jobs.Changes(a.cfg.Store, j)
					if err != nil {
						a.internal(w)
						return
					}
					d.Files = filesSummary(c)
				}
				if j.Ended == nil && d.State != queue.TurnStopped {
					// As GET /api/v0/turns/{id}: running until its job has
					// ended, unless it was stopped.
					d.State, d.Ended, d.DurationMS = queue.TurnRunning, nil, nil
				}
				if st, err := a.applyState(id); err != nil {
					a.internal(w)
					return
				} else if st != nil {
					d.Apply = *st
				}
			}
		case !errors.Is(err, queue.ErrNotFound):
			a.internal(w)
			return
		}
	}
	act, err := a.buildActivity(d.Turn, d.Session)
	if err != nil {
		a.internal(w)
		return
	}
	d.ModelCalls.Models = act.Models
	d.Tools.Count = len(act.Tools)
	writeJSON(w, http.StatusOK, d)
}

// filesSummary is a turn's file changes in summary, for the diff. nil for nil.
func filesSummary(c *event.TurnChanges) *FilesSummary {
	if c == nil {
		return nil
	}
	return &FilesSummary{Isolated: c.Isolated, From: c.From, To: c.To, Kind: workspace.KindSnapshot, Count: c.Count}
}

// fileChanges is a turn's changes as the changes route lists them: the full
// list and the record's completeness flags. nil for nil.
func fileChanges(c *event.TurnChanges) ([]FileChange, bool, bool, bool, string) {
	if c == nil {
		return nil, false, false, false, ""
	}
	out := make([]FileChange, len(c.Files))
	for i, ch := range c.Files {
		out[i] = FileChange{Path: ch.Path, Change: ch.Change, Size: ch.Size, SHA256: ch.SHA256, SHA256Prior: ch.SHA256Before,
			Diff: ch.Diff, DiffLeftOut: ch.DiffLeftOut, Content: ch.Content}
	}
	return out, c.Truncated, c.DiffsCut, c.ContentLeftOut, c.Error
}

// activity is a turn's model calls, tool calls and per-model counts, read
// whole from its events: no cap, so a list route pages them and the summary
// counts them. Models is an aggregate; Calls and Tools are the full lists,
// oldest first (each tool call carries its own commands).
type activity struct {
	Models []ModelCount
	Calls  []ModelCall
	Tools  []ToolCall
}

// buildActivity reads a turn's model calls, tool calls and commands from its
// events, every one: it pages to the end, so a later call after many tool
// events is counted and listed, never dropped by a page cap.
func (a *API) buildActivity(turn, session string) (*activity, error) {
	f := store.Filter{Turn: turn, TurnSession: session}
	out := &activity{Models: []ModelCount{}, Calls: []ModelCall{}, Tools: []ToolCall{}}
	callIdx := map[string]int{} // exchange -> index in Calls
	models := map[string]int{}
	toolIdx := map[string]int{} // tool id -> index in Tools
	// The gateway stores a call's tool.refused before its tool.requested,
	// so a refusal waits here for its call.
	refused := map[string]string{}
	var after int64
	for {
		recs, more, err := a.cfg.Store.Page(after, f, activityPageLimit, store.PageBytes)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			after = rec.Seq
			switch rec.Kind {
			case event.ModelRequested, event.ModelResponded, event.ToolRequested, event.ToolCompleted, event.ToolRefused:
			default:
				continue
			}
			var e event.Event
			if json.Unmarshal(rec.Data, &e) != nil {
				continue
			}
			switch e.Kind {
			case event.ModelRequested:
				if i, ok := models[e.Model]; ok {
					out.Models[i].Calls++
				} else {
					models[e.Model] = len(out.Models)
					out.Models = append(out.Models, ModelCount{Model: e.Model, Calls: 1})
				}
				callIdx[e.Exchange] = len(out.Calls)
				out.Calls = append(out.Calls, ModelCall{Exchange: e.Exchange, Model: e.Model})
			case event.ModelResponded:
				if i, ok := callIdx[e.Exchange]; ok {
					c := &out.Calls[i]
					c.Status, c.Usage, c.CostUSD, c.LatencyMS = e.Status, e.Usage, e.CostUSD, e.LatencyMS
				}
			case event.ToolRequested:
				cmds := gateway.ToolCommands(e.Vendor, e.ToolName, e.ToolInput)
				tc := ToolCall{ID: e.ToolID, Name: e.ToolName, Commands: cmds, RefusedRule: refused[e.ToolID]}
				if len(e.ToolInput) <= maxToolInput {
					tc.Input = e.ToolInput
				} else {
					tc.InputCut = true
				}
				toolIdx[e.ToolID] = len(out.Tools)
				out.Tools = append(out.Tools, tc)
			case event.ToolCompleted:
				if i, ok := toolIdx[e.ToolID]; ok && e.ToolError != nil {
					out.Tools[i].Error = e.ToolError
				}
			case event.ToolRefused:
				if i, ok := toolIdx[e.ToolID]; ok {
					out.Tools[i].RefusedRule = e.Rule
				} else {
					refused[e.ToolID] = e.Rule
				}
			}
		}
		if !more {
			break
		}
	}
	return out, nil
}

// Refusals of an apply. Fixed wording.
const (
	applyNotApplicable = "this turn's changes cannot be applied: it did not run in an isolated copy of a folder"
	applyNotEnded      = "this turn has not ended; apply it once it has"
	applyNeverStarted  = "this turn ended before it started, so it has no changes to apply"
	applySessionClosed = "this turn's session was closed, so its copy and checkpoints are gone"
	applyNoJob         = "no turn of this service has this id"
	applyBusy          = "an apply of an earlier turn of this session has not ended; ask again once it has"
	applyOffline       = "the folder's machine is offline, so nothing was written: start agentbus runner there, then ask again"
)

// POST /api/v0/turns/<id>/apply: put the turn's changes into the person's
// folder, through the runner that holds its session.
func (a *API) applyTurn(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, created, err := a.cfg.Queue.ApplyAs(queue.DefaultTeam, id, askedBy(r))
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, applyNoJob)
		return
	case errors.Is(err, queue.ErrNotApplicable):
		writeError(w, http.StatusBadRequest, a.whyNotApplicable(id))
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusBadRequest, applySessionClosed)
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeError(w, http.StatusConflict, applyBusy)
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, applyOffline)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	already := !created && j.Ended != nil
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st, err := a.applyStateOf(j)
	if err != nil {
		a.internal(w)
		return
	}
	st.Already = already
	switch st.State {
	case "applied":
		writeJSON(w, http.StatusOK, st)
	case "queued", "running":
		writeJSON(w, http.StatusAccepted, st)
	default:
		writeJSON(w, http.StatusConflict, st)
	}
}

// whyNotApplicable says why turn id's changes cannot be applied: it has not
// ended, it never started, or it did not run in a copy.
func (a *API) whyNotApplicable(id string) string {
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	switch {
	case err != nil || j.Kind != queue.KindTurn:
	case j.Ended == nil:
		return applyNotEnded
	case !started(j):
		return applyNeverStarted
	}
	return applyNotApplicable
}

// awaitEnd waits for job to end, up to TurnWait. It answers the job as it
// is then.
func (a *API) awaitEnd(ctx context.Context, job queue.Job) (queue.Job, error) {
	wait := a.cfg.TurnWait
	if wait == 0 {
		wait = DefaultTurnWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil || j.Ended != nil {
			return j, err
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-deadline.C:
			return j, nil
		case <-tick.C:
		}
	}
}

// applyState is the apply that put turn's changes in the folder, its own or
// a later turn's, else the newest apply of turn; nil when none was asked for.
func (a *API) applyState(turn string) (*ApplyState, error) {
	j, ok, err := a.cfg.Queue.AppliedBy(queue.DefaultTeam, turn)
	if err != nil || !ok {
		return nil, err
	}
	st, err := a.applyStateOf(j)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// applyStateOf is an apply job as the API shows it: its state from the
// job, and once it has ended, what it did from its turn.applied.
func (a *API) applyStateOf(j queue.Job) (ApplyState, error) {
	st := ApplyState{Turn: j.Links.AppliesTurn, Session: j.Session, Turns: j.Links.AppliesTurns}
	if b := j.Links.ApplyBy; b != nil {
		st.By = &event.By{Kind: b.Kind, Session: b.Session, Turn: b.Turn}
	}
	switch {
	case j.State == queue.Queued:
		st.State = "queued"
		return st, nil
	case j.Ended == nil:
		st.State = "running"
		return st, nil
	}
	ap, err := jobs.Applied(a.cfg.Store, j)
	if err != nil {
		return st, err
	}
	st.State, st.Error, st.Turns, st.By = ap.Result, ap.Error, ap.Turns, ap.By
	if len(j.Links.AppliesTurns) == 0 {
		st.Turns = nil // an apply from before 0.5 never named them
	}
	st.Files, st.Count, st.Truncated, st.Pruned = ap.Files, ap.Count, ap.Truncated, ap.Pruned
	for _, c := range ap.Conflicts {
		st.Conflicts = append(st.Conflicts, queue.ApplyConflict{Path: c.Path, Why: c.Why})
	}
	return st, nil
}

// askedBy is who sent request r, as an apply's turn.applied names it: an
// agent (a turn's token, with its session and turn), else the person (the
// main token). Never the token.
func askedBy(r *http.Request) *queue.By {
	if g := grantOf(r); g != nil {
		return &queue.By{Kind: event.ByAgent, Session: g.Session, Turn: g.Turn}
	}
	return &queue.By{Kind: event.ByPerson}
}
