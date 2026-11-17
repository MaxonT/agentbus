package run

// NoHomes is why a session cannot be sandboxed when it has no home of its own
// (Config.Homes is empty, or a session from before homes): the wall needs one.
// The runner's sandbox report says it, and so does a turn's refusal.
const NoHomes = "this session has no home of its own, so it cannot be sandboxed"

// NoAgent is why a turn that names no agent is refused: every turn names
// the saved agent it runs on, a session's next turn too (his rule,
// 2026-11-12).
const NoAgent = "agent is required: name the saved agent this turn runs on (GET /api/v0/agents lists them)"
