package credentials

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A login folder a stopped control left, which may hold a sign-in, goes when
// control starts again; nothing else in the subscriptions folder does
// (0.3.1 codex relay, left).
func TestRemoveLeftLogins(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{".login-123", ".login-456/.codex", "agy-home"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".login-456/.codex/auth.json", "codex.json", ".login-file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{Store: &Subscriptions{Dir: dir}}
	if n := m.RemoveLeftLogins(); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	ents, _ := os.ReadDir(dir)
	var left []string
	for _, e := range ents {
		left = append(left, e.Name())
	}
	if want := []string{".login-file", "agy-home", "codex.json"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
	// A folder that is not there yet removes nothing.
	if n := (&Manager{Store: &Subscriptions{Dir: filepath.Join(dir, "none")}}).RemoveLeftLogins(); n != 0 {
		t.Errorf("no folder: %d", n)
	}
}
