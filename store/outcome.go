package store

// Control's own outcomes of a job, kept in the record (turn.changed,
// turn.applied) and named from the job by their event id.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/event"
)

// EventByID reads the event with id of session and turn (turn empty for an
// event of no turn). found is false when the record holds none.
func (s *Store) EventByID(session, turn, id string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(`SELECT data FROM events WHERE session = ? AND turn = ? AND json_extract(data, '$.id') = ?
		ORDER BY seq LIMIT 1`, session, turn, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read event: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, false, fmt.Errorf("store: read event: %w", err)
	}
	return e, true, nil
}

// LatestPreview reads the session's newest changes.previewed event, and the
// seq it was stored at. found is false when the session has none.
func (s *Store) LatestPreview(session string) (e event.Event, seq int64, found bool, err error) {
	var data string
	err = s.r.QueryRow(`SELECT seq, data FROM events WHERE session = ? AND kind = ?
		ORDER BY seq DESC LIMIT 1`, session, string(event.ChangesPreviewed)).Scan(&seq, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, 0, false, nil
	}
	if err != nil {
		return e, 0, false, fmt.Errorf("store: read preview: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, 0, false, fmt.Errorf("store: read preview: %w", err)
	}
	return e, seq, true, nil
}
