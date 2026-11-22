//go:build linux

package guest

import (
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

func ifUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("%s flags: %w", name, err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func setAddr(name string, ip netip.Addr, bits int) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	a := ip.As4()
	if err := ifr.SetInet4Addr(a[:]); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, ifr); err != nil {
		return fmt.Errorf("%s address: %w", name, err)
	}
	m := ^uint32(0) << (32 - bits)
	if err := ifr.SetInet4Addr([]byte{byte(m >> 24), byte(m >> 16), byte(m >> 8), byte(m)}); err != nil { //nolint:gosec // G115: bytes of a mask
		return err
	}
	return unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, ifr)
}

func setMTU(name string, mtu int) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	ifr.SetUint32(uint32(mtu)) //nolint:gosec // G115: MTU fits
	return unix.IoctlIfreq(fd, unix.SIOCSIFMTU, ifr)
}

type sockaddrIn struct {
	Family uint16
	Port   uint16
	Addr   [4]byte
	Zero   [8]byte
}

// rtentry is struct rtentry of <net/route.h> on 64-bit Linux.
type rtentry struct {
	Pad1    uint64
	Dst     sockaddrIn
	Gateway sockaddrIn
	Genmask sockaddrIn
	Flags   uint16
	Pad2    int16
	_       [4]byte
	Pad3    uint64
	Pad4    uintptr
	Metric  int16
	_       [6]byte
	Dev     uintptr
	MTU     uint64
	Window  uint64
	IRTT    uint16
	_       [6]byte
}

func addDefaultRoute(gw netip.Addr) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	rt := rtentry{
		Dst:     sockaddrIn{Family: unix.AF_INET},
		Gateway: sockaddrIn{Family: unix.AF_INET, Addr: gw.As4()},
		Genmask: sockaddrIn{Family: unix.AF_INET},
		Flags:   unix.RTF_UP | unix.RTF_GATEWAY,
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCADDRT, uintptr(unsafe.Pointer(&rt))); e != 0 && e != unix.EEXIST { //nolint:gosec // G103: SIOCADDRT takes a struct rtentry
		return fmt.Errorf("default route: %w", e)
	}
	return nil
}

func configureNet(n *mmds.Network) error {
	pfx, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return fmt.Errorf("network address %q: %w", n.Address, err)
	}
	if err := setAddr("eth0", pfx.Addr(), pfx.Bits()); err != nil {
		return err
	}
	if n.MTU > 0 {
		_ = setMTU("eth0", n.MTU)
	}
	gw, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("gateway %q: %w", n.Gateway, err)
	}
	return addDefaultRoute(gw)
}
