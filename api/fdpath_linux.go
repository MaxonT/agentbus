//go:build linux

package api

import (
	"fmt"
	"os"
)

func fdPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
