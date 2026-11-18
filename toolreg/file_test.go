package toolreg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Open reads back what a registry on the same file wrote, and a missing file
// is an empty overlay.
func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetDefaults([]Tool{tool("list_turns"), tool("get_turn")})
	if _, err := r.Add(tool("my_tool")); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("get_turn"); err != nil {
		t.Fatal(err)
	}
	// A second registry opened on the same file.
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r2.SetDefaults([]Tool{tool("list_turns"), tool("get_turn")})
	if got := names(r2.List()); !reflect.DeepEqual(got, []string{"list_turns", "my_tool"}) {
		t.Fatalf("reloaded: %v", got)
	}
}

// A file that cannot be read is an error, not an empty overlay that would hide
// the person's tools.
func TestFileUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a broken overlay file should be an error")
	}
}
