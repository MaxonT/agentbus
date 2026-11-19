// Package machine is the runner's workspace adapter: a session's machine
// state — its working copy and its home — behind four methods, so the layer
// that holds that state can change without the runner changing. A folder copy
// holds it today; a microVM (Firecracker, Docker sbx) can hold it later as a
// second Adapter, with no caller change.
//
// The four methods are Create (bring the machine state into being), Snapshot
// (record it whole and return an id), Restore (bring it back to a snapshot)
// and Release (free it). The record that can recreate a released workspace
// lives on control, not here; Release frees only this machine's copy.
//
// Only a real adapter can record and restore a machine whole. The folder copy
// is the honest floor: it implements Create and Release — today's copy path,
// unchanged — and refuses Snapshot and Restore, because a file copy cannot
// faithfully capture or restore a machine (unreadable, immutable and special
// files, a running process). So no release-or-rebuild path is wired to any
// adapter until a real snapshotting one exists, and a rebuild then restores
// the machine whole rather than remaking it (his call, 2026-11-19).
//
// Who calls it: the runner, and only the runner. Control never holds an
// Adapter — it records and reads events (workspace.released, workspace.rebuilt)
// and each runner acts on its own machine and reports back, which is the
// architecture's rule that no part reaches into another. So an adapter call
// is always runner-side; its outcome reaches control as an event, never as a
// control-to-runner call.
//
// A SnapshotID is opaque: the adapter assigns it and only the adapter reads
// it. The runner stores it and hands it back to Restore; it never parses it.
// For a microVM it is that driver's own snapshot id. This is the border — the
// id's meaning stays inside the adapter.
package machine

// SnapshotID is an opaque handle to one recorded machine state. Only the
// adapter that made it understands it; a caller stores it and passes it back
// to Restore.
type SnapshotID string

// Spec is what a session's workspace is made from, in neutral terms: a folder
// on this machine (the local fast path), or a repo at a base commit (a
// worktree). The adapter translates it to whatever its layer needs. It is the
// small, stable set the session row already names; it is not tied to any one
// adapter's internals.
type Spec struct {
	// ID is the session the workspace belongs to; it names the copy and the
	// snapshot store.
	ID string
	// Folder is the person's folder, copied as it is. Repo and Base, instead,
	// make a worktree of Repo at commit Base. Ref is the folder's reference,
	// kept with the workspace.
	Folder     string
	Repo, Base string
	Ref        string
	// Isolation is the setting that chooses how the copy is made (auto, copy,
	// git, overlay, none); the adapter reads it only if its layer has a choice.
	Isolation string
}

// Workspace is the handle the runner holds between calls: where the copy is,
// where a turn runs, and where the session's home is. A real adapter may keep
// more of its own (a microVM would keep its VM id); the folder copy needs
// nothing beyond these.
type Workspace struct {
	// ID is the session.
	ID string
	// Root is the copy (or worktree). Dir is where a turn runs: Root, or the
	// same subfolder inside it that the source was inside its repo.
	Root string
	Dir  string
	// Home is the session's home on this runner, or empty when the session
	// has none.
	Home string
}

// Adapter owns the whole lifecycle of a session's machine state. One adapter
// makes a Workspace (Create), records and restores it whole (Snapshot,
// Restore) and frees it (Release); nothing above it makes, finds or frees a
// workspace by itself.
type Adapter interface {
	// Name is the adapter's name, for events and logs.
	Name() string
	// Create brings a session's machine state into being and returns a handle
	// to it. A Refusal says, in words for the person, why it cannot be made.
	Create(Spec) (*Workspace, error)
	// Snapshot records w's machine state whole and returns an id Restore
	// understands. A Refusal says why it cannot be recorded.
	Snapshot(w *Workspace) (SnapshotID, error)
	// Restore brings w back to the machine state a snapshot recorded.
	Restore(w *Workspace, id SnapshotID) error
	// Release frees w's machine state on this runner. The record that can
	// recreate it lives on control; Release removes only the copy and what the
	// adapter kept for it.
	Release(w *Workspace) error
}

// Refusal is a workspace operation that cannot be done, in words for the
// person. It never quotes a path.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }
