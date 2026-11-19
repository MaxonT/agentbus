package api

// POST /api/v0/turns: a program starts a turn, an agent with a prompt. Every
// turn runs as a job: this makes a turn job in the queue and answers once a
// runner has started its turn (package runner, which starts it through
// package run). The reply keeps its shape: the session, the turn and its base
// URL. With session in the body, the same endpoint starts that session's next
// turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/staysup/agentbus/agents"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// MaxRunBody bounds a turn's request body. A 256 KiB prompt can take six
// bytes per byte once JSON-escaped.
const MaxRunBody = 2 << 20

// MaxPrompt bounds a turn's prompt, in bytes.
const MaxPrompt = run.MaxPrompt

// DefaultTurnWait bounds how long POST /api/v0/turns waits for a runner to
// start the turn, when Config.TurnWait is zero.
const DefaultTurnWait = 30 * time.Second

var runModel = run.ModelShape

const badRun = "the body must be one JSON object with prompt, agent and folder (or session, and optionally agent), and optionally model, isolate and idempotency_key, at most 2 MiB"

// TurnRequest is the body of POST /api/v0/turns. omitempty marks a field as
// optional in the API's description.
type TurnRequest struct {
	Session   string `json:"session,omitempty" doc:"A session the snapshot calls resumable: this turn is its next one. Absent: a new session."`
	Agent     string `json:"agent,omitempty" doc:"A saved agent's id, from GET /api/v0/agents: its harness, access and model run the turn. A removed agent, a default one (claude, codex, agy) too, is refused: pick another. Required on every turn, a session's next one too (400 without it): a session keeps no agent of its own. The agent is read as it is when the turn is asked for, and its model is the turn's unless model is sent. A removed agent is refused: pick another. With session the turn runs in the session's folder. On the same harness as the latest turn the conversation continues by own replay; on another harness the turn gets the earlier turns as plain text (continuity transcript)."`
	Folder    string `json:"folder,omitempty" doc:"An absolute path to a directory on the machine named by machine, that the user its runner runs as can read. Typing it is the grant for this session: that machine's runner checks it and registers it (agentbus up's own; on agentbus server the machine named by machine, or the only one online), and the session runs on that machine for its whole life. The queue keeps only the folder's reference, and GET /api/v0/folders lists the folder from then on. Without session, folder or folder_ref is required. With session it is the session's own; if sent it must match."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference, fld_ and 16 hex digits, from GET /api/v0/folders: the turn runs in that folder, on the runner of the machine that holds it. For a new session, instead of folder."`
	Machine   string `json:"machine,omitempty" doc:"The machine the session runs on (con_ and 16 hex digits): a runner's machine in GET /api/v0/runners, or a folder's in GET /api/v0/folders. Only the runner on that machine takes the session's work. With folder_ref: the machine that holds the folder. With folder: the machine whose runner checks and registers it. Absent: this service's own machine (agentbus up), or else the only one online; with several online, it is required for folder."`
	// compat 0.6: drop connector. A client of a build before 0.6 sends the
	// machine under the old key; decode folds it into Machine when Machine
	// is absent. Removed next release.
	Connector string `json:"connector,omitempty" doc:"Deprecated alias of machine, read for one release. Send machine."`
	Isolate   *bool  `json:"isolate,omitempty" doc:"True unless false, for a new session. True: the session runs in its own copy of folder, exactly as folder is now, uncommitted edits included, made as the isolation setting says (auto, overlay, copy, git or none). False: always folder as it is. Not sent with session."`
	Prompt    string `json:"prompt" doc:"From 1 byte to 256 KiB, no NUL byte. Always passed after --."`
	Model     string `json:"model,omitempty" doc:"Passed only as the value of --model, for this turn only. Absent: the named agent's model, as it is when the turn is asked for; an agent with none runs on its harness's own default."`
	// compat 0.6: read-only turns retired (B12). A client of a build before
	// 0.6 may still send read_only; it is read and dropped, so the request is
	// not refused. A review is a normal turn now, in its own copy, and never
	// applied automatically. Removed next release.
	ReadOnly *bool `json:"read_only,omitempty" doc:"Deprecated and ignored: read-only turns were retired in 0.6. A review runs like any turn, in its own copy of the folder, and its changes are never applied automatically. Accepted for one release so an older client is not refused."`
	// IdempotencyKey makes a retry safe: a client whose request timed out
	// sends it again with the same key and gets the same turn.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key and the same fields answers 202 with the turn the key first started, existing true, and starts nothing. The same key with other fields gets 409. A request refused with 400 used no key."`
	// Delivery records the on for the turn's reply in the SAME write as the turn
	// (his call (a), 2026-11-19), so starting and delivering are one atomic step.
	Delivery string `json:"delivery,omitempty" doc:"A session to deliver this turn's reply to when it completes, recorded as a turn.completed on in the same transaction that starts the turn (the same as POST /api/v0/ons, but atomic with the start, so no separate call can be lost). A turn token may name only its own session, so it sends its own; the main token may name any. Absent: no delivery is registered. The turn's idempotency_key covers the on too, so a retry adds no second."`
}

// Started is the answer to a turn's start, and to a review's.
type Started struct {
	Session string `json:"session" doc:"The turn's session: a new one, or the session sent. A review's is a session of its own."`
	Turn    string `json:"turn" doc:"The turn's id. Every event of the turn carries it in turn; GET /api/v0/turns/{id} follows it from now on."`
	State   string `json:"state" doc:"running: a runner started it. queued: it waits behind the session's earlier turns (ahead says how many), or for a runner. It runs next, in order."`
	BaseURL string `json:"base_url,omitempty" doc:"The address the agent uses, <service>/s/<session>/t/<turn>. Present only when this service runs the turn itself (agentbus up). Absent while queued, and on a server, where the turn runs on another machine's runner and that runner serves its gateway."`
	// Existing marks the answer to a retry: the idempotency key had already
	// made this turn.
	Existing bool `json:"existing,omitempty" doc:"True when idempotency_key had already made this turn, so nothing new was made."`
	Ahead    int  `json:"ahead,omitempty" doc:"With state queued: how many turns of the session, running or waiting, are before this one. Absent when the turn waits only for a runner."`
}

// idempotencyKey is the shape of any idempotency_key a request sends. It
// excludes '/', which is how the internal key namespaces (delivery/, result/,
// apply/) stay unreachable by a caller — so every route that accepts a key must
// check it with checkIdempotencyKey, not just this one (his call, 2026-11-19:
// one rule, one function, after a key that could hold '/' at POST /ons let a
// caller's on collide with a turn's delivery on).
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// checkIdempotencyKey is the one validator every route with an idempotency_key
// shares. It returns "" for a valid key (including an empty one, which means no
// key) or the message to refuse with. It reads only the incoming value; keys
// already stored are never re-checked, so this changes no existing row.
func checkIdempotencyKey(key string) string {
	if key != "" && !idempotencyKey.MatchString(key) {
		return "idempotency_key must match " + idempotencyKey.String()
	}
	return ""
}

func (a *API) run(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var body TurnRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	// compat 0.6: drop connector. A client before 0.6 names the machine
	// under the old key; prefer machine, then read it. Code below reads
	// Machine only.
	if body.Machine == "" {
		body.Machine = body.Connector
	}
	body.Connector = ""
	// compat 0.6: read_only is retired (B12); read and dropped so an older
	// client's request is not refused. The turn runs as any turn does.
	// TODO(B12 P2): a job created before 0.6 with read_only:true stored a
	// request hash that included it; an identical retry now hashes without it
	// and gets a key conflict instead of the existing turn. This only bites a
	// retry that spans the upgrade; a new key, or a fresh request, is unaffected.
	body.ReadOnly = nil
	// Only a body without session starts a new session. A session sent as
	// "" or null is refused, never read as absent: that would start a new
	// session in place of the one the caller meant.
	var sent struct {
		Session json.RawMessage `json:"session"`
	}
	if json.Unmarshal(data, &sent) != nil {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// wait is validated here, before any job is created: an invalid value must
	// refuse without queuing a turn or consuming its idempotency key (found in
	// review 2026-11-19). noWait returns the turn once recorded; see below.
	noWait := false
	if v := r.URL.Query().Get("wait"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "wait must be true or false")
			return
		}
		noWait = !b
	}
	// A turn token asks another agent: its checks (tokens.go) fill the new
	// session's folder, and the job names the asking turn.
	var links queue.Links
	if g := grantOf(r); g != nil {
		var ok bool
		if links, ok = a.askChecks(w, *g, policyOf(r), &body, sent.Session != nil); !ok {
			return
		}
	}
	spec := queue.Spec{Prompt: body.Prompt, Model: body.Model}
	if sent.Session != nil {
		if !sessionID.MatchString(body.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
		if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		// The session decides its folder. One sent that differs is refused,
		// never quietly replaced. The record of this service knows the
		// sessions its own runner ran. An agent sent runs this turn: a switch.
		c, err := a.cfg.Store.Continuation(body.Session)
		if err != nil {
			a.internal(w)
			return
		}
		if body.Isolate != nil {
			writeError(w, http.StatusBadRequest, "isolate is for a new session; a session keeps its own copy")
			return
		}
		if body.FolderRef != "" || body.Machine != "" {
			writeError(w, http.StatusBadRequest, "folder_ref and machine are for a new session; a session keeps its own folder")
			return
		}
		// Every turn names its agent, a next one too (his rule,
		// 2026-11-12): it is read here, once, as it is now, and the turn's
		// tool and its credential both come from this one answer.
		if body.Agent == "" {
			writeError(w, http.StatusBadRequest, run.NoAgent)
			return
		}
		ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
		if !ok {
			return
		}
		spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if spec.Model == "" {
			spec.AgentModel = ag.Model
		}
		if c.Known && body.Folder != "" && filepath.Clean(body.Folder) != filepath.Clean(c.Folder) {
			writeError(w, http.StatusBadRequest, "folder is not this session's folder "+strconv.Quote(c.Folder)+
				"; leave folder out to use the session's")
			return
		}
		// A session whose turn runs takes the next one too: its job waits in
		// the queue behind the turns before it and runs next, in order. A
		// session the record shows open with no job of this queue behind it
		// (a turn this service did not start) cannot be waited for, so it is
		// refused, before the key is used; a retry of a known key goes on to
		// the queue, which returns its job.
		if c.Open {
			busy, err := a.cfg.Queue.SessionHasWork(queue.DefaultTeam, body.Session)
			if err != nil {
				a.internal(w)
				return
			}
			known, err := a.cfg.Queue.HasKey(queue.DefaultTeam, body.IdempotencyKey)
			if err != nil {
				a.internal(w)
				return
			}
			if !busy && !known {
				writeError(w, http.StatusConflict, run.TurnRunning)
				return
			}
		}
		spec.Session = body.Session
	} else {
		ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
		if !ok {
			return
		}
		if body.FolderRef == "" && (len(body.Folder) > run.MaxFolder || !filepath.IsAbs(body.Folder)) {
			writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
			return
		}
		if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if spec.Model == "" && ag.Model != "" {
			spec.Model, spec.ModelFromAgent = ag.Model, true
		}
		spec.Folder, spec.FolderRef, spec.Machine = body.Folder, body.FolderRef, body.Machine
		spec.Isolate = body.Isolate == nil || *body.Isolate
		if body.Folder != "" && body.FolderRef == "" {
			// A typed folder: its machine's runner checks and registers it, and the
			// session goes on by its reference. A retry of a key already used
			// is not checked again: the queue answers with the key's turn, or
			// 409 when the request differs.
			spec.Typed, spec.TypedMachine = body.Folder, body.Machine
			known := false
			if body.IdempotencyKey != "" {
				if known, err = a.cfg.Queue.HasKey(queue.DefaultTeam, body.IdempotencyKey); err != nil {
					a.internal(w)
					return
				}
			}
			ref, conn := "", ""
			if !known {
				var status int
				var msg string
				ref, conn, status, msg = a.typedFolder(r.Context(), body.Folder, body.Machine)
				switch {
				case status == 0 && msg != "":
					return // the caller went away
				case status == http.StatusInternalServerError:
					a.internal(w)
					return
				case msg != "":
					writeError(w, status, msg)
					return
				}
			}
			spec.Folder, spec.FolderRef, spec.Machine = "", ref, conn
		}
		if msg := queue.ResolveFolders(&spec, a.cfg.Folders); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	// Atomic delivery (his call (a), 2026-11-19): if delivery is set, record the
	// on in the SAME write as the turn, under the one authorization that starts
	// it — the same rules POST /api/v0/ons applies, but no separate call that a
	// stop or a lost token can drop. A turn token delivers only to its own
	// session (the new turn is already in its tree, so the on is within its
	// reach); the main token may name any session. The turn's key covers the on.
	var deliver *queue.Delivery
	if body.Delivery != "" {
		if g := grantOf(r); g != nil {
			if body.Delivery != g.Session {
				writeError(w, http.StatusForbidden, "a turn token may deliver only to its own session")
				return
			}
			deliver = &queue.Delivery{Session: g.Session, Maker: queue.Maker{Session: g.Session, Turn: g.Turn}}
		} else {
			deliver = &queue.Delivery{Session: body.Delivery}
		}
	}
	// Never refused for a busy session: the job waits behind the session's turns
	// that have not ended, as a delivered result does.
	job, created, err := a.cfg.Queue.CreateTurnLinked(queue.DefaultTeam, body.IdempotencyKey, spec, links, queueLimits(policyOf(r)), deliver)
	var limit *queue.LimitError
	var removed *queue.RunnerRemovedError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusConflict, removed.Error())
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key already started a turn with other fields; send a new key for a new turn")
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusConflict, "this session is closed; it takes no more turns")
		return
	case errors.Is(err, queue.ErrSessionEnded):
		writeError(w, http.StatusConflict, queue.SessionEnded)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns are waiting; wait for some to end and retry")
		return
	case err != nil:
		a.internal(w)
		return
	}
	// async by default (his rule, 2026-11-19): wait=false returns once the turn
	// is recorded, naming it, without waiting for a runner. The ask_agent and
	// review_turn apps send it, so startTurn never blocks and registerOn runs
	// next in the same tool call, shrinking the gap between the two steps to that
	// one dispatch. Absent or true keeps the 30s wait.
	if noWait {
		// Report the turn's real state, not a hardcoded "queued": a retry whose
		// turn was stopped or ended before it started must say so, not pretend it
		// is still waiting to run (found in review 2026-11-19). This is awaitTurn's
		// own first check, without the wait.
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil {
			a.internal(w)
			return
		}
		turn := startedTurn(j)
		switch {
		case turn != "":
			writeJSON(w, http.StatusAccepted, a.startedRunning(j, turn, !created))
		case j.Ended != nil:
			a.notStarted(w, j)
		default:
			writeJSON(w, http.StatusAccepted, Started{Session: j.Session, Turn: j.Turn, State: queue.TurnQueued, Existing: !created})
		}
		return
	}
	a.awaitTurn(w, r.Context(), job, !created)
}

// turnAgent is the saved agent a turn names, or false once it has answered
// why the turn cannot start. A retry of a request whose agent was removed
// since gets a stand-in: the queue matches the key against the request as
// asked, the agent by its id, and answers with the first job.
func (a *API) turnAgent(w http.ResponseWriter, id, key string) (agents.Agent, bool) {
	var retried func() (bool, error)
	if key != "" && sessionID.MatchString(id) {
		retried = func() (bool, error) { return a.cfg.Queue.HasKey(queue.DefaultTeam, key) }
	}
	ag, why, err := agents.Lookup(id, a.resolveAgent, retried)
	switch {
	case err != nil:
		a.internal(w)
		return agents.Agent{}, false
	case why != nil:
		writeError(w, http.StatusBadRequest, why.Error())
		return agents.Agent{}, false
	}
	return ag, true
}

// resolveAgent is the saved agent a turn names, or why it cannot start, in
// fixed words fit for the caller. Without saved agents only claude and codex
// are known, each on its own subscription.
func (a *API) resolveAgent(id string) (agents.Agent, error) {
	if a.cfg.Agents == nil {
		if !run.KnownHarness(id) {
			return agents.Agent{}, errors.New("agent must be " + harness.NameList())
		}
		return agents.Agent{Harness: id, Access: agents.Subscription}, nil
	}
	if !sessionID.MatchString(id) {
		return agents.Agent{}, errors.New("agent must be a saved agent's id; GET /api/v0/agents lists them")
	}
	ag, err := a.cfg.Agents.Resolve(id)
	if errors.Is(err, agents.ErrRemoved) {
		return agents.Agent{}, errors.New("agent " + strconv.Quote(id) + " was removed; pick another (GET /api/v0/agents lists them)")
	}
	if err != nil {
		return agents.Agent{}, errors.New("agent " + strconv.Quote(id) + " is not a saved agent; GET /api/v0/agents lists them")
	}
	return ag, nil
}

const noTurns = "this service starts no turns: it keeps no queue"

// startedRunning is the Started for a turn a runner has started: its running
// state, and the gateway BaseURL when this service runs the turn itself
// (agentbus up). Both the waiting path and the wait=false path build their
// running reply through it, so they cannot diverge on the gateway address
// (found in review 2026-11-19: wait=false had omitted BaseURL).
func (a *API) startedRunning(j queue.Job, turn string, existing bool) Started {
	st := Started{Session: j.Session, Turn: turn, State: queue.TurnRunning, Existing: existing}
	// BaseURL is the turn's gateway, which only this service serves, and only
	// when it runs turns itself. A server runs no turns of its own: the gateway
	// is on the runner that ran it, a machine this service does not serve, so
	// BaseURL is left out rather than pointing at control.
	if a.cfg.Engine != nil {
		st.BaseURL = a.cfg.BaseURL + "/s/" + j.Session + "/t/" + turn
	}
	return st
}

// awaitTurn answers once the job's turn has started: 202 with the turn. A job
// that ended before its turn started answers why, with the status the runner
// was refused with. A job no runner took within TurnWait answers 202 with
// queued: it stays queued. A job that waits behind its session's earlier
// turns answers queued at once, with how many are ahead.
func (a *API) awaitTurn(w http.ResponseWriter, ctx context.Context, job queue.Job, existing bool) {
	wait := a.cfg.TurnWait
	if wait == 0 {
		wait = DefaultTurnWait
	}
	if job.State == queue.Queued && job.Session != "" {
		ahead, err := a.turnsAhead(job.ID)
		if err != nil {
			a.internal(w)
			return
		}
		if ahead > 0 {
			writeJSON(w, http.StatusAccepted, Started{Session: job.Session, Turn: job.Turn, State: queue.TurnQueued, Existing: existing, Ahead: ahead})
			return
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil {
			a.internal(w)
			return
		}
		if turn := startedTurn(j); turn != "" {
			writeJSON(w, http.StatusAccepted, a.startedRunning(j, turn, existing))
			return
		}
		if j.Ended != nil {
			a.notStarted(w, j)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			writeJSON(w, http.StatusAccepted, Started{Session: j.Session, Turn: j.Turn, State: queue.TurnQueued, Existing: existing})
			return
		case <-tick.C:
		}
	}
}

// turnsAhead counts the turns the job waits behind: its session's earlier
// turn jobs that have not ended, less those whose turn the record shows
// ended, which are only finishing and free the session in a moment.
func (a *API) turnsAhead(job string) (int, error) {
	jobs, err := a.cfg.Queue.TurnsAhead(queue.DefaultTeam, job)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if turn := startedTurn(j); turn != "" && a.turnEnded(turn) {
			continue
		}
		n++
	}
	return n, nil
}

// startedTurn is the job's turn once an attempt has started it, else "".
func startedTurn(j queue.Job) string {
	for _, at := range j.Attempts {
		if at.TurnStarted {
			return j.Turn
		}
	}
	return ""
}

// notStarted answers for a job that ended before its turn started: the
// refusal the runner recorded, with its status.
func (a *API) notStarted(w http.ResponseWriter, j queue.Job) {
	res, _ := queue.ReadResult(j.Result)
	switch {
	case j.State == queue.Failed && j.Reason == queue.ReasonSetup && res.Refused != "":
		status := res.Status
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeError(w, status, res.Refused)
	case j.State == queue.Cancelled:
		writeError(w, http.StatusConflict, "the turn was stopped before it started")
	default:
		writeError(w, http.StatusInternalServerError, "the turn ended before it started: "+queue.TurnState(j.State)+" "+j.Reason)
	}
}

// turnEnded says the record shows the turn ended: its job may still be
// finishing. A turn this record does not hold, as on a server whose runner
// keeps its own, is not known to have ended.
func (a *API) turnEnded(id string) bool {
	list, err := a.cfg.Store.Turns(store.TurnQuery{Turn: id, Limit: 1})
	return err == nil && len(list.Turns) == 1 && list.Turns[0].State != store.TurnRunning
}

// Stopping is the answer to a stop.
type Stopping struct {
	Session string `json:"session" doc:"The turn's session."`
	Turn    string `json:"turn" doc:"The turn being stopped. It reads stopped once its process tree has ended; a queued turn at once."`
}

const (
	turnNotRunning = "this turn has ended, or it is not a turn of this service"
	noTurn         = "no such turn"
)

// POST /api/v0/turns/<id>/stop: a stop cancels the turn's job. A queued one
// ends at once; a running one's runner hears it on its next heartbeat and
// stops the turn's whole process tree.
func (a *API) stopTurn(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	switch {
	case err == nil:
		if j.Ended != nil || (started(j) && a.turnEnded(id)) {
			writeError(w, http.StatusConflict, turnNotRunning)
			return
		}
		if _, err := a.cfg.Queue.Cancel(queue.DefaultTeam, j.ID); err != nil {
			a.internal(w)
			return
		}
		writeJSON(w, http.StatusAccepted, Stopping{turnSession(j), id})
		return
	case !errors.Is(err, queue.ErrNotFound):
		a.internal(w)
		return
	}
	recs, err := a.cfg.Store.After(0, store.Filter{Turn: id}, 1)
	if err != nil {
		a.internal(w)
		return
	}
	if len(recs) == 0 {
		writeError(w, http.StatusNotFound, noTurn)
		return
	}
	writeError(w, http.StatusConflict, turnNotRunning)
}
