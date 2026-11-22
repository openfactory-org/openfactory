//go:build linux

package guest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// Ioctls of the random device.
const (
	rndAddEntropy  = 0x40085203 // _IOW('R', 0x03, int[2])
	rndReseedCRNG  = 0x5207     // _IO('R', 0x07)
	linkLocalTemp  = "169.254.0.2"
	reconnectLimit = 10 * time.Minute
)

// Main runs vm-init. It never returns on success: it reboots the VM.
func Main() int {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "vm-init runs only as PID 1 of a worker microVM")
		return 2
	}
	g := &initProc{}
	err := g.run()
	g.finish(err)
	return 1
}

type initProc struct {
	mu         sync.Mutex
	conn       *ctlstream.Conn
	key        string
	runnerPID  int
	cancel     cancelQueue
	tail       *ctlstream.Tail
	runnerDone chan unix.WaitStatus
	exitAck    chan struct{}
}

func (g *initProc) current() *ctlstream.Conn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conn
}

func (g *initProc) send(f ctlstream.Frame) error {
	c := g.current()
	if c == nil {
		return errors.New("no control stream")
	}
	return c.Write(f)
}

func mountAll(mounts [][4]string) error {
	for _, m := range mounts {
		if err := os.MkdirAll(m[1], 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m[0], m[1], m[2], 0, m[3]); err != nil && !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount %s on %s: %w", m[2], m[1], err)
		}
	}
	return nil
}

func dialVsock() (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: ctlstream.HostCID, Port: ctlstream.Port}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock"), nil
}

// connect dials the host with retries and sends hello.
func (g *initProc) connect(hello ctlstream.Frame, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		f, err := dialVsock()
		if err == nil {
			c := ctlstream.New(f)
			if err := c.Write(hello); err == nil {
				g.mu.Lock()
				g.conn = c
				g.mu.Unlock()
				return nil
			}
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control stream: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (g *initProc) run() error {
	if err := mountAll([][4]string{
		{"proc", "/proc", "proc", ""}, {"sysfs", "/sys", "sysfs", ""}, {"devtmpfs", "/dev", "devtmpfs", ""},
		{"tmpfs", "/run", "tmpfs", "mode=0755"}, {"tmpfs", "/mnt", "tmpfs", "mode=0755"},
	}); err != nil {
		return err
	}
	if null, err := os.OpenFile("/dev/null", os.O_RDWR, 0); err == nil {
		_ = unix.Dup3(int(null.Fd()), 0, 0)
		_ = unix.Dup3(int(null.Fd()), 1, 0)
		if os.Getenv("OF_VMINIT_DEBUG") == "" {
			_ = unix.Dup3(int(null.Fd()), 2, 0)
		}
		_ = null.Close()
	}
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF)
	g.cancel = newCancelQueue()
	g.exitAck = make(chan struct{}, 1)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigs {
			if s == syscall.SIGINT { // Ctrl-Alt-Del from the host
				g.cancel.push(10)
			}
		}
	}()

	// Hello, then wait for go. A stream that breaks before go means a
	// snapshot restore: reconnect and say "restored".
	if err := g.connect(ctlstream.Frame{Type: ctlstream.TypeHello, V: ctlstream.Version, VMInitVersion: Version, Phase: ctlstream.PhaseBoot}, 10*time.Second); err != nil {
		return err
	}
	var goFrame ctlstream.Frame
	for {
		f, err := g.current().Expect(ctlstream.TypeGo, nil)
		if err == nil {
			goFrame = f
			break
		}
		if err := g.connect(ctlstream.Frame{Type: ctlstream.TypeHello, V: ctlstream.Version, VMInitVersion: Version, Phase: ctlstream.PhaseRestored}, 30*time.Second); err != nil {
			return err
		}
	}
	g.key = goFrame.StreamKey

	// MMDS needs an on-link address.
	if err := ifUp("lo"); err != nil {
		return err
	}
	if err := ifUp("eth0"); err != nil {
		return err
	}
	if err := setAddr("eth0", netip.MustParseAddr(linkLocalTemp), 16); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	doc, err := mmds.Fetch(ctx, &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}, "http://"+mmds.Address)
	cancel()
	if err != nil {
		return err
	}
	if err := g.send(ctlstream.Frame{Type: ctlstream.TypeMMDSRead}); err != nil {
		return err
	}
	p := doc.OpenFactory

	// Entropy, clock, hostname.
	if raw, err := base64.StdEncoding.DecodeString(p.Entropy); err == nil && len(raw) > 0 {
		addEntropy(raw)
	}
	if p.ClockUnixNs > 0 {
		ts := unix.NsecToTimespec(p.ClockUnixNs)
		_ = unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
	}
	_ = unix.Sethostname([]byte(p.Hostname))

	if err := setupRoot(p); err != nil {
		return err
	}
	if n := p.Network; n != nil {
		if err := configureNet(n); err != nil {
			return err
		}
	}
	if err := pivot(); err != nil {
		return err
	}
	passwd, _ := os.ReadFile("/etc/passwd")
	uid, gid, home := ResolveUser(p.User, string(passwd))
	if uid == 0 && p.User != nil && p.User.UID != 0 {
		return errors.New("refusing to run the runner as root")
	}
	for _, d := range []string{home, Workspace, RunnerState} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // G703: home comes from the image passwd inside the VM
			return err
		}
		_ = os.Chown(d, uid, gid) //nolint:gosec // G703: as above
	}
	env := BuildEnv(p, home)
	return g.supervise(env, uid, gid, p.MaxSessionSeconds)
}

func addEntropy(raw []byte) {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	type poolInfo struct {
		Count int32
		Size  int32
		Buf   [64]byte
	}
	var info poolInfo
	n := copy(info.Buf[:], raw)
	info.Count = int32(n * 8)                                                                     //nolint:gosec // G115: at most 512 bits
	info.Size = int32(n)                                                                          //nolint:gosec // G115: at most 64
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&info))) //nolint:gosec // G103: RNDADDENTROPY takes a struct rand_pool_info
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0)
}
