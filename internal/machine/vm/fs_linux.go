//go:build linux

package vm

import (
	"bufio"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func bindReadOnly(src, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	_ = f.Close()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

func reflink(out, in *os.File) error {
	return unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) //nolint:gosec // G115: file descriptors fit in int
}

// unmountAll detaches every mount under root, deepest first.
func unmountAll(root string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return
	}
	defer f.Close()
	var mounts []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && (fields[4] == root || strings.HasPrefix(fields[4], root+"/")) {
			mounts = append(mounts, fields[4])
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mounts)))
	for _, m := range mounts {
		_ = unix.Unmount(m, unix.MNT_DETACH)
	}
}
