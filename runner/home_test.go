package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// ListHome lists the usual config paths a home holds, with sizes, and never
// a vendor sign-in: not by its name, not inside a folder, not through a
// link. A path of the wrong kind is left out.
func TestListHome(t *testing.T) {
	home := t.TempDir()
	for p, c := range map[string]string{
		".claude/settings.json": "{}", ".claude/agents/a.md": "12345", ".claude/agents/deep/b.md": "1",
		".claude/.credentials.json": "x", ".codex/auth.json": "x", ".gemini/antigravity-cli/antigravity-oauth-token": "x",
		".config/gh/hosts.yml": "abcd",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, p), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// ~/.gitconfig is a link to codex's sign-in; ~/.ssh/config is a folder.
	if err := os.Symlink(filepath.Join(home, ".codex", "auth.json"), filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := append(slices.Clone(settings.UsualHomeFiles), "~/.claude/.credentials.json", "~/.codex/auth.json", "~/.codex/")
	got := ListHome(home, paths)
	want := []queue.HomeFile{
		{Path: "~/.claude/settings.json", Kind: "file", Size: 2},
		{Path: "~/.claude/agents/", Kind: "folder", Size: 6, Files: 2},
		{Path: "~/.config/gh/", Kind: "folder", Size: 4, Files: 1},
		{Path: "~/.codex/", Kind: "folder", Size: 0, Files: 0},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListHome:\n%+v\nwant\n%+v", got, want)
	}
}
