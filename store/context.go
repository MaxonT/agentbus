package store

// The context view's token data: how big each turn's prompt got and how much
// of it came from the prompt cache, read from the turns' model.responded
// events. A turn's context is the largest prompt it sent: the most one of its
// calls read. cache_read is what came from the cache; input + cache_write is
// what was processed afresh. Both are the vendor's own counts, summed or maxed
// over a turn's calls. This view owns tokens only; a turn's cost and whether
// that cost is complete have one source, the turn itself (turns.go), and are
// not recomputed here. Nothing here is stored; it is read on request.

import (
	"fmt"
	"time"
)

// ContextTurn is one turn's token context, from its model.responded events.
type ContextTurn struct {
	Turn    string
	Started time.Time
	Seq     int64 // the turn's first model call, the paging cursor
	Calls   int   // calls that carried usage
	Context int   // max over the turn's calls of input + cache_read + cache_write
	// Summed over the turn's calls:
	CacheRead int
	Fresh     int // input + cache_write
	Output    int
}

// SessionContext is a session's token context: its totals and a page of its
// turns, newest first.
type SessionContext struct {
	Store      string
	Seq        int64
	Turns      int // the session's turns that made a model call
	Peak       int // the largest Context over those turns
	CacheRead  int // the session's calls summed
	Fresh      int // input + cache_write, summed
	Output     int // summed
	Page       []ContextTurn
	NextBefore *int64
}

// perCall selects a session's model.responded events (those with a turn), up to
// the read's seq, as columns turn, seq, time, has_usage (1 when a usage object
// is present) and the four usage figures i (input), o (output), r (cache read)
// and w (cache write), each defaulting to 0.
const perCall = `SELECT turn, seq, time,
		(json_type(data, '$.usage') = 'object') AS has_usage,
		COALESCE(json_extract(data, '$.usage.input_tokens'), 0) AS i,
		COALESCE(json_extract(data, '$.usage.output_tokens'), 0) AS o,
		COALESCE(json_extract(data, '$.usage.cache_read_tokens'), 0) AS r,
		COALESCE(json_extract(data, '$.usage.cache_write_tokens'), 0) AS w
	FROM events
	WHERE session = ? AND seq <= ? AND kind = 'model.responded' AND turn <> ''`

// perTurn groups perCall into one row per turn that carried usage: its first
// call's seq and time, its usage-bearing call count, the peak context and the
// summed cache, fresh and output.
const perTurn = `SELECT turn, MIN(seq) AS first_seq, MIN(time) AS started, SUM(has_usage) AS calls,
		MAX(i + r + w) AS ctx, SUM(r) AS cache_read, SUM(i) + SUM(w) AS fresh, SUM(o) AS output
	FROM call GROUP BY turn HAVING SUM(has_usage) > 0`

// TurnContextTokens is one turn's token figures, every one read in a single
// pass over its model.responded events, so the fields are consistent with each
// other (no reply can land between two reads). Reading the replies directly,
// not request/reply pairs, counts a reply whose model.requested event is gone.
type TurnContextTokens struct {
	Calls     int    // calls that carried usage
	Peak      int    // max over the calls of input + cache_read + cache_write
	CacheRead int    // summed
	Fresh     int    // input + cache_write, summed
	Output    int    // summed
	Exchange  string // the peak call's exchange
}

// TurnContextTokens reads one turn's token figures from its model.responded
// events in one pass.
func (s *Store) TurnContextTokens(session, turn string) (TurnContextTokens, error) {
	var out TurnContextTokens
	rows, err := s.r.Query(`SELECT COALESCE(json_extract(data, '$.exchange'), ''),
			COALESCE(json_extract(data, '$.usage.input_tokens'), 0),
			COALESCE(json_extract(data, '$.usage.output_tokens'), 0),
			COALESCE(json_extract(data, '$.usage.cache_read_tokens'), 0),
			COALESCE(json_extract(data, '$.usage.cache_write_tokens'), 0)
		FROM events
		WHERE session = ? AND turn = ? AND kind = 'model.responded'
			AND json_type(data, '$.usage') = 'object'`, session, turn)
	if err != nil {
		return out, fmt.Errorf("store: turn context: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ex string
		var i, o, r, w int
		if err := rows.Scan(&ex, &i, &o, &r, &w); err != nil {
			return out, fmt.Errorf("store: turn context: %w", err)
		}
		out.Calls++
		out.CacheRead += r
		out.Fresh += i + w
		out.Output += o
		if ctx := i + r + w; ctx > out.Peak {
			out.Peak, out.Exchange = ctx, ex
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: turn context: %w", err)
	}
	return out, nil
}

// SessionContext reads a session's token context: the totals over every turn
// that made a model call, and a page of those turns newest first. before is
// the previous page's NextBefore (0 for the first page); limit the page size.
func (s *Store) SessionContext(session string, before int64, limit int) (SessionContext, error) {
	if limit <= 0 {
		limit = 100
	}
	out := SessionContext{Store: s.ID(), Seq: s.Last()}

	// Totals: one row per turn, summed into the session's peak context and its
	// summed cache, fresh and output.
	rows, err := s.r.Query(`WITH call AS (`+perCall+`) `+perTurn, session, out.Seq)
	if err != nil {
		return out, fmt.Errorf("store: session context totals: %w", err)
	}
	for rows.Next() {
		var turn string
		var firstSeq, started int64
		var calls, ctx, cacheRead, fresh, output int
		if err := rows.Scan(&turn, &firstSeq, &started, &calls, &ctx, &cacheRead, &fresh, &output); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: session context totals: %w", err)
		}
		out.Turns++
		if ctx > out.Peak {
			out.Peak = ctx
		}
		out.CacheRead += cacheRead
		out.Fresh += fresh
		out.Output += output
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, fmt.Errorf("store: session context totals: %w", err)
	}
	rows.Close()

	// The page: the same per-turn rows, newest first by the turn's first call's
	// seq, with before as the cursor. limit+1 rows tell whether an older page
	// follows.
	rows, err = s.r.Query(`SELECT turn, first_seq, started, calls, ctx, cache_read, fresh, output FROM (
			WITH call AS (`+perCall+`) `+perTurn+`
		) WHERE (? = 0 OR first_seq < ?) ORDER BY first_seq DESC LIMIT ?`,
		session, out.Seq, before, before, limit+1)
	if err != nil {
		return out, fmt.Errorf("store: session context page: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t ContextTurn
		var started int64
		if err := rows.Scan(&t.Turn, &t.Seq, &started, &t.Calls, &t.Context,
			&t.CacheRead, &t.Fresh, &t.Output); err != nil {
			return out, fmt.Errorf("store: session context page: %w", err)
		}
		t.Started = time.Unix(0, started).UTC()
		out.Page = append(out.Page, t)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: session context page: %w", err)
	}
	if len(out.Page) > limit {
		last := out.Page[limit-1].Seq
		out.Page = out.Page[:limit]
		out.NextBefore = &last
	}
	return out, nil
}
