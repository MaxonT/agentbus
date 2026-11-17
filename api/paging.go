package api

// One paging convention for every served list, the same the turn list uses:
// ?before=<n>&limit=<n> in, next_before out. before is an exclusive upper
// bound on a list's 1-based position; a page is the last limit items before
// it, and next_before is the before for the next, earlier page, or null at
// the start. A list a client reads whole in one page when it is short, and
// walks in pages when it is long; no list is ever cut.

import (
	"net/http"
	"strconv"
)

const defaultPageLimit = 100

// pageParams reads before and limit from the query, in the turn list's shape.
// It writes the error and returns ok=false when either is malformed.
func pageParams(w http.ResponseWriter, r *http.Request) (before int64, limit int, ok bool) {
	limit = defaultPageLimit
	q := r.URL.Query()
	if q.Has("before") {
		n, err := strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be a whole number, 1 or more")
			return 0, 0, false
		}
		before = n
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxTurnLimit {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 1000")
			return 0, 0, false
		}
		limit = n
	}
	return before, limit, true
}

// pageBefore returns the window of items ending just before the 1-based
// position before (0 or past the end means the end), at most limit of them,
// and the before for the next, earlier page, or nil at the start. The list
// is in its canonical order; the page is the last limit items before the
// cursor, so a default read returns the end and next_before walks back.
func pageBefore[T any](items []T, before int64, limit int) (window []T, nextBefore *int64) {
	n := int64(len(items))
	hi := n
	if before > 0 && before-1 < n {
		hi = before - 1
	}
	if hi < 0 {
		hi = 0
	}
	lo := hi - int64(limit)
	if lo < 0 {
		lo = 0
	}
	window = items[lo:hi]
	if lo > 0 {
		v := lo + 1
		nextBefore = &v
	}
	return window, nextBefore
}
