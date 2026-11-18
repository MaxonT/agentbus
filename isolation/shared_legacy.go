package isolation

// Legacy shared copies. Before 0.6, the share_read_only setting linked a
// session's copy to one read-only shared copy of a big folder (such as
// node_modules) under .isolation/.shared, so copies of the same folder state
// shared it. The setting is retired (settings/defaults.go): no new shared copy
// is ever made, and a copy now clones the whole folder copy-on-write. An
// install that used the setting may still have shared copies left, referenced
// by the space.json of sessions opened before the upgrade; gcLegacyShared
// removes one once no remaining space references it, the cleanup those copies
// had through the removed gcShared. A folder that never used the setting has no
// .shared folder, so the common path reads one directory and stops.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const legacySharedFolder = ".shared"

// legacySharedMu keeps the legacy cleanup to one change at a time, as the
// shared copies' own lock once did.
var legacySharedMu sync.Mutex

// legacyShared reads the retired "shared" array a pre-0.6 space.json still
// holds; the Space struct no longer has the field.
type legacyShared struct {
	Shared []string `json:"shared"`
}

// gcLegacyShared removes the shared copies under .isolation/.shared that no
// remaining space references. Best-effort: it leaves copies in place rather
// than remove one it is unsure of, and a folder that never used the setting
// returns at once.
func gcLegacyShared(workdir string) {
	legacySharedMu.Lock()
	defer legacySharedMu.Unlock()
	dir := filepath.Join(workdir, stateFolder, legacySharedFolder)
	kept, err := os.ReadDir(dir)
	if err != nil {
		return // no legacy shared copies: the folder never used the setting
	}
	used := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(workdir, stateFolder))
	for _, e := range entries {
		if !e.IsDir() || !spaceName.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(workdir, stateFolder, e.Name(), "space.json"))
		if err != nil {
			continue
		}
		var s legacyShared
		if json.Unmarshal(b, &s) == nil {
			for _, d := range s.Shared {
				used[d] = true
			}
		}
	}
	for _, e := range kept {
		if p := filepath.Join(dir, e.Name()); !used[p] {
			_ = removeAll(p)
		}
	}
}
