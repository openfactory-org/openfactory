//go:build unix

package image

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the bytes available to unprivileged users
// on the filesystem of dir (statfs Bavail × Bsize).
func FreeBytes(dir string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)                                    //nolint:unconvert // Bsize is uint32 on darwin and int64 on linux
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil //nolint:gosec // G115: block counts fit
}

// DiskUsage returns the allocated bytes of a file (sparse files count
// only their blocks).
func DiskUsage(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512 //nolint:unconvert // Blocks differs by platform
	}
	return fi.Size()
}
