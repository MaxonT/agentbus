package api

// The context view: how big a turn's or a session's context is, how much came
// from the prompt cache, and what the prompt is made of. Both read the record
// at request time; nothing is stored. GET /api/v0/turns/{id}/context is one
// turn: its context size, its cache-versus-fresh split, and what its largest
// prompt was made of, from that call's stored request pieces. GET
// /api/v0/sessions/{id}/context is the session: its totals and a page of its
// turns, so the context's growth across the session is visible.

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/store"
)

// TurnContext is the answer to GET /api/v0/turns/{id}/context.
type TurnContext struct {
	Store           string       `json:"store" doc:"The store id."`
	Seq             int64        `json:"seq" doc:"The seq the context was read at."`
	Turn            string       `json:"turn"`
	Session         string       `json:"session"`
	Model           string       `json:"model" doc:"The model of the turn's largest call, the one made_of is of, read from its request. Empty when the turn made no call with usage, or that call's request body is not kept."`
	Calls           int          `json:"calls" doc:"The turn's model calls that carried usage."`
	ContextTokens   int          `json:"context_tokens" doc:"How big the turn's prompt got: the largest over its calls of input + cache_read + cache_write tokens, the whole context one call read. 0 when no call carried usage. The vendor's own counts."`
	CacheReadTokens int          `json:"cache_read_tokens" doc:"Summed over the turn's calls: tokens read from the prompt cache instead of processed afresh."`
	FreshTokens     int          `json:"fresh_tokens" doc:"Summed over the turn's calls: input + cache_write tokens, the part processed afresh. The cost driver."`
	OutputTokens    int          `json:"output_tokens" doc:"Summed over the turn's calls: the tokens the model wrote."`
	MadeOf          *Composition `json:"made_of" doc:"What the turn's largest prompt was made of, from that call's stored request pieces. Null when no call carried usage, or no request body is kept for that call."`
}

// Composition is what one prompt was made of, from its stored request pieces.
type Composition struct {
	Exchange     string        `json:"exchange" doc:"The call this is of: the turn's largest, at GET /api/v0/turns/{id}/calls/{exchange}."`
	Vendor       string        `json:"vendor" doc:"The request format: anthropic or openai."`
	Bytes        int           `json:"bytes" doc:"The request body's length in bytes."`
	ApproxTokens int           `json:"approx_tokens" doc:"The parts' approx tokens summed: a rough size/2.66 count measured on one Claude Code request, off by up to 15%, not the vendor's tokenizer."`
	Parts        []ContextPart `json:"parts" doc:"The prompt's parts, largest first in bytes."`
	Note         string        `json:"note" doc:"What these parts do and do not separate for this vendor."`
}

// ContextPart is one kind of piece in a prompt, summed.
type ContextPart struct {
	Kind         string `json:"kind" doc:"system, tool or message (Anthropic); instructions, tool or input_item (OpenAI)."`
	Label        string `json:"label,omitempty" doc:"For a message or input item, the pieces' shared role or type, such as user or assistant. Empty for system and tools, which are grouped whole."`
	ApproxTokens int    `json:"approx_tokens" doc:"This part's approx tokens, as in the composition."`
	Bytes        int    `json:"bytes" doc:"This part's bytes."`
	Count        int    `json:"count" doc:"How many pieces of this kind and label the prompt carried."`
}

// SessionContext is the answer to GET /api/v0/sessions/{id}/context.
type SessionContext struct {
	Store             string               `json:"store" doc:"The store id."`
	Seq               int64                `json:"seq" doc:"The seq the context was read at."`
	Session           string               `json:"session"`
	Turns             int                  `json:"turns" doc:"The session's turns that reported usage on a model call; growth holds a page of them."`
	PeakContextTokens int                  `json:"peak_context_tokens" doc:"The largest context any of the session's turns carried: input + cache_read + cache_write on its biggest call."`
	CacheReadTokens   int                  `json:"cache_read_tokens" doc:"The session's calls summed: tokens read from the prompt cache."`
	FreshTokens       int                  `json:"fresh_tokens" doc:"The session's calls summed: input + cache_write tokens, the part processed afresh."`
	OutputTokens      int                  `json:"output_tokens" doc:"The session's calls summed: the tokens the models wrote."`
	Growth            []SessionContextTurn `json:"growth" doc:"A page of the session's turns, newest first, each with how big its context got. Later turns carry the earlier ones, so the context grows across the session. This view owns tokens, not cost: a turn's cost is on GET /api/v0/turns/{id}."`
	NextBefore        *int64               `json:"next_before" doc:"The before for the next, older page, or null when the page reaches the session's first turn. Follow it to the end for the whole history."`
}

// SessionContextTurn is one turn in a session's context growth.
type SessionContextTurn struct {
	Turn            string    `json:"turn"`
	Started         time.Time `json:"started" doc:"When the turn's first model call was recorded."`
	Calls           int       `json:"calls" doc:"The turn's model calls that carried usage."`
	ContextTokens   int       `json:"context_tokens" doc:"How big this turn's prompt got: input + cache_read + cache_write on its biggest call."`
	CacheReadTokens int       `json:"cache_read_tokens" doc:"This turn's calls summed: tokens read from the prompt cache."`
	FreshTokens     int       `json:"fresh_tokens" doc:"This turn's calls summed: input + cache_write tokens."`
	OutputTokens    int       `json:"output_tokens" doc:"This turn's calls summed."`
}

// GET /api/v0/turns/{id}/context: one turn's context size, cache split and
// what its largest prompt was made of.
func (a *API) turnContext(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	// Every token figure comes from one pass over the turn's model.responded
	// events, so the fields are consistent (store/turns.go's summed usage is a
	// separate read that a reply could land between). Reading the replies
	// straight also counts one whose request event was dropped.
	tok, err := a.cfg.Store.TurnContextTokens(t.Session, t.ID)
	if err != nil {
		a.internal(w)
		return
	}
	out := TurnContext{Store: a.cfg.Store.ID(), Seq: a.cfg.Store.Last(), Turn: t.ID, Session: t.Session,
		Calls: tok.Calls, ContextTokens: tok.Peak, CacheReadTokens: tok.CacheRead,
		FreshTokens: tok.Fresh, OutputTokens: tok.Output}
	if tok.Exchange != "" {
		cc, found, err := a.cfg.Store.CallContext(tok.Exchange, false)
		if err != nil {
			a.internal(w)
			return
		}
		if found {
			out.MadeOf = composition(cc)
			// The model the peak call asked for, from the request it carried.
			var top struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(cc.Request, &top) == nil {
				out.Model = top.Model
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// composition sums a call's request pieces into parts: system and tools whole,
// messages and input items split by their role or type. The finer split the
// request does not carry (a file read inside a tool result inside a user
// message) is named in the note, not invented.
func composition(c store.CallContext) *Composition {
	type key struct{ kind, label string }
	idx := map[key]int{}
	var parts []ContextPart
	approx, bytes := 0, 0
	for _, p := range c.Pieces {
		label := ""
		if p.Kind == body.Message || p.Kind == body.InputItem {
			label = p.Label
		}
		k := key{p.Kind, label}
		i, ok := idx[k]
		if !ok {
			i = len(parts)
			idx[k] = i
			parts = append(parts, ContextPart{Kind: p.Kind, Label: label})
		}
		parts[i].ApproxTokens += p.ApproxTokens
		parts[i].Bytes += p.Size
		parts[i].Count++
		approx += p.ApproxTokens
		bytes += p.Size
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Bytes > parts[j].Bytes })
	if parts == nil {
		parts = []ContextPart{}
	}
	return &Composition{Exchange: c.Exchange, Vendor: c.Vendor, Bytes: c.Size,
		ApproxTokens: approx, Parts: parts, Note: compositionNote(c.Vendor)}
}

func compositionNote(vendor string) string {
	switch vendor {
	case "anthropic":
		return "A user message holds your text and the tool results and files the turn read, together: the request does not separate them. Assistant messages are the model's own replies. Tokens are approximate; the cache split above is the vendor's exact count."
	case "openai":
		return "Input items split the conversation by type: a message, a function call and its output, a reasoning item. A file the turn read is inside the function-call output that returned it. Tokens are approximate; the cache split above is the vendor's exact count."
	default:
		return "Parts are the request's own pieces. Tokens are approximate; the cache split above is the vendor's exact count."
	}
}

// GET /api/v0/sessions/{id}/context: a session's context totals and a page of
// its turns, newest first, so its growth is visible.
func (a *API) sessionContext(w http.ResponseWriter, r *http.Request, id string) {
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	sc, err := a.cfg.Store.SessionContext(id, before, limit)
	if err != nil {
		a.internal(w)
		return
	}
	growth := make([]SessionContextTurn, len(sc.Page))
	for i, t := range sc.Page {
		growth[i] = SessionContextTurn{Turn: t.Turn, Started: t.Started, Calls: t.Calls,
			ContextTokens: t.Context, CacheReadTokens: t.CacheRead, FreshTokens: t.Fresh,
			OutputTokens: t.Output}
	}
	writeJSON(w, http.StatusOK, SessionContext{Store: sc.Store, Seq: sc.Seq, Session: id,
		Turns: sc.Turns, PeakContextTokens: sc.Peak, CacheReadTokens: sc.CacheRead,
		FreshTokens: sc.Fresh, OutputTokens: sc.Output, Growth: growth, NextBefore: sc.NextBefore})
}
