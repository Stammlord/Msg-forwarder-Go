//go:build linux

package main

import (
	"os"
	"syscall"
)

// openNoFollow refuses to open a symlink, so a compromised service account
// cannot redirect debug files to an arbitrary path inside the config dir.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}
