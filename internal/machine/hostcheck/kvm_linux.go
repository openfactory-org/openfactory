//go:build linux

package hostcheck

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// kvmGetAPIVersion is the KVM_GET_API_VERSION ioctl.
const kvmGetAPIVersion = 0xAE00

func checkKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("no usable /dev/kvm: %w", err)
	}
	defer f.Close()
	v, err := unix.IoctlRetInt(int(f.Fd()), kvmGetAPIVersion) //nolint:gosec // G115: fd fits in int
	if err != nil {
		return fmt.Errorf("KVM_GET_API_VERSION: %w", err)
	}
	if v != 12 {
		return fmt.Errorf("KVM API version %d, want 12", v)
	}
	return nil
}

func uname() (string, string) {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return "", ""
	}
	return unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Machine[:])
}

func freeDisk(dir string) (int64, int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize, nil //nolint:gosec // G115: block counts fit
}
