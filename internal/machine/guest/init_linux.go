//go:build linux

package guest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
	cancelCh   chan int
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
	runtime.LockOSThread()
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
	}
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF)
	g.cancelCh = make(chan int, 4)
	g.exitAck = make(chan struct{}, 1)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigs {
			if s == syscall.SIGINT { // Ctrl-Alt-Del from the host
				g.cancelCh <- 10
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

// setupRoot mounts the image, the scratch overlay, the tools and the cache
// disks under /mnt/root and writes the files of /etc.
func setupRoot(p mmds.Payload) error {
	d := p.Disks
	if d == nil {
		return errors.New("the document names no disks")
	}
	drives := []string{d.Image, d.Scratch}
	for _, c := range d.Cache {
		drives = append(drives, c.Drive)
	}
	devs := MapDisks("/sys/block", drives)
	img, scr := devs[d.Image], devs[d.Scratch]
	if img == "" || scr == "" {
		return fmt.Errorf("image or scratch disk not found (%v)", devs)
	}
	steps := []struct {
		src, dst, fs string
		flags        uintptr
		data         string
	}{
		{img, "/mnt/lower", "ext4", unix.MS_RDONLY | unix.MS_NODEV, "noload"},
		{scr, "/mnt/scratch", "ext4", unix.MS_NODEV | unix.MS_NOSUID, "discard"},
	}
	for _, s := range steps {
		if err := os.MkdirAll(s.dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(s.src, s.dst, s.fs, s.flags, s.data); err != nil {
			return fmt.Errorf("mount %s: %w", s.dst, err)
		}
	}
	for _, dir := range []string{"/mnt/scratch/upper", "/mnt/scratch/work", "/mnt/root"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("overlay", "/mnt/root", "overlay", unix.MS_NODEV|unix.MS_NOSUID,
		"lowerdir=/mnt/lower,upperdir=/mnt/scratch/upper,workdir=/mnt/scratch/work"); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	r := "/mnt/root"
	if err := os.MkdirAll(r+ToolsMount, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("/", r+ToolsMount, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind tools: %w", err)
	}
	_ = unix.Mount("", r+ToolsMount, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	for _, c := range d.Cache {
		dev := devs[c.Drive]
		if dev == "" {
			continue
		}
		dst := filepath.Join(r, GitCacheMount, c.Key+".git")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(dev, dst, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOEXEC, "noload"); err != nil {
			return fmt.Errorf("mount git cache %s: %w", c.Key, err)
		}
	}
	if err := mountAll([][4]string{
		{"proc", r + "/proc", "proc", ""},
		{"sysfs", r + "/sys", "sysfs", ""},
		{"devtmpfs", r + "/dev", "devtmpfs", ""},
		{"devpts", r + "/dev/pts", "devpts", "newinstance,ptmxmode=0666"},
		{"tmpfs", r + "/dev/shm", "tmpfs", "nosuid,nodev"},
		{"tmpfs", r + "/run", "tmpfs", "nosuid,nodev"},
		{"tmpfs", r + "/tmp", "tmpfs", "nosuid,nodev,size=25%"},
	}); err != nil {
		return err
	}
	dns := []string{}
	if p.Network != nil {
		dns = p.Network.DNS
	}
	files := map[string]string{
		"/etc/hostname":    p.Hostname + "\n",
		"/etc/hosts":       Hosts(p.Hostname),
		"/etc/resolv.conf": ResolvConf(dns),
		"/etc/machine-id":  p.MachineID + "\n",
	}
	_ = os.MkdirAll(r+"/etc", 0o755)
	for f, data := range files {
		_ = os.Remove(r + f)
		if err := os.WriteFile(r+f, []byte(data), 0o644); err != nil { //nolint:gosec // G306: world-readable system files
			return err
		}
	}
	if len(d.Cache) > 0 {
		gc, _ := os.OpenFile(r+"/etc/gitconfig", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302: world-readable git config
		if gc != nil {
			_, _ = gc.WriteString("[safe]\n\tdirectory = *\n")
			_ = gc.Close()
		}
	}
	return nil
}

func pivot() error {
	if err := os.Chdir("/mnt/root"); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach the old root: %w", err)
	}
	return os.Chdir("/")
}

// supervise starts the runner, reaps children, serves the control stream
// and reports the exit.
func (g *initProc) supervise(env []string, uid, gid, maxSession int) error {
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 65536, Max: 65536})
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	g.tail = ctlstream.NewTail(4096)
	go func() { _, _ = io.Copy(g.tail, pr) }()
	null, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	pid, err := syscall.ForkExec(RunnerBinary, []string{"openfactory", "runner"}, &syscall.ProcAttr{
		Dir: Workspace, Env: env, Files: []uintptr{null.Fd(), null.Fd(), pw.Fd()},
		Sys: &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, Setsid: true}, //nolint:gosec // G115: uids fit
	})
	_ = pw.Close()
	if err != nil {
		return fmt.Errorf("start the runner: %w", err)
	}
	g.runnerPID = pid
	_ = g.send(ctlstream.Frame{Type: ctlstream.TypeRunnerStarted, PID: pid})

	g.runnerDone = make(chan unix.WaitStatus, 1)
	go g.reap()
	go g.readLoop()
	var deadline <-chan time.Time
	if maxSession > 0 {
		deadline = time.After(time.Duration(maxSession+60) * time.Second)
	}
	var kill <-chan time.Time
	for {
		select {
		case ws := <-g.runnerDone:
			g.report(ws)
			return nil
		case grace := <-g.cancelCh:
			_ = unix.Kill(pid, unix.SIGTERM)
			if grace <= 0 {
				_ = unix.Kill(-1, unix.SIGKILL)
			}
			kill = time.After(time.Duration(grace) * time.Second)
		case <-deadline:
			_ = unix.Kill(pid, unix.SIGTERM)
			kill = time.After(30 * time.Second)
		case <-kill:
			_ = unix.Kill(-1, unix.SIGKILL)
		}
	}
}

// reap waits for every child; the status of the runner ends supervise.
func (g *initProc) reap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if pid == g.runnerPID {
			g.runnerDone <- ws
		}
	}
}

// readLoop serves host frames and reconnects after a broken stream (an
// agent restart): the new agent accepts the resume hello with the key.
func (g *initProc) readLoop() {
	for {
		c := g.current()
		for {
			f, err := c.Read()
			if err != nil {
				break
			}
			switch f.Type {
			case ctlstream.TypeCancel:
				g.cancelCh <- f.GraceSeconds
			case ctlstream.TypeExitAck:
				select {
				case g.exitAck <- struct{}{}:
				default:
				}
			}
		}
		_ = c.Close()
		g.mu.Lock()
		if g.conn == c {
			g.conn = nil
		}
		g.mu.Unlock()
		if err := g.connect(ctlstream.Frame{Type: ctlstream.TypeHello, V: ctlstream.Version, VMInitVersion: Version, Phase: ctlstream.PhaseResume, StreamKey: g.key, Running: true}, reconnectLimit); err != nil {
			return
		}
	}
}

// report ends every process, sends the exit frame and waits for the ack.
func (g *initProc) report(ws unix.WaitStatus) {
	_ = unix.Kill(-1, unix.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = unix.Kill(-1, unix.SIGKILL)
	vmstat, _ := os.ReadFile("/proc/vmstat")
	f := ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseRunner, OOMKills: OOMKills(string(vmstat)), OutputTail: g.tail.String()}
	if ws.Signaled() {
		f.Signal = int(ws.Signal())
	} else {
		code := ws.ExitStatus()
		f.ExitCode = &code
	}
	g.deliver(f)
}

// deliver sends the exit frame until the host acknowledges it, also across
// a reconnect, for at most reconnectLimit.
func (g *initProc) deliver(f ctlstream.Frame) {
	deadline := time.Now().Add(reconnectLimit)
	for time.Now().Before(deadline) {
		if g.send(f) == nil {
			select {
			case <-g.exitAck:
				return
			case <-time.After(2 * time.Second):
			}
		} else {
			time.Sleep(time.Second)
		}
	}
}

// finish reports an init error and reboots. With reboot=k the guest
// reboot ends Firecracker on both architectures.
func (g *initProc) finish(err error) {
	if err != nil {
		msg := err.Error()
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		if g.current() != nil {
			if g.exitAck == nil {
				g.exitAck = make(chan struct{}, 1)
			}
			if g.runnerPID == 0 { // no read loop yet: read the ack here
				go func() {
					c := g.current()
					if c != nil {
						if _, err := c.Expect(ctlstream.TypeExitAck, nil); err == nil {
							g.exitAck <- struct{}{}
						}
					}
				}()
			}
			_ = g.send(ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseInit, Error: strings.TrimSpace(msg)})
			select {
			case <-g.exitAck:
			case <-time.After(2 * time.Second):
			}
		}
	}
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}
