package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL. With it the
// agent reads its session's history and asks other agents: it starts turns
// on any saved agent, each in a new session, asks for reviews, and reads
// their results. It never continues a session (his call, 2026-11-12). Each result comes back to its session as a later
// turn, delivered by an on row the ask path registers (queue/on.go, FireOns).
//
// What a turn token must never do: read or change the settings (the policy
// and the instructions: every /api/v0/settings route is main-token only),
// change or remove saved agents, see or make tokens (none is
// ever in a reply), close sessions, write program events, or read or write
// sessions outside its request tree. Its own session and the person's session,
// the tree's root, are never continued by it: results reach them by delivery
// only.
//
// A request tree is the person's session (its root) and every session work
// asked for from it, at any depth (queue.Chain). route.turn says what a turn
// token may do on each route; the zero value refuses it.
//
// The engine mints and keeps the tokens (package run), in memory only, as
// their SHA-256, and revokes each when its turn's child exits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// What a turn token may do on a route.
const (
	// mainOnly: a turn token is refused. The zero value, so a new route is
	// closed to turn tokens until it says otherwise.
	mainOnly = iota
	// treeSessionPath: {id} is a session id in the token's request tree.
	treeSessionPath
	// treeSessionQuery: the session query must name a session in the
	// token's request tree. Absent, it is set to the token's own, so the
	// reply holds that session only.
	treeSessionQuery
	// treeTurn: {id} is a turn of a session in the token's request tree:
	// the session query's when sent, else the turn's newest.
	treeTurn
	// askTurn: POST /api/v0/turns. The handler checks the rest (askChecks).
	askTurn
	// askReview: {id} is a turn of a session in the token's request tree, to
	// review.
	askReview
	// askedTurn: {id} is a turn that work in the token's request tree asked
	// for, to stop.
	askedTurn
	// anyRead: a read that holds no session's data, such as the saved
	// agents.
	anyRead
	// renameSession: {id} is a session in the token's scope, to rename.
	renameSession
	// applyTurnTok: {id} is a turn of a session in the token's scope, whose
	// changes to apply.
	applyTurnTok
	// toolList: POST /api/v0/mcp, the API as tools. Each tool call is sent
	// through its own route and checked there (tools.go).
	toolList
	// onRegister: POST /api/v0/ons. The subject turn is in the body; the
	// handler checks it is a turn the token's tree started and the delivery
	// session is the token's own (on.go).
	onRegister
	// onPath: DELETE /api/v0/ons/{id}. The handler checks the on's maker is
	// this token's turn (on.go).
	onPath
)

// Refusals a turn token gets. Fixed wording: they never quote the request.
const (
	turnWriteRefused = "a turn token cannot call this route; it takes the main token. " +
		"A turn token reads its request tree's sessions and turns, lists agents, " +
		"and asks for work with POST /api/v0/turns and POST /api/v0/turns/{id}/review"
	turnSessionRefused  = "a turn token reads only its own session and the sessions its request tree started"
	turnCallRefused     = "a turn token reads only the calls of its own session and the sessions its request tree started"
	turnReviewLeaf      = "a review reviews the turn and reports a verdict; it cannot ask for work, review another turn, apply changes or stop work. Read the record and reply with your verdict."
	turnContinueRefused = "a turn token does not continue sessions: ask_agent starts a new one, and its result comes back to your session " +
		"as a later turn; to follow up, ask again with what the result said"
	turnFolderRefused = "a turn token starts sessions only in its own session's folder, or a folder inside it; " +
		"leave folder out to start in your own session's folder"
	turnIsolateRefused = "a session a turn token starts always gets its own isolated copy of its folder; leave isolate out"
	turnAskedRefused   = "a turn token stops only turns its request tree asked for"
)

// The refusal when [agent_api] turns a kind of call off, naming the
// switch as the cockpit's Settings shows it. Fixed wording.
func turnSwitchedOff(what, label string) string {
	return "a turn token may not " + what + " here: " + label + " is off in Settings (Permissions)"
}

// turnPolicyBroken is the refusal when the policy cannot be used. It names
// where the policy is, never what is wrong with it.
func turnPolicyBroken(err error) string {
	where := "the permissions"
	var se *settings.Error
	if errors.As(err, &se) {
		where = se.Where
	}
	return where + " cannot be used, so a turn token's requests are refused; fix it"
}

// grantKey carries a turn token's grant from route to its handler.
type grantKey struct{}

// turnCall is a turn token's grant with the policy read for its request.
type turnCall struct {
	g    run.Grant
	perm settings.Policy
}

// grantOf is the grant of the turn token r was sent with, or nil.
func grantOf(r *http.Request) *run.Grant {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return nil
	}
	return &c.g
}

// policyOf is the permissions.toml read for r's turn token. Only a request
// grantOf answers for has one.
func policyOf(r *http.Request) settings.Policy {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return settings.DefaultPolicy()
	}
	return c.perm
}

// policy is the policy for one turn token's request. Where this service runs
// turns, its folders must be folders here, as a turn's are: a policy a turn
// could not start under refuses its token's requests too.
func (a *API) policy() (settings.Policy, error) {
	if a.cfg.Settings == nil {
		return settings.DefaultPolicy(), nil
	}
	p, _, err := a.cfg.Settings.Policy()
	if err != nil || a.cfg.Engine == nil {
		return p, err
	}
	home, _ := os.UserHomeDir()
	return p.Resolve(home)
}

// queueLimits are [limits] as the queue takes them.
func queueLimits(p settings.Policy) queue.Limits {
	return queue.Limits{MaxDepth: *p.Limits.MaxDepth, MaxPerTurn: *p.Limits.MaxJobsPerTurn, MaxPerTree: *p.Limits.MaxJobsPerTree}
}

// allowTurn checks a turn token's request on rt, and says why not. It may set
// the session query to the token's own (treeSessionQuery). The request it
// returns carries the grant, for the handler.
func (a *API) allowTurn(w http.ResponseWriter, r *http.Request, rt *route, id string, g run.Grant) (*http.Request, bool) {
	refuse := func(msg string) (*http.Request, bool) {
		writeError(w, http.StatusForbidden, msg)
		return nil, false
	}
	failed := func(err error) (*http.Request, bool) {
		if errors.Is(err, queue.ErrNotFound) {
			writeError(w, http.StatusNotFound, noTurn)
		} else {
			a.internal(w)
		}
		return nil, false
	}
	// The policy is read for every request, so an edit applies at once. A
	// file that cannot be used refuses every turn token's request: a token
	// never runs without the policy the person wrote.
	perm, err := a.policy()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, turnPolicyBroken(err))
		return nil, false
	}
	if msg := turnRefusal(rt.turn, perm.AgentAPI); msg != "" {
		return refuse(msg)
	}
	// A review is a leaf (B12): it reviews the turn and reports a verdict, so
	// its token never asks for, reviews, applies or stops further work. Its one
	// turn runs in a copy that is torn down at the end, so there is nothing for
	// a later turn to run in. Reading the record is allowed. (Read-only turns
	// retired, but a review stays a leaf at the token, not at the harness.)
	switch rt.turn {
	case askTurn, askReview, applyTurnTok, askedTurn:
		if a.cfg.Queue != nil {
			if isReview, err := a.cfg.Queue.IsReview(queue.DefaultTeam, g.Session); err != nil {
				return failed(err)
			} else if isReview {
				return refuse(turnReviewLeaf)
			}
		}
	}
	// A turn token reaches its own request tree, always: its own session,
	// the tree's root and every session work in the tree started. Start and
	// continue are told apart by the body: askChecks.
	inScope := func(session string) (bool, error) { return a.inTree(g, session) }
	switch rt.turn {
	case treeSessionPath, renameSession:
		if ok, err := inScope(id); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case treeSessionQuery:
		q := r.URL.Query()
		if vs, ok := q["session"]; ok {
			if len(vs) != 1 {
				return refuse(turnSessionRefused)
			}
			if in, err := inScope(vs[0]); err != nil {
				return failed(err)
			} else if !in {
				return refuse(turnSessionRefused)
			}
			break
		}
		q.Set("session", g.Session)
		r.URL.RawQuery = q.Encode()
	case treeTurn:
		session := ""
		if q := r.URL.Query(); q.Has("session") {
			if vs := q["session"]; len(vs) != 1 {
				return refuse(turnSessionRefused)
			}
			session = q.Get("session")
		}
		session, err := a.sessionOfTurn(id, session)
		if err != nil {
			a.internal(w)
			return nil, false
		}
		if session == "" {
			writeError(w, http.StatusNotFound, noTurn)
			return nil, false
		}
		if ok, err := inScope(session); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case askReview, applyTurnTok:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		if err != nil {
			return failed(err)
		}
		if ok, err := inScope(turnSession(j)); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case askedTurn:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		if err != nil {
			return failed(err)
		}
		if j.Links.Chain == nil || j.Links.Chain.By == "" || j.Links.Chain.Root != g.Root {
			return refuse(turnAskedRefused)
		}
	case askTurn, anyRead, toolList, onRegister, onPath:
		// The handler checks the rest: onRegister reads the subject turn from
		// the body and the session it delivers to; onPath reads the on's maker.
	default:
		return refuse(turnWriteRefused)
	}
	if rt.turn == askReview {
		// The review's idempotency key, read from its body and put back: a
		// retry of a review already made is not new work.
		var key struct {
			Key string `json:"idempotency_key"`
		}
		if r.Body != nil {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "the body could not be read")
				return nil, false
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			_ = json.Unmarshal(b, &key)
		}
		if msg, err := a.overBudget(g, perm, key.Key); err != nil {
			a.internal(w)
			return nil, false
		} else if msg != "" {
			return refuse(msg)
		}
	}
	ctx := context.WithValue(r.Context(), grantKey{}, &turnCall{g: g, perm: perm})
	return r.WithContext(ctx), true
}

// turnRefusal says why a turn token may not call a route of kind turn at all
// under [agent_api], or "": apply and tools. Reading, listing agents, starting,
// reviewing, stopping and renaming have no switch: within its request tree a
// turn token always may (0.5.2). The rest of the check needs the request
// (allowTurn). The tool list (tools.go) asks it too, so a tool is listed only
// where its route would take the call.
func turnRefusal(turn int, api settings.AgentAPI) string {
	switch {
	case turn == mainOnly:
		return turnWriteRefused
	case turn == applyTurnTok && !*api.Apply:
		return turnSwitchedOff("apply a turn's changes", "Agents may apply")
	case turn == toolList && !*api.Tools:
		return turnSwitchedOff("use the agentbus MCP tools", "AgentBus tools")
	}
	return ""
}

// askedLinks are the links of work g's turn asks for: the request tree it is
// in, one level deeper, naming g's turn and session as who asked (Chain.By/
// BySession). That is the job's recorded origin and the caps' provenance; its
// result is delivered by an on row the ask path registers (on.go), not from
// these links.
func askedLinks(g run.Grant) queue.Links {
	return queue.Links{Chain: &queue.Chain{Root: g.Root, Depth: g.Depth + 1, By: g.Turn, BySession: g.Session}}
}

// queueJob reads a job of the default team.
func (a *API) queueJob(id string) (queue.Job, error) {
	if a.cfg.Queue == nil {
		return queue.Job{}, queue.ErrNotFound
	}
	return a.cfg.Queue.Get(queue.DefaultTeam, id)
}

// overBudget says why g's request tree may ask for no more work under
// max_cost_usd_per_tree, or "" while it may: the cost of every model call in
// the tree's sessions so far. 0 is off. A retry whose idempotency key already
// made a job is not new work, so it passes, and gets its job as before.
func (a *API) overBudget(g run.Grant, perm settings.Policy, key string) (string, error) {
	max := *perm.Limits.MaxCostUSDPerTree
	if max <= 0 || a.cfg.Queue == nil {
		return "", nil
	}
	if known, err := a.cfg.Queue.HasKey(queue.DefaultTeam, key); err != nil || known {
		return "", err
	}
	sessions, err := a.cfg.Queue.TreeSessions(queue.DefaultTeam, g.Root)
	if err != nil {
		return "", err
	}
	cost, err := a.cfg.Store.CostOf(append(sessions, g.Root, g.Session))
	if err != nil {
		return "", err
	}
	if cost >= max {
		return fmt.Sprintf("work started from one session may cost at most $%.2f in all, and it has cost $%.2f (Cost per request tree in Settings, Limits)", max, cost), nil
	}
	return "", nil
}

// inTree says session is in g's request tree: its own session, the tree's
// root, or a session work in the tree started.
func (a *API) inTree(g run.Grant, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	if session == g.Session || session == g.Root {
		return true, nil
	}
	if a.cfg.Queue == nil {
		return false, nil
	}
	root, err := a.cfg.Queue.SessionRoot(queue.DefaultTeam, session)
	return err == nil && root != "" && root == g.Root, err
}

// askChecks checks a turn token's POST /api/v0/turns body and fills what the
// token decides: for a new session its folder, and the links of the work. It
// says why not, and has then answered.
func (a *API) askChecks(w http.ResponseWriter, g run.Grant, perm settings.Policy, body *TurnRequest, continues bool) (queue.Links, bool) {
	refuse := func(msg string) (queue.Links, bool) {
		writeError(w, http.StatusForbidden, msg)
		return queue.Links{}, false
	}
	// A turn's token never continues a session (his call, 2026-11-12):
	// ask_agent starts a new one, and its result comes back on its own.
	if continues {
		return refuse(turnContinueRefused)
	}
	if msg, err := a.overBudget(g, perm, body.IdempotencyKey); err != nil {
		a.internal(w)
		return queue.Links{}, false
	} else if msg != "" {
		return refuse(msg)
	}
	if body.Isolate != nil && !*body.Isolate {
		return refuse(turnIsolateRefused)
	}
	// The folder is compared by reference: the control plane holds no
	// path. Whether a path is inside a folder is for the folder's machine
	// to say (queue.FolderIssuer). The new session names the same machine,
	// so it runs where this one does.
	ref, machine, repo, err := a.cfg.Queue.SessionFolder(queue.DefaultTeam, g.Session)
	if err != nil {
		a.internal(w)
		return queue.Links{}, false
	}
	if ref == "" {
		return refuse(turnFolderRefused)
	}
	// local is true when the session's folder is on this control plane's own
	// machine (agentbus up). Only then can control answer whether a path or a
	// folder child is inside the session's folder: it needs the folder here.
	// A remote runner holds the folder, and control holds no path of a remote
	// session (jobs/record.go drops it), so a request that needs a path check
	// is left to that machine, never answered from here.
	local := a.cfg.Folders != nil && a.cfg.Folders.Machine() == machine
	switch {
	case body.Folder == "" && body.FolderRef == "" && repo && local:
		// A worktree's session: the new session works on the worktree as
		// this one left it, its base and edits, in a copy of its own.
		c, err := a.cfg.Store.Continuation(g.Session)
		if err != nil {
			a.internal(w)
			return queue.Links{}, false
		}
		if c.Folder == "" {
			return refuse(turnFolderRefused)
		}
		body.Folder = c.Folder
	case body.Folder == "" && body.FolderRef == "":
		// The folder the person picked, so the new session gets a copy of
		// its own beside this one, not inside it.
		body.FolderRef, body.Machine = ref, machine
	case body.FolderRef != "":
		// A folder named by reference. It must be on the session's machine
		// (machine), whichever machine that is. The same reference as the
		// session's own folder is in scope with no path check, so it is
		// allowed on a remote runner too. A different reference is inside the
		// session's folder only if the folder's machine says so, which control
		// can ask only when that machine is its own.
		if body.Machine == "" {
			body.Machine = machine
		}
		if body.Machine != machine {
			return refuse(turnFolderRefused)
		}
		if body.FolderRef != ref && (!local || !a.cfg.Folders.WithinRef(body.FolderRef, ref)) {
			return refuse(turnFolderRefused)
		}
	default:
		// The session's own folder or one inside it, or the folder its turns
		// run in (its copy, as the record names it), or one inside that.
		if !local {
			return refuse(turnFolderRefused)
		}
		if !a.cfg.Folders.Within(body.Folder, ref) {
			c, err := a.cfg.Store.Continuation(g.Session)
			if err != nil {
				a.internal(w)
				return queue.Links{}, false
			}
			if c.Folder == "" || !a.cfg.Folders.WithinDir(body.Folder, c.Folder) {
				return refuse(turnFolderRefused)
			}
		}
	}
	// A path was checked on this service's own machine, so it goes there
	// and nowhere else: never to another machine.
	if body.Folder != "" {
		if body.Machine != "" && body.Machine != machine {
			return refuse(turnFolderRefused)
		}
		body.Machine = machine
	}
	return askedLinks(g), true
}
