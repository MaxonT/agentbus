package toolreg

// The overlay on disk: tools.json in the data folder, where control keeps the
// tools the person added and the defaults they removed. The defaults
// themselves are in code, never written here.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// FileName is the file in the data folder that holds the overlay.
const FileName = "tools.json"

// file is the overlay as stored.
type file struct {
	Added   []Tool   `json:"added"`
	Removed []string `json:"removed,omitempty"`
}

// fileBackend keeps the overlay in a JSON file, written whole by atomic
// rename, as the other control-plane files are.
type fileBackend struct{ path string }

// Open returns a registry whose overlay is the file at path (read now, and
// written on every change). A missing file is an empty overlay. The caller
// sets the defaults with SetDefaults.
func Open(path string) (*Registry, error) {
	return New(&fileBackend{path})
}

func (b *fileBackend) Load() ([]Tool, []string, error) {
	data, err := os.ReadFile(b.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	}
	var f file
	if json.Unmarshal(data, &f) != nil {
		return nil, nil, errors.New("toolreg: " + filepath.Base(b.path) + " cannot be read")
	}
	return f.Added, f.Removed, nil
}

func (b *fileBackend) Save(added []Tool, removed []string) error {
	data, err := json.MarshalIndent(file{Added: added, Removed: removed}, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}
