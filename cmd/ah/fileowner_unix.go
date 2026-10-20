//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileOwnerUID returns the owner of fi when the platform reports it.
func fileOwnerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
