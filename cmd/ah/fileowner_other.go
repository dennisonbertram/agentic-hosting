//go:build !unix

package main

import "io/fs"

// fileOwnerUID is unavailable on this platform.
func fileOwnerUID(fs.FileInfo) (uint32, bool) { return 0, false }
