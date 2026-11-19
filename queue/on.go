package queue

// The on primitive: a standing instruction on control, "when this event
// happens, do this". It is a row (the ons table) plus the control loop's
// existing check (FireOns, called where Deliver was); nothing waits on
// control. B13 has one event and one action:
//
//	on turn.completed -> start a turn in a session with the subject turn's reply.
//
// This is what today's delivery already does (asked.go, Deliver); the on row
// generalises it so later events (apply, a timer, before-apply) are the same
// primitive with another event, not another mechanism.
//
// A turn.completed on is one-shot: it fires once, when its subject turn ends,
// and is then off. So it has a definite end, which is how "an on cannot set
// itself off forever" holds for B13 (there is no repeating on yet; a repeating
// one, C3, must carry an end or be ended by off). A fire is recorded in
// on_fires, keyed by the subject turn, so a second sweep or a control restart
// never fires it twice: the same safety the deliveries table gives delivery.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// OnTurnCompleted is B13's only event: the subject turn has ended.
const OnTurnCompleted = "turn.completed"

// ErrUnknownEvent: RegisterOn was given an event it does not know. Caught at
// registration so a typo cannot become an on that never fires.
var ErrUnknownEvent = errors.New("queue: unknown on event")

// Maker is who registered an on: their session and turn. Kept for the record,
// and, for a turn.completed on, the turn whose agent the delivered result runs
// on (the asking turn).
type Maker struct {
	Session string `json:"session"`
	Turn    string `json:"turn"`
}

// On is a registered standing instruction.
type On struct {
	ID      string `json:"id"`
	Team    string `json:"team"`
	Event   string `json:"event"`
	Subject string `json:"subject"`
	Session string `json:"session"`
	Maker   Maker  `json:"maker"`
	Created int64  `json:"created"`
}

// onReg is an on to register in the same transaction as the turn it watches,
// so the two are durable together: an ask registers one for the turn it starts
// (api/runs.go, api/review.go), and a control stop between the two writes can
// no longer leave an asked turn with no on and strand its result. Event is
// always turn.completed; the subject is the turn create assigns.
type onReg struct {
	session string
	maker   Maker
}

// insertOn writes the on row for turnID inside tx, idempotent per subject so a
// retry of the same ask does not add a second. "" turnID is a no-op.
func insertOn(tx *sql.Tx, team, turnID string, on *onReg, now int64) error {
	if on == nil || turnID == "" {
		return nil
	}
	mk, err := json.Marshal(on.maker)
	if err != nil {
		return fmt.Errorf("queue: register on: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, created) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(team, event, subject) DO NOTHING`, NewID("on_"), team, OnTurnCompleted, turnID, on.session, string(mk), now); err != nil {
		return fmt.Errorf("queue: register on: %w", err)
	}
	return nil
}

// RegisterOn stores an on and returns it. event must be known; subject (the
// turn watched) and session (where the action starts its turn) are required.
// It is idempotent per (event, subject): registering the same subject again
// returns the on already stored, unchanged, so the ask path may register on
// both the first request and a retry.
func (q *Queue) RegisterOn(team, event, subject, session string, maker Maker) (On, error) {
	if event != OnTurnCompleted {
		return On{}, fmt.Errorf("%w: %q", ErrUnknownEvent, event)
	}
	if subject == "" || session == "" {
		return On{}, ErrBadState
	}
	mk, err := json.Marshal(maker)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	o := On{ID: NewID("on_"), Team: team, Event: event, Subject: subject, Session: session, Maker: maker, Created: nanos(q.now())}
	res, err := q.db.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, created) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(team, event, subject) DO NOTHING`,
		o.ID, o.Team, o.Event, o.Subject, o.Session, string(mk), o.Created)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return o, nil
	}
	// Already registered for this subject: return the stored one.
	return q.onBySubject(team, event, subject)
}

// GetOn reads an on by id, off or not; ErrNotFound if none.
func (q *Queue) GetOn(team, id string) (On, error) {
	o := On{ID: id, Team: team}
	var mk string
	switch err := q.db.QueryRow(`SELECT event, subject, session, maker, created FROM ons WHERE id = ? AND team = ?`,
		id, team).Scan(&o.Event, &o.Subject, &o.Session, &mk, &o.Created); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	return o, nil
}

// onBySubject reads the on registered for (event, subject).
func (q *Queue) onBySubject(team, event, subject string) (On, error) {
	o := On{Team: team, Event: event, Subject: subject}
	var mk string
	switch err := q.db.QueryRow(`SELECT id, session, maker, created FROM ons WHERE team = ? AND event = ? AND subject = ?`,
		team, event, subject).Scan(&o.ID, &o.Session, &mk, &o.Created); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	return o, nil
}

// RemoveOn sets an on off: FireOns ignores it from then on. Removing one that
// is already off is a no-op; an unknown id is ErrNotFound.
//
// Remove is best-effort against a fire already in flight, the standard shape of
// cancellation: it stops every future fire, but an on the sweep is already
// delivering this tick may still deliver its reply once (never twice: the
// delivery is idempotent on the result's key, ResultKey). FireOns re-reads off
// right before it delivers, so a remove that finishes before the sweep reaches
// the on wins; the residual window is between that read and the delivery. This
// is a design choice, not a defect: closing it fully would put the off check
// inside the delivery's own transaction and give up the crash-safe re-delivery
// the sweep relies on (deliver-first, deduped by on_fires, survives a restart).
func (q *Queue) RemoveOn(team, id string) error {
	res, err := q.db.Exec(`UPDATE ons SET off = ? WHERE id = ? AND team = ? AND off IS NULL`, nanos(q.now()), id, team)
	if err != nil {
		return fmt.Errorf("queue: remove on: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var one int
	switch err := q.db.QueryRow(`SELECT 1 FROM ons WHERE id = ? AND team = ?`, id, team).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("queue: remove on: %w", err)
	default:
		return nil // already off
	}
}

// ListOns returns the team's live (not off) ons, oldest first.
func (q *Queue) ListOns(team string) ([]On, error) {
	rows, err := q.db.Query(`SELECT id, event, subject, session, maker, created FROM ons
		WHERE team = ? AND off IS NULL ORDER BY created, id`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list ons: %w", err)
	}
	defer rows.Close()
	var out []On
	for rows.Next() {
		o := On{Team: team}
		var mk string
		if err := rows.Scan(&o.ID, &o.Event, &o.Subject, &o.Session, &mk, &o.Created); err != nil {
			return nil, fmt.Errorf("queue: list ons: %w", err)
		}
		if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
			return nil, fmt.Errorf("queue: list ons: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FireOns is the control loop's check for ons that have matured: for each live
// turn.completed on whose subject turn has ended and that has not fired, it
// queues the delivery turn (exactly as Deliver did), records the fire once,
// and sets the one-shot off. It says how many delivery turns it queued. name
// and agent are as Deliver's: name gives a saved agent's name for the prompt,
// agent the agent a delivered result runs on. A full queue stops the sweep
// with no error, to try again later.
func (q *Queue) FireOns(team string, name func(id string) string, agent AgentOf) (int, error) {
	rows, err := q.db.Query(`SELECT o.id, o.subject, o.session, o.maker FROM ons o
		JOIN jobs j ON j.team = o.team AND j.turn = o.subject
		WHERE o.team = ? AND o.event = ? AND o.off IS NULL AND j.ended IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM on_fires f WHERE f.on_id = o.id AND f.key = o.subject)
		ORDER BY j.ended, o.id LIMIT 20`, team, OnTurnCompleted)
	if err != nil {
		return 0, fmt.Errorf("queue: list ons: %w", err)
	}
	type pending struct{ id, subject, session, maker string }
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject, &p.session, &p.maker); err != nil {
			rows.Close()
			return 0, fmt.Errorf("queue: list ons: %w", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		var mk Maker
		if err := json.Unmarshal([]byte(p.maker), &mk); err != nil {
			return n, fmt.Errorf("queue: read on maker: %w", err)
		}
		// Re-read the on: a removal since the sweep listed it should win, so a
		// removed on does not deliver. A removal in the small window between
		// this read and the delivery is the residual race; the delivery is
		// idempotent (ResultKey), so at worst one reply is queued for an on
		// removed at that instant, never more.
		var off sql.NullInt64
		if err := q.db.QueryRow(`SELECT off FROM ons WHERE id = ? AND team = ?`, p.id, team).Scan(&off); err != nil {
			return n, fmt.Errorf("queue: read on: %w", err)
		}
		if off.Valid {
			continue
		}
		j, err := q.JobByTurn(team, p.subject)
		if err != nil {
			return n, err
		}
		did, why, err := q.deliverResult(team, p.session, mk.Turn, j, name, agent)
		if errors.Is(err, ErrFull) {
			return n, nil // the queue is full: try again later, the on is still live
		}
		if err != nil {
			return n, err
		}
		if why == "" {
			n++
		}
		if _, err := q.db.Exec(`INSERT INTO on_fires(on_id, key, job, error, created) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`,
			p.id, p.subject, did, why, nanos(q.now())); err != nil {
			return n, fmt.Errorf("queue: write on fire: %w", err)
		}
		if _, err := q.db.Exec(`UPDATE ons SET off = ? WHERE id = ? AND team = ? AND off IS NULL`,
			nanos(q.now()), p.id, team); err != nil {
			return n, fmt.Errorf("queue: set on off: %w", err)
		}
	}
	return n, nil
}

// An asked session is not closed here. Closing it (which retires its workspace
// copy) safely needs more than a delivered turn's change count: the session
// must be an asked one, every turn ended, with no unapplied changes anywhere
// (an unreadable diff counts as changes) and no reply still owed to it.
// Deciding that from one turn in the delivery sweep closed sessions that had
// changes or pending replies, so it is a separate "safe to retire" check, a
// follow-up with those cases as its tests. Until then nothing auto-closes.
