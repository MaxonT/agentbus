package machine

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/staysup/agentbus/isolation"
)

// FolderCopy is an Adapter.
var _ Adapter = FolderCopy{}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// copier makes a FolderCopy whose Make and Drop stand in for isolation plus
// the engine's home, over temp folders: Make returns a prepared copy and home,
// Drop removes them.
func copier(t *testing.T) (FolderCopy, Spec) {
	t.Helper()
	base := t.TempDir()
	copyRoot := filepath.Join(base, "copy")
	home := filepath.Join(base, "home")
	writeFile(t, copyRoot, "code.txt", "v1")
	writeFile(t, home, "config", "home-v1")
	fc := FolderCopy{
		Make: func(sp Spec) (*Workspace, error) {
			return &Workspace{ID: sp.ID, Root: copyRoot, Dir: copyRoot, Home: home}, nil
		},
		Drop: func(w *Workspace) error {
			return errors.Join(os.RemoveAll(w.Root), os.RemoveAll(w.Home))
		},
	}
	return fc, Spec{ID: "sess_1", Folder: base}
}

func TestCreateAndRelease(t *testing.T) {
	fc, sp := copier(t)
	w, err := fc.Create(sp)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != "sess_1" || w.Root == "" || w.Home == "" {
		t.Fatalf("workspace not set up: %+v", w)
	}
	if read(t, filepath.Join(w.Root, "code.txt")) != "v1" {
		t.Fatal("the copy does not hold the file")
	}
	if err := fc.Release(w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatal("release left the copy")
	}
	if _, err := os.Stat(w.Home); !os.IsNotExist(err) {
		t.Fatal("release left the home")
	}
}

func TestCreateNoMakeRefuses(t *testing.T) {
	if _, err := (FolderCopy{}).Create(Spec{ID: "s"}); !isRefusal(err) {
		t.Fatalf("want refusal, got %v", err)
	}
}

// TestSnapshotAndRestoreRefuse: the folder copy refuses both, whatever it is
// asked, so there is no partial snapshot and no copy-based restore.
func TestSnapshotAndRestoreRefuse(t *testing.T) {
	fc, sp := copier(t)
	w, err := fc.Create(sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Snapshot(w); !isRefusal(err) {
		t.Fatalf("snapshot: want refusal, got %v", err)
	}
	for _, id := range []SnapshotID{"", "snap_abc", "../../outside", "snap_0123456789abcdef"} {
		if err := fc.Restore(w, id); !isRefusal(err) {
			t.Fatalf("restore %q: want refusal, got %v", id, err)
		}
	}
}

// TestSnapshotAndRestoreTouchNothing proves every earlier data-loss finding is
// impossible now: a tree holding each hazard the review rounds used (a
// read-only directory, an unreadable file, a special file) is left exactly as
// it was, because Snapshot and Restore do nothing but refuse.
func TestSnapshotAndRestoreTouchNothing(t *testing.T) {
	fc, sp := copier(t)
	w, err := fc.Create(sp)
	if err != nil {
		t.Fatal(err)
	}
	// A read-only directory (round 1: restore wrecked the live tree on one).
	writeFile(t, filepath.Join(w.Root, "cache"), "m.txt", "pkg")
	if err := os.Chmod(filepath.Join(w.Root, "cache"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(w.Root, "cache"), 0o700) })
	// An unreadable file (round 1: dropped from a snapshot, deleted on restore).
	secret := filepath.Join(w.Root, "secret.txt")
	writeFile(t, w.Root, "secret.txt", "x")
	if err := os.Chmod(secret, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(secret, 0o600) })
	// A special file (round 1/2: an irregular entry copyTree silently skips).
	if err := syscall.Mkfifo(filepath.Join(w.Root, "pipe"), 0o600); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	before := treeNames(t, w.Root)
	if _, err := fc.Snapshot(w); !isRefusal(err) {
		t.Fatalf("snapshot: want refusal, got %v", err)
	}
	if err := fc.Restore(w, "snap_0123456789abcdef"); !isRefusal(err) {
		t.Fatalf("restore: want refusal, got %v", err)
	}
	if after := treeNames(t, w.Root); after != before {
		t.Fatalf("snapshot/restore changed the tree:\nbefore %q\nafter  %q", before, after)
	}
	if read(t, filepath.Join(w.Home, "config")) != "home-v1" {
		t.Fatal("the home changed")
	}
}

// treeNames is the sorted set of entries under root, by relative path, read
// without opening file contents (so an unreadable file still lists).
func treeNames(t *testing.T, root string) string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		names = append(names, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// TestWrapsIsolation drives Create and Release with real isolation (the copy
// backend), proving the adapter wraps today's copy path: a real copy is made
// from the folder and removed, and the person's folder is never touched.
func TestWrapsIsolation(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "folder")
	workdir := filepath.Join(base, "workdir")
	home := filepath.Join(base, "home")
	writeFile(t, folder, "code.txt", "v1")
	writeFile(t, home, "config", "home-v1")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}

	var space *isolation.Space
	fc := FolderCopy{
		Make: func(sp Spec) (*Workspace, error) {
			b, ok := isolation.For(sp.Isolation, sp.Folder)
			if !ok {
				return nil, &Refusal{Msg: "no backend"}
			}
			s, err := b.Open(isolation.Request{ID: sp.ID, Workdir: workdir, Folder: sp.Folder, Ref: sp.Ref})
			if err != nil {
				return nil, err
			}
			space = s
			return &Workspace{ID: sp.ID, Root: s.Root, Dir: s.Dir, Home: home}, nil
		},
		Drop: func(w *Workspace) error {
			return errors.Join(isolation.Close(space), os.RemoveAll(w.Home))
		},
	}

	w, err := fc.Create(Spec{ID: "sess_iso", Folder: folder, Isolation: isolation.Copy})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(w.Root, "code.txt")) != "v1" {
		t.Fatal("the copy does not hold the folder's file")
	}
	if err := fc.Release(w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatal("release left the copy")
	}
	if got := read(t, filepath.Join(folder, "code.txt")); got != "v1" {
		t.Fatalf("the person's folder changed: %q", got)
	}
}
