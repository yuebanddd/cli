//go:build !windows

package mcpserver

import "golang.org/x/sys/unix"

func diskFree(path string) (int64, error) {
	var s unix.Statfs_t
	e := unix.Statfs(path, &s)
	return int64(s.Bavail) * int64(s.Bsize), e
}
