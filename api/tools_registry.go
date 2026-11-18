package api

// The tool registry served: the built-in apps (the routes that are tools) and
// the person's own, kept in one format by package toolreg. tools.go turns a
// route into a built-in tool; here each stored tool, built-in or custom, is
// resolved to the route it runs and served by one path, so a default removed
// and re-added as a custom tool behaves the same.
//
// toolSnap holds the served set, rebuilt whenever the registry changes and
// read without a lock. The management routes (GET/POST/DELETE /api/v0/tools)
// are control's own: a turn token cannot reach them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/staysup/agentbus/toolreg"
)

// toolSnapshot is the tools served at one moment.
type toolSnapshot struct {
	list   []servedTool
	byName map[string]servedTool
}

// servedTool is one stored tool resolved for serving: its stored record, the
// route it runs, and its MCP annotations.
type servedTool struct {
	def   toolreg.Tool
	rt    *route
	annot map[string]any
}

// defaultTools expresses the built-in tools in the stored format: each route
// that is a tool becomes one record, with its generated input schema and a
// run that names its route. These are the registry's defaults.
func defaultTools(tools []toolRoute, schemas map[string]map[string]any) []toolreg.Tool {
	out := make([]toolreg.Tool, 0, len(tools))
	for _, tr := range tools {
		out = append(out, toolreg.Tool{
			Name:        tr.t.name,
			Description: tr.t.doc,
			Inputs:      schemas[tr.t.name],
			Effect:      effect2(tr.t.effect),
			Where:       toolreg.WhereControl,
			Run: toolreg.Run{
				Kind:       toolreg.RunAPI,
				Method:     tr.rt.method,
				Path:       tr.rt.path,
				IDInput:    tr.t.id,
				OwnSession: tr.t.ownSession,
				Query:      queryNames(tr.rt),
				Body:       tr.rt.body != nil,
			},
		})
	}
	return out
}

// effect2 maps a route tool's finer effect to the two stored values: only
// reading, or doing something (his decision, 2026-11-18).
func effect2(effect string) string {
	if effect == effectNone {
		return toolreg.EffectReads
	}
	return toolreg.EffectDoes
}

// effect5 maps a stored effect back to a finer one, for the MCP annotations: a
// tool that only reads is read-only and idempotent; one that does something is
// neither. Hints come from the tool's own effect, so replacing a built-in's
// name with a different definition advertises the new definition's hints.
func effect5(e string) string {
	if e == toolreg.EffectReads {
		return effectNone
	}
	return effectStarts
}

// queryNames are a route's query parameters, the tool inputs that go into the
// query string rather than the body.
func queryNames(rt *route) []string {
	if len(rt.query) == 0 {
		return nil
	}
	out := make([]string, 0, len(rt.query))
	for _, p := range rt.query {
		out = append(out, p.name)
	}
	return out
}

// routeIndex is the route table by "METHOD path", built at New. It is a field
// so no function the route table's own initializer reaches refers back to the
// routes variable, which would be an initialization cycle.
func routeIndex() map[string]*route {
	out := make(map[string]*route, len(routes))
	for i := range routes {
		out[routes[i].method+" "+routes[i].path] = &routes[i]
	}
	return out
}

// routeFor is the route with this method and path, or nil.
func (a *API) routeFor(method, path string) *route {
	return a.routes[method+" "+path]
}

// rebuildTools resolves the registry into the served snapshot and stores it.
// A tool whose run names no route is left out, with a line in the service log:
// it cannot be served, but the others still can.
func (a *API) rebuildTools() {
	var list []servedTool
	byName := map[string]servedTool{}
	for _, def := range a.registry.List() {
		st, ok := a.resolveServed(def)
		if !ok {
			if a.cfg.Logf != nil {
				a.cfg.Logf("tool.unserved name=%s method=%s path=%s", def.Name, def.Run.Method, def.Run.Path)
			}
			continue
		}
		list = append(list, st)
		byName[def.Name] = st
	}
	a.toolSnap.Store(&toolSnapshot{list: list, byName: byName})
}

// resolveServed resolves one stored tool for serving: the route its call runs,
// and its MCP annotations, taken from the tool's own effect so a built-in and
// a custom tool of the same effect read the same.
func (a *API) resolveServed(def toolreg.Tool) (servedTool, bool) {
	rt := a.routeFor(def.Run.Method, def.Run.Path)
	if rt == nil {
		return servedTool{}, false
	}
	eff := effect5(def.Effect)
	return servedTool{def: def, rt: rt, annot: annotations(eff)}, true
}

// servedByName is the tool named name, served or not listed to this caller.
func (a *API) servedByName(name string) (servedTool, bool) {
	st, ok := a.toolSnap.Load().byName[name]
	return st, ok
}

// toolRequestFrom is the HTTP request a tool call names, built from the stored
// run and the call's arguments: the method, the path with {id} filled, the
// query and the body. own is the caller's session, for a tool whose session is
// optional, or "". It says why not in fixed words that name the input, never
// its value, exactly as the built-in path did.
func toolRequestFrom(def toolreg.Tool, args map[string]any, own string) (method, target string, body []byte, why string) {
	run := def.Run
	props, _ := def.Inputs["properties"].(map[string]any)
	for _, r := range schemaRequired(def.Inputs) {
		if v, ok := args[r]; !ok || v == nil {
			if r == run.IDInput && run.OwnSession && own != "" {
				continue
			}
			return "", "", nil, r + " is required"
		}
	}
	query := map[string]bool{}
	for _, q := range run.Query {
		query[q] = true
	}
	path := run.Path
	q := url.Values{}
	var fields map[string]any
	for name, v := range args {
		if _, ok := props[name]; !ok {
			return "", "", nil, "this tool takes no input of that name; tools/list gives its inputs"
		}
		switch {
		case run.IDInput != "" && name == run.IDInput:
			s, ok := v.(string)
			if !ok || !sessionID.MatchString(s) {
				return "", "", nil, name + " must be an id of letters, digits, _ and -"
			}
			path = strings.Replace(path, "{id}", s, 1)
		case query[name]:
			s, ok := queryValue(v)
			if !ok {
				return "", "", nil, name + " must be a string or a number"
			}
			q.Set(name, s)
		default:
			if fields == nil {
				fields = map[string]any{}
			}
			fields[name] = v
		}
	}
	if strings.Contains(path, "{id}") {
		// The caller's own session fills {id} only when the tool allows it
		// (own_session). Without that, a missing id is refused, never filled
		// in: a tool that does not opt in can never act on the caller's
		// session by leaving the id out.
		if !run.OwnSession || own == "" {
			return "", "", nil, run.IDInput + " is required"
		}
		path = strings.Replace(path, "{id}", own, 1)
	}
	if run.Body {
		if fields == nil {
			fields = map[string]any{}
		}
		body, _ = json.Marshal(fields)
	}
	target = path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return run.Method, target, body, ""
}

// The management routes: control's own. A turn token cannot reach them.

// ToolList is the answer to GET /api/v0/tools.
type ToolList struct {
	Tools []toolreg.Tool `json:"tools" doc:"The tools, built-in and custom, in a stable order."`
}

const (
	badTool    = "the body must be one JSON object: a tool with name, description, inputs, effect, where and run"
	noSuchTool = "no such tool"
	toolRoute0 = "run must name a route a built-in tool already exposes; another route (one that is not a tool, has a {call}/{n} path, or needs a header such as If-Match) cannot be served as a tool"
)

// GET /api/v0/tools
func (a *API) listTools(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, ToolList{a.registry.List()})
}

// POST /api/v0/tools
func (a *API) createTool(w http.ResponseWriter, r *http.Request) {
	var t toolreg.Tool
	if !decodeObject(r, &t) {
		writeError(w, http.StatusBadRequest, badTool)
		return
	}
	// An api tool may run only a route a built-in tool already exposes (one
	// with a tools entry). This one rule is what keeps a custom tool servable:
	// it rejects a route that is not a tool at all, a path with {call} or {n}
	// (never a tool's route), and a route that needs a header the stored
	// format cannot supply (such as a settings write's If-Match) — none of
	// which a built-in exposes. The call still runs with the caller's own
	// token, so a custom tool can reach nothing the caller could not.
	if t.Run.Kind == toolreg.RunAPI {
		if rt := a.routeFor(t.Run.Method, t.Run.Path); rt == nil || len(rt.tools) == 0 {
			writeError(w, http.StatusBadRequest, toolRoute0)
			return
		}
	}
	// The change and the rebuild it triggers are one critical section, so two
	// changes at once cannot publish a snapshot that misses one.
	a.toolsMu.Lock()
	saved, err := a.registry.Add(t)
	if err == nil {
		a.rebuildTools()
	}
	a.toolsMu.Unlock()
	if a.toolFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// DELETE /api/v0/tools/{id}: id is the tool's name.
func (a *API) removeTool(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	a.toolsMu.Lock()
	err := a.registry.Remove(id)
	if err == nil {
		a.rebuildTools()
	}
	a.toolsMu.Unlock()
	if a.toolFailed(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toolFailed answers err, if there is one, and says whether it did.
func (a *API) toolFailed(w http.ResponseWriter, err error) bool {
	var bad *toolreg.Invalid
	switch {
	case err == nil:
		return false
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Why)
	case errors.Is(err, toolreg.ErrExists):
		writeError(w, http.StatusConflict, "a tool of that name already exists; remove it first")
	case errors.Is(err, toolreg.ErrFull):
		writeError(w, http.StatusConflict, "too many tools; remove some first")
	case errors.Is(err, toolreg.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchTool)
	default:
		a.internal(w)
	}
	return true
}
