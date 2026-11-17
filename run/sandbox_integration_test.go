//go:build integration && (darwin || linux)

package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/sandbox"
	"github.com/staysup/agentbus/settings"
)

// canSandbox skips where this machine cannot build the wall.
func canSandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	box, err := sandbox.New(sandbox.Spec{Copy: dir, Home: dir, Net: netjail.Spec{Open: true}})
	if err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	box.Close()
}

// A turn with [sandbox] on runs in the wall: it writes its folder and its
// home, and cannot write beside its folder.
func TestSandboxTurn(t *testing.T) {
	canSandbox(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder, outside := filepath.Join(root, "folder"), filepath.Join(root, "outside")
	for _, d := range []string{folder, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `echo in > in.txt; echo home > "$HOME/home.txt"; echo out > ` + filepath.Join(outside, "turn.txt") + `; exit 0`
	e, _ := sandboxEngine(t, "[sandbox]\nmode = \"on\"\n", true, script)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	if _, err := os.Stat(filepath.Join(folder, "in.txt")); err != nil {
		t.Errorf("the turn could not write its folder")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Homes, turn.Session, "home.txt")); err != nil {
		t.Errorf("the turn could not write its home")
	}
	if _, err := os.Stat(filepath.Join(outside, "turn.txt")); err == nil {
		t.Errorf("the turn wrote outside its folder")
	}
}

// With edit = "none" the wall keeps the folder read only for codex, whose
// own read-only sandbox is off in it, and not for claude, which had none.
func TestSandboxEditNone(t *testing.T) {
	canSandbox(t)
	for _, c := range []struct {
		harness string
		writes  bool
	}{{"claude", true}, {"codex", false}} {
		t.Run(c.harness, func(t *testing.T) {
			e, _ := sandboxEngine(t, "edit = \"none\"\n[sandbox]\nmode = \"on\"\n", true, "echo in > in.txt")
			folder := t.TempDir()
			turn, err := e.Start(Request{Harness: c.harness, Folder: folder, Prompt: "hi"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-turn.Done()
			_, err = os.Stat(filepath.Join(folder, "in.txt"))
			if got := err == nil; got != c.writes {
				t.Errorf("wrote its folder: %t, want %t", got, c.writes)
			}
		})
	}
}

// home_files are copied into a session's own home at the start of each
// turn with the sandbox off, and with it on, where the turn reads them
// inside the wall. A file it does not list stays out (TestCopyGrants*
// cover the rest of the copy's rules).
func TestHomeFilesTurn(t *testing.T) {
	for _, sandboxOn := range []bool{false, true} {
		t.Run(map[bool]string{false: "sandbox off", true: "sandbox on"}[sandboxOn], func(t *testing.T) {
			policy := "home_files = [\"~/.aws/config\"]\n"
			if sandboxOn {
				canSandbox(t)
				policy += "[sandbox]\nmode = \"on\"\n"
			}
			user, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", user)
			if err := os.MkdirAll(filepath.Join(user, ".aws"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(user, ".aws", "config"), []byte("[default]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(user, ".aws", "credentials"), []byte("not granted\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e, _ := sandboxEngine(t, policy, true, `cat "$HOME/.aws/config" > "$HOME/got.txt"; exit 0`)
			turn, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-turn.Done()
			home := filepath.Join(e.cfg.Homes, turn.Session)
			if b, err := os.ReadFile(filepath.Join(home, "got.txt")); err != nil || string(b) != "[default]\n" {
				t.Fatalf("the turn read %q %v from its home", b, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".aws", "credentials")); err == nil {
				t.Error("a file not listed was copied")
			}
		})
	}
}

// A contained turn reads nothing under the real home by default: a tool kept
// there (standing in for ~/.nvm) is unreachable until its folder is listed in
// read_folders. His rule, 2026-11-17: nothing by default, explicit opt-in in
// Settings. The folder is a temp dir, which the wall denies by default
// (systemRead leaves out /private/var/folders), so "not granted" is the real
// default, not an artefact of where the home is.
func TestSandboxReadFoldersGrant(t *testing.T) {
	canSandbox(t)
	tool, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "data"), []byte("TOOLDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `cat "` + filepath.Join(tool, "data") + `" > "$HOME/got.txt" 2>/dev/null; exit 0`
	for _, c := range []struct {
		name, policy, want string
	}{
		{"not granted", "[sandbox]\nmode = \"on\"\n", ""},
		{"granted", "read_folders = [\"" + tool + "\"]\n[sandbox]\nmode = \"on\"\n", "TOOLDATA"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := sandboxEngine(t, c.policy, true, script)
			turn, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-turn.Done()
			b, _ := os.ReadFile(filepath.Join(e.cfg.Homes, turn.Session, "got.txt"))
			if string(b) != c.want {
				t.Fatalf("a %s tool read %q, want %q", c.name, b, c.want)
			}
		})
	}
}

// [network] contain on with the sandbox off, on a machine with no network
// namespace (macOS), runs the turn in the wall the runner builds for it:
// it writes its folder, cannot write beside it, and its proxy lets through
// the allow list only. Linux closes it off with netjail and no wall.
func TestContainOnBuildsWall(t *testing.T) {
	if !needsWall(settings.Policy{Network: settings.Network{Contain: settings.ContainOn}}) {
		t.Skip("this machine closes an agent off without the wall")
	}
	canSandbox(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder, outside := filepath.Join(root, "folder"), filepath.Join(root, "outside")
	for _, d := range []string{folder, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `echo in > in.txt; echo out > ` + filepath.Join(outside, "turn.txt") + `; echo "$HTTPS_PROXY" > proxy.txt; exit 0`
	e, _ := sandboxEngine(t, "[network]\ncontain = \"on\"\n", true, script)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	if _, err := os.Stat(filepath.Join(folder, "in.txt")); err != nil {
		t.Errorf("the turn could not write its folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "turn.txt")); err == nil {
		t.Errorf("the turn wrote outside its folder: no wall")
	}
	if b, err := os.ReadFile(filepath.Join(folder, "proxy.txt")); err != nil || len(b) < 2 {
		t.Errorf("the turn had no proxy: %q %v", b, err)
	}
}
