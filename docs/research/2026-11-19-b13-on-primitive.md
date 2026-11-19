# B13 — the minimal on/off, and ask_agent as a pure API app

Date: 2026-11-19. Author: claude (b13-on), for coordinator claude-agentbus-e0.
Task: `agentbus-notes/plans/TASKS_0_6.md` B13. Design of record:
`agentbus-notes/plans/DESIGN_0_6_tools.md` "One primitive: on and off".

## References first (what they do, in one line each)

- **GitHub Actions `on:`** stores the trigger as durable config; the platform watches for the
  event and starts a run. Nothing in a workflow waits for the next event.
- **omnigent** stores a scheduled task as a durable occurrence row and a scheduler fires it, stamping
  a run id into each request; the reaction is a record, not a waiting process.
- **opencode** uses an in-memory `Bus` (publish/subscribe) — ephemeral, needs a live process; this is
  the shape we do NOT copy, because control is a web app with a database, and nothing waits on it.

So an `on` is a **row on control plus control's existing check** (the per-second loop in
`cmd/agentbus/main.go`). This is what today's delivery already is (`queue.Deliver` + the `deliveries`
dedupe table): B13 generalises it, it does not invent a new mechanism.

## What is true today (file:line)

- A turn an agent asks for carries `links.asker` (asking session+turn) and `links.chain` (root,
  depth): `queue/asked.go:22,32`. `askedLinks` injects them for a turn token's POST
  (`api/tokens.go:320`).
- `queue.Deliver` scans ended jobs that have `links.asker`, starts a turn in the asking session with
  the result as its prompt, and records each delivery once in `deliveries` keyed by the job id
  (`queue/asked.go:202`). Called once a second from the control loop (`cmd/agentbus/main.go:1018`).
- The caps on asked work (`checkAsked`, `queue/asked.go:69`) read `links.asker` (per-turn count) and
  `links.chain` (depth, per-tree count).
- Token auth: `inTree` (`api/tokens.go:361`), route kinds, `allowTurn` (`api/tokens.go:159`).

## The split that makes this clean

`links.asker` does two unrelated jobs today:
1. **Provenance for the caps** — which turn asked, so `MaxPerTurn`/`MaxPerTree`/`MaxDepth` hold.
2. **The delivery target** — where `Deliver` sends the result.

B13 keeps (1) in core and moves (2) to the `on` row.

- **Keep** `links.chain` (root, depth) — the request-tree tracking the caps and `inTree`/`SessionRoot`
  need. Add the asking turn to it (`chain.by`) so `MaxPerTurn` still has its key without `asker`.
- **Retire** `links.asker` as a struct and `Deliver`. The delivery target is now the `on` row's own
  `session`.
- **Origin "asked"** is read from the token, not injected: a turn job whose creator is a turn token
  (it has a `chain` with depth ≥ 1) has origin "asked". The cockpit filters on this.

## The `on` primitive (queue)

Two tables, the same migration style as `deliveries`:

```
CREATE TABLE IF NOT EXISTS ons (
    id       TEXT PRIMARY KEY,   -- on_<hex>
    team     TEXT NOT NULL REFERENCES teams(id),
    event    TEXT NOT NULL,      -- B13: only 'turn.completed'
    subject  TEXT NOT NULL,      -- the turn id watched
    session  TEXT NOT NULL,      -- where the action starts its turn (the maker's own session)
    maker    TEXT NOT NULL,      -- JSON: who registered it (session, turn) — for the record
    off      INTEGER,            -- set when removed or fired; NULL = active
    created  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS on_fires (
    on_id   TEXT NOT NULL REFERENCES ons(id),
    key     TEXT NOT NULL,       -- one-shot: the subject turn id; keeps exactly-once
    job     TEXT NOT NULL DEFAULT '',
    error   TEXT NOT NULL DEFAULT '',
    created INTEGER NOT NULL,
    PRIMARY KEY (on_id, key)
);
```

B13 has one event (`turn.completed`) and one action (deliver the subject turn's reply to `session`),
so the action is implicit and needs no stored script. C1 widens `event`/adds an `action` column.

**One-shot, so it ends itself.** A `turn.completed` `on` fires once (its subject either completes or
is already ended) and is then set `off`. This is how "an `on` cannot set itself off forever" holds for
B13: there is no repeating `on` yet, so each one has a definite end. A repeating `on` (C3) must carry
an end or be ended by `off`.

**The check (`FireOns`) replaces `Deliver`** and is the same work: find active `ons` with
`event='turn.completed'` whose `subject` job has ended and that have not fired, build the delivery
turn exactly as `Deliver` does (same `ResultPrompt`, same `deliveredAgent`), record the fire once in
`on_fires`, set the `on` off. The per-second loop calls `FireOns` where it called `Deliver`.

## Public API

- `POST /api/v0/ons` — register. Body `{turn: <subject turn id>}`; the action (deliver to the caller's
  session) is implicit in B13. Route kind: a new `askedTurn`-like kind, `onRegister`.
- `DELETE /api/v0/ons/{id}` — remove (set `off`); the maker (or the main token) only.

**Token auth (the caller's token decides what it may do):**
- A **turn token** may register an `on` whose `subject` is a turn its tree started (reuse the
  `askedTurn` check: the subject turn's job is in the token's tree) and whose `session` is its own
  session. Anything else is refused with fixed wording.
- A **main token** may register an `on` for any turn.
- Remove: the maker's own token, or the main token.

## ask_agent as a pure API app

The app composes two public calls with the agent's own turn token, no special core path:
1. `POST /api/v0/turns` — start the asked turn. The handler no longer injects `asker`; it records
   `chain` (root, depth+1, by=asking turn) from the token. Origin "asked" follows from the token.
2. `POST /api/v0/ons` — register `on turn.completed` on the new turn, delivering to the caller's
   session.

The caps stay in core on step 1 (`checkAsked` reads `chain`). `links.asker` and `Deliver` retire once
this path is green.

## The asked session closes itself — DEFERRED to a follow-up

Deciding this from one delivered turn's change count in the delivery sweep closed sessions it should
not have: a person's session watched by a main-token `on`, a session whose diff was unreadable (which
also retires its workspace copy, so edits could be removed), a session whose later turn changed files,
and a session still owed a nested reply. Two review rounds found these in this one area, and they risk
data, so by the keep-data rule `FireOns` no longer closes anything. The safe form is a separate
"safe to retire" check: close only an asked session, every turn ended, with no unapplied changes
anywhere (an unreadable diff counts as changes) and no live `on` owed to it. That is a carded
follow-up with those four cases as its tests (his call, 2026-11-19). Until it lands, nothing
auto-closes.

## Cockpit

- The sessions list shows only sessions the person started (origin not "asked"): a view filter on the
  recorded origin, nothing hidden from the record.
- An ask shows inside the asking session with a link to the asked turn.

## Tests (fail first)

1. An ask end to end delivers exactly once.
2. It survives a control restart (the `on` row + `on_fires` dedupe, not in-memory state).
3. A main-token `on` (no asking turn) still delivers, on the subject's own agent.
Plus unit tests: `on` register/remove token auth at both a turn token and the main token; the rule
gives the same answer the old asker path did. (Session self-close and its two cases are the deferred
follow-up's tests, above.)

## Stages (one reviewed commit per repo per round)

1. Queue: the `on` tables, `RegisterOn`/`RemoveOn`/`FireOns`, `chain.by`; unit tests. (framework)
2. API: the two routes + token auth; ask_agent app rework; retire `asker`/`Deliver`; the loop calls
   `FireOns`; session self-close; end-to-end tests. (framework)
3. Cockpit: sessions-list filter + ask-inside-session view. (cockpit)
