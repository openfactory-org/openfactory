//go:build linux

package image

import (
	"archive/tar"
	"os"

	"golang.org/x/sys/unix"
)

func mknod(dst string, h *tar.Header) error {
	_ = os.Remove(dst)
	mode := uint32(h.Mode & 0o777) //nolint:gosec // G115: tar modes fit
	switch h.Typeflag {
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	default:
		mode |= unix.S_IFIFO
	}
	return unix.Mknod(dst, mode, int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))) //nolint:gosec // G115: device numbers fit
}
