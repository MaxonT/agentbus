//go:build !darwin && !linux

package api

import (
	"errors"
	"os"
)

func fdPath(f *os.File) (string, error) {
	return "", errors.New("fdPath not supported on this platform")
}
