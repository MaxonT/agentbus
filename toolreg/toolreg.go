// Package toolreg is the tool registry: the one place control keeps which
// tools exist and what each is. A tool is one stored record in the format of
// plans/DESIGN_0_6_tools.md (milestone B): a name, a description written for a
// model, its inputs as one JSON Schema object, what it runs, its effect and
// where it runs.
//
// The built-in tools are defaults, set from code at start-up (SetDefaults);
// the person's own tools, and any default they removed, are the overlay, kept
// on control through a Backend. List is the defaults, minus the ones removed,
// with the added ones on top: a default removed and re-added as a custom tool
// is served from the overlay and behaves as any custom tool does, which is the
// point of one format.
//
// The package imports nothing of the module, so every package may use it. It
// knows nothing of routes or of how a call is served: the api package builds
// the defaults from its route table and serves what List returns.
package toolreg

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// A tool's effect, the honest label of what a call does. His decision,
// 2026-11-18: two values. The gate's default reads off it (reads → allow,
// does something → ask); a person's rule always overrides.
const (
	EffectReads = "reads"          // it only reads
	EffectDoes  = "does-something" // it starts work, or changes files
)

// Where a tool runs. Only control for now; a runner comes with script and
// command-MCP tools (B4, B5).
const WhereControl = "control"

// What a tool's run is. Only api for now: a call on the AgentBus API, run on
// control. mcp and script come later (B4, B5).
const RunAPI = "api"

// MaxTools bounds how many tools are kept.
const MaxTools = 500

// MaxName bounds a tool's name. It matches the API's path id (sessionID,
// {1,64}), so every name a tool is added under can also be removed through
// DELETE /api/v0/tools/{name}.
const MaxName = 64

// Tool is one stored tool.
type Tool struct {
	// Name is <verb>_<noun>, lower case, unique. A model and a recorded call
	// name it.
	Name string `json:"name" doc:"The tool's name: <verb>_<noun>, lower case, such as list_turns. Unique. A model calls it by this name."`
	// Description is what the tool does and its scope first, then what it does
	// not do. Written for a model.
	Description string `json:"description" doc:"What the tool does and its scope, written for a model: the scope first, then what it does not do."`
	// Inputs is the tool's arguments as one JSON Schema object.
	Inputs map[string]any `json:"inputs" doc:"The tool's arguments as one JSON Schema object: type object, with properties and required."`
	// Effect is reads or does-something.
	Effect string `json:"effect" doc:"reads (a call only reads; allowed by default) or does-something (a call starts work or changes files; asked by default). A person's rule overrides."`
	// Where is control.
	Where string `json:"where" doc:"Where the call runs: control."`
	// Run is what the call runs.
	Run Run `json:"run" doc:"What the call runs."`
}

// Run is what a tool call runs. For an api tool it is one HTTP request on the
// AgentBus API, built from these fields and the call's arguments.
type Run struct {
	// Kind is api.
	Kind string `json:"kind" doc:"api: a call on the AgentBus API."`
	// Method and Path are the API request the call makes. Path may hold {id}.
	Method string `json:"method" doc:"The API method, such as GET or POST."`
	Path   string `json:"path" doc:"The API path, such as /api/v0/turns/{id}/diff. {id} is filled from id_input."`
	// IDInput is the input that fills {id} in Path; "" when Path has none.
	IDInput string `json:"id_input,omitempty" doc:"The input whose value fills {id} in path; absent when the path has no {id}."`
	// OwnSession makes IDInput optional: absent, {id} is the caller's own
	// session.
	OwnSession bool `json:"own_session,omitempty" doc:"When true, id_input is optional: absent, {id} is the caller's own session."`
	// Query are the inputs that go into the request's query string; the rest
	// go into its JSON body, when Body is set.
	Query []string `json:"query,omitempty" doc:"The inputs that go into the request's query string; the rest go into its JSON body."`
	// Body says the request carries a JSON body of the inputs that are not the
	// id and not in Query. False sends no body, as a GET or a bodyless POST
	// does.
	Body bool `json:"body,omitempty" doc:"Whether the request carries a JSON body of the inputs that are not the id and not in query."`
}

// Errors.
var (
	ErrNotFound = errors.New("toolreg: no such tool")
	ErrExists   = errors.New("toolreg: a tool of that name already exists")
	ErrFull     = errors.New("toolreg: too many tools")
)

// Invalid says why a tool was refused, in words fit for the person.
type Invalid struct{ Why string }

func (e *Invalid) Error() string { return e.Why }

// nameShape is a tool name: <verb>_<noun>, lower case.
var nameShape = regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`)

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// Check says what is wrong with t, or "". It fills Where when absent.
func Check(t *Tool) string {
	t.Name = strings.TrimSpace(t.Name)
	switch {
	case !nameShape.MatchString(t.Name):
		return "name must be <verb>_<noun>, in lower case, such as list_turns"
	case len(t.Name) > MaxName:
		return "name must be at most 64 bytes, so it can also be removed by name"
	case strings.TrimSpace(t.Description) == "":
		return "a tool needs a description, written for a model"
	case t.Effect != EffectReads && t.Effect != EffectDoes:
		return "effect must be reads or does-something"
	case t.Where != "" && t.Where != WhereControl:
		return "where must be control"
	case t.Run.Kind != RunAPI:
		return "run kind must be api"
	case !methods[t.Run.Method]:
		return "run method must be GET, POST, PUT, PATCH or DELETE"
	case !strings.HasPrefix(t.Run.Path, "/api/"):
		return "run path must be an AgentBus API path, such as /api/v0/turns"
	case strings.Contains(t.Run.Path, "{id}") != (t.Run.IDInput != ""):
		return "run id_input must be set exactly when the path has {id}"
	case !objectSchema(t.Inputs):
		return "inputs must be a JSON Schema object: type object, with properties that are each a schema object, and required a list of its property names"
	}
	// Every input the run references must be a property, so a call can supply
	// it and the serving path accepts it: the id and the query inputs. (The
	// required names are checked by objectSchema; a body field is any input
	// that is neither.) Without this an accepted tool could never be called.
	props, _ := t.Inputs["properties"].(map[string]any)
	if t.Run.IDInput != "" {
		if _, ok := props[t.Run.IDInput]; !ok {
			return "run id_input must be one of the inputs (a property of inputs)"
		}
	}
	for _, q := range t.Run.Query {
		if _, ok := props[q]; !ok {
			return "every run query input must be one of the inputs (a property of inputs)"
		}
	}
	if t.Where == "" {
		t.Where = WhereControl
	}
	return ""
}

// objectSchema says s is a structurally consistent object schema: one this
// service can serve and call. The whole rule, so a schema that is accepted is
// always callable:
//   - type is object;
//   - properties is a map, and every property's value is itself a schema
//     object;
//   - required, when present, is a list of names, and every name is a
//     property (so a required input a call can actually supply).
//
// It is deliberately bounded, not a full JSON Schema validator (his call,
// 2026-11-18): it checks the structure the serving path depends on, and does
// not validate the keywords inside each property's schema.
func objectSchema(s map[string]any) bool {
	if s == nil {
		return false
	}
	if ty, _ := s["type"].(string); ty != "object" {
		return false
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return false
	}
	for _, v := range props {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	required, ok := nameList(s["required"])
	if !ok {
		return false
	}
	for _, name := range required {
		if _, ok := props[name]; !ok {
			return false // a required input that is not a property
		}
	}
	return true
}

// nameList reads a JSON Schema "required" value as a list of names: absent is
// an empty list; an array of strings is that list; anything else is not a list
// of names, and false says so.
func nameList(v any) ([]string, bool) {
	switch r := v.(type) {
	case nil:
		return nil, true
	case []string:
		return r, true
	case []any:
		out := make([]string, 0, len(r))
		for _, x := range r {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// Registry keeps the tools. Its methods are safe to call at once from many
// goroutines of one process.
type Registry struct {
	b  Backend
	mu sync.Mutex
	// defaults are the built-in tools, in order, set from code.
	defaults []Tool
	// added are the person's tools and re-added defaults, by name.
	added map[string]Tool
	// removed are the names of defaults the person removed.
	removed map[string]bool
}

// Backend keeps the overlay on control: the added tools and the removed
// defaults. Nil keeps the overlay in memory for this process only.
type Backend interface {
	// Load reads the overlay.
	Load() (added []Tool, removed []string, err error)
	// Save writes the whole overlay.
	Save(added []Tool, removed []string) error
}

// New returns a registry with the overlay read from b (nil: in memory). The
// defaults are set after, with SetDefaults.
func New(b Backend) (*Registry, error) {
	r := &Registry{b: b, added: map[string]Tool{}, removed: map[string]bool{}}
	if b == nil {
		return r, nil
	}
	added, removed, err := b.Load()
	if err != nil {
		return nil, err
	}
	for _, t := range added {
		r.added[t.Name] = t
	}
	for _, n := range removed {
		r.removed[n] = true
	}
	return r, nil
}

// SetDefaults sets the built-in tools. They are kept in code, not in the
// overlay; the overlay holds only what the person changed.
func (r *Registry) SetDefaults(defaults []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults = append([]Tool(nil), defaults...)
}

// List is the tools served, in a stable order: the defaults in their order,
// minus the ones removed, then the added tools by name. A default re-added as
// a custom tool is served from the overlay.
func (r *Registry) List() []Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list()
}

func (r *Registry) list() []Tool {
	out := make([]Tool, 0, len(r.defaults)+len(r.added))
	seen := map[string]bool{}
	for _, d := range r.defaults {
		if r.removed[d.Name] {
			continue
		}
		if t, ok := r.added[d.Name]; ok { // changed in place
			out = append(out, t)
		} else {
			out = append(out, d)
		}
		seen[d.Name] = true
	}
	extra := make([]string, 0, len(r.added))
	for n := range r.added {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		out = append(out, r.added[n])
	}
	return out
}

// Get is the tool named name, or ErrNotFound.
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.list() {
		if t.Name == name {
			return t, nil
		}
	}
	return Tool{}, ErrNotFound
}

// Add saves t as a new tool. It refuses a name already served, unless that
// name is a default the person removed, which this re-adds as a custom tool.
// It validates t and persists the overlay.
func (r *Registry) Add(t Tool) (Tool, error) {
	if why := Check(&t); why != "" {
		return Tool{}, &Invalid{why}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.list() {
		if s.Name == t.Name {
			return Tool{}, ErrExists
		}
	}
	if len(r.added) >= MaxTools {
		return Tool{}, ErrFull
	}
	prevAdded := r.added[t.Name]
	hadAdded := false
	if _, ok := r.added[t.Name]; ok {
		hadAdded = true
	}
	wasRemoved := r.removed[t.Name]
	r.added[t.Name] = t
	// Re-adding a removed default: the overlay no longer marks it removed, so
	// List serves the added one.
	delete(r.removed, t.Name)
	if err := r.save(); err != nil {
		if hadAdded {
			r.added[t.Name] = prevAdded
		} else {
			delete(r.added, t.Name)
		}
		r.removed[t.Name] = wasRemoved
		return Tool{}, err
	}
	return t, nil
}

// Remove removes the tool named name: a custom tool is dropped from the
// overlay; a default is marked removed. ErrNotFound when no tool is served by
// that name. It persists the overlay.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	served := false
	isDefault := false
	for _, d := range r.defaults {
		if d.Name == name {
			isDefault = true
		}
	}
	for _, s := range r.list() {
		if s.Name == name {
			served = true
		}
	}
	if !served {
		return ErrNotFound
	}
	prevAdded, hadAdded := r.added[name]
	wasRemoved := r.removed[name]
	delete(r.added, name)
	if isDefault {
		r.removed[name] = true
	}
	if err := r.save(); err != nil {
		if hadAdded {
			r.added[name] = prevAdded
		}
		if wasRemoved {
			r.removed[name] = true
		} else {
			delete(r.removed, name)
		}
		return err
	}
	return nil
}

// save writes the overlay through the backend. The caller holds the lock.
func (r *Registry) save() error {
	if r.b == nil {
		return nil
	}
	added := make([]Tool, 0, len(r.added))
	for _, t := range r.added {
		added = append(added, t)
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Name < added[j].Name })
	removed := make([]string, 0, len(r.removed))
	for n := range r.removed {
		removed = append(removed, n)
	}
	sort.Strings(removed)
	return r.b.Save(added, removed)
}
