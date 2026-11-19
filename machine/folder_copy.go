package machine

// FolderCopy is today's adapter and the honest floor: a session's machine
// state is a working copy of the person's folder and a home. It can make that
// copy (Create) and remove it (Release) — exactly what the copy path does
// today — and it refuses Snapshot and Restore.
//
// It refuses them because a file copy cannot be made faithful: it silently
// omits an unreadable or a special file (a socket, a device), it cannot
// remove an immutable one, and it never sees a running process — so a
// copy-based snapshot is partial and a copy-based restore removes live files
// it could not record. Recording and restoring a machine whole is a real
// adapter's job (a microVM, a filesystem snapshot), which implements the same
// four methods. Until one exists, no release-or-rebuild path is wired to any
// adapter (his call, 2026-11-19).
//
// Make and Drop are today's copy-make and copy-remove — isolation.Open plus
// the engine's home, and isolation.Close plus RemoveHome — injected so the
// adapter wraps them rather than carry a second copy path, and so tests drive
// it over temp folders.
type FolderCopy struct {
	Make func(Spec) (*Workspace, error)
	Drop func(*Workspace) error
}

// Name is the adapter's name.
func (FolderCopy) Name() string { return "folder-copy" }

// Create makes the copy and home through Make.
func (fc FolderCopy) Create(sp Spec) (*Workspace, error) {
	if fc.Make == nil {
		return nil, &Refusal{Msg: "this runner cannot make a folder copy"}
	}
	return fc.Make(sp)
}

// Snapshot refuses: a folder copy cannot record a session's machine state
// whole, so it never records a partial one.
func (FolderCopy) Snapshot(*Workspace) (SnapshotID, error) {
	return "", &Refusal{Msg: "the folder copy cannot snapshot a session's machine state; a microVM workspace is what records it"}
}

// Restore refuses, for the reason Snapshot does: there is no folder-copy
// snapshot to restore, and replacing a live copy from a file copy cannot be
// made faithful.
func (FolderCopy) Restore(*Workspace, SnapshotID) error {
	return &Refusal{Msg: "the folder copy cannot restore a session's machine state; a microVM workspace is what records and restores it"}
}

// Release removes the session's copy and home through Drop — today's copy
// removal, unchanged. It frees only this machine's copy; the record that can
// recreate the session lives on control.
func (fc FolderCopy) Release(w *Workspace) error {
	if fc.Drop == nil {
		return nil
	}
	return fc.Drop(w)
}
