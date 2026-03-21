//go:build darwin

package api

import "syscall"

// makeFakeStatfs constructs a Statfs_t for unit tests with the given total
// and free byte counts. Block size is fixed at 4096.
func makeFakeStatfs(totalBytes, freeBytes uint64) syscall.Statfs_t {
	const bsize = 4096
	return syscall.Statfs_t{
		Bsize:  uint32(bsize),
		Blocks: totalBytes / bsize,
		Bavail: freeBytes / bsize,
	}
}
