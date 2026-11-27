//go:build !linux

package main

import "os"

// openNoFollow falls back to a regular open on platforms without O_NOFOLLOW.
// The router build (linux/arm64) uses the strict variant in fileopen_linux.go.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
