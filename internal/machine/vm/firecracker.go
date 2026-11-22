package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// TemplateMAC is the MAC of every VM restored from a snapshot template.
// Each tap is its own link, so clones may share it.
var TemplateMAC = net.HardwareAddr{0x06, 0x00, 0xa9, 0xfe, 0x00, 0x02}

// ErrGuestStart means vm-init did not say hello in time.
var ErrGuestStart = errors.New("guest did not start")

// Firecracker is the VMM over the jailer and the Firecracker API.
type Firecracker struct {
	Jail    JailerConfig
	Cgroups Cgroups
	Log     *slog.Logger
	// NewAPI returns the API client of a socket. Nil means NewClient.
	NewAPI func(socket string) API
	// RunJailer runs the jailer command and returns when it exited (it
	// daemonizes Firecracker). Nil means os/exec.
	RunJailer func(ctx context.Context, args []string) error
	// Chown changes the owner of a jail file. Nil means os.Lchown.
	Chown func(path string, uid, gid int) error
	// HelloTimeout bounds the wait for vm-init (30 s).
	HelloTimeout time.Duration
	// SocketTimeout bounds the wait for the API socket (5 s).
	SocketTimeout time.Duration
	// Alive reports whether a process runs. Nil means signal 0.
	Alive func(pid int) bool
}

func (f *Firecracker) log() *slog.Logger {
	if f.Log == nil {
		return slog.Default()
	}
	return f.Log
}

func (f *Firecracker) api(socket string) API {
	if f.NewAPI != nil {
		return f.NewAPI(socket)
	}
	return NewClient(socket)
}

func (f *Firecracker) chown(path string, uid, gid int) error {
	if f.Chown != nil {
		return f.Chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}

func (f *Firecracker) alive(pid int) bool {
	if f.Alive != nil {
		return f.Alive(pid)
	}
	return processAlive(pid)
}

func (f *Firecracker) runJailer(ctx context.Context, args []string) error {
	if f.RunJailer != nil {
		return f.RunJailer(ctx, args)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // G204: the pinned jailer with agent-built arguments
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jailer: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// prepare creates the jail directory with the disks and the logs.
func (f *Firecracker) prepare(s Spec) (string, error) {
	root := f.Jail.Root(s.VMID)
	_ = os.RemoveAll(f.Jail.JailDir(s.VMID)) // a stale jail of a crashed start
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		return "", err
	}
	links := map[string]string{"vmlinux": s.Kernel, "tools.ext4": s.Tools}
	if s.Image != "" {
		links["rootfs.ext4"] = s.Image
	}
	for i, c := range s.Cache {
		links[fmt.Sprintf("cache-%d.ext4", i)] = c
	}
	if s.Snapshot != nil {
		links["vm.state"] = s.Snapshot.State
		links["mem"] = s.Snapshot.Mem
	}
	for name, src := range links {
		if err := linkReadOnly(src, filepath.Join(root, name)); err != nil {
			return "", fmt.Errorf("jail %s: %w", name, err)
		}
	}
	scratch := s.Scratch
	if s.Snapshot != nil && s.Snapshot.Scratch != "" {
		scratch = s.Snapshot.Scratch
	}
	if scratch != "" {
		dst := filepath.Join(root, "scratch.ext4")
		if err := CloneFile(scratch, dst); err != nil {
			return "", fmt.Errorf("scratch disk: %w", err)
		}
		if err := os.Chmod(dst, 0o600); err != nil {
			return "", err
		}
		if err := f.chown(dst, s.UID, s.GID); err != nil {
			return "", err
		}
	}
	logPath := filepath.Join(root, "fc.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		return "", err
	}
	if err := f.chown(logPath, s.UID, s.GID); err != nil {
		return "", err
	}
	return root, nil
}

// listen opens the host listener of the guest control stream.
func (f *Firecracker) listen(s Spec, root string) (net.Listener, error) {
	path := filepath.Join(root, fmt.Sprintf("v.sock_%d", ctlstream.Port))
	_ = os.Remove(path)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control stream listener: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := f.chown(path, s.UID, s.GID); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func waitSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d := net.Dialer{Timeout: 200 * time.Millisecond}
		c, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket did not appear: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Start boots or restores a VM.
func (f *Firecracker) Start(ctx context.Context, s Spec) (VM, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	root, err := f.prepare(s)
	if err != nil {
		f.removeJail(s.VMID)
		return nil, err
	}
	ln, err := f.listen(s, root)
	if err != nil {
		f.removeJail(s.VMID)
		return nil, err
	}
	v := f.newVM(s, ln)
	fail := func(err error) (VM, error) {
		_ = v.Cleanup(context.Background())
		return nil, err
	}
	if err := f.runJailer(ctx, f.Jail.Args(s)); err != nil {
		return fail(err)
	}
	pid, err := f.Jail.ReadPID(s.VMID)
	if err != nil {
		return fail(err)
	}
	v.pid = pid
	sock := filepath.Join(root, "run", "firecracker.socket")
	st := f.SocketTimeout
	if st <= 0 {
		st = 5 * time.Second
	}
	if err := waitSocket(ctx, sock, st); err != nil {
		return fail(err)
	}
	v.api = f.api(sock)
	if s.Snapshot != nil {
		err = f.restore(ctx, v)
	} else {
		err = f.boot(ctx, v)
	}
	if err != nil {
		return fail(err)
	}
	ht := f.HelloTimeout
	if ht <= 0 {
		ht = 30 * time.Second
	}
	hello, err := v.acceptBoot(ht)
	if err != nil {
		return fail(err)
	}
	// vm-init cannot tell a template boot from a session boot before it
	// reads MMDS, so both say "boot". After a restore its stream broke
	// before go, so it says "restored".
	want := ctlstream.PhaseBoot
	if s.Snapshot != nil {
		want = ctlstream.PhaseRestored
	}
	if hello.Phase != want {
		return fail(fmt.Errorf("%w: vm-init said hello with phase %q, want %q", ErrGuestStart, hello.Phase, want))
	}
	go v.watch()
	return v, nil
}

func (f *Firecracker) boot(ctx context.Context, v *fcVM) error {
	s := v.spec
	a := v.api
	steps := []func() error{
		func() error {
			return a.PutMachineConfig(ctx, MachineConfig{VCPUCount: s.VCPUs, MemSizeMiB: s.MemoryMiB, HugePages: "None"})
		},
		func() error {
			return a.PutBootSource(ctx, BootSource{KernelImagePath: "vmlinux", BootArgs: BootArgs(s.Debug, s.Arch)})
		},
		func() error {
			return a.PutDrive(ctx, Drive{DriveID: "tools", PathOnHost: "tools.ext4", IsRootDevice: true, IsReadOnly: true})
		},
	}
	if s.Image != "" {
		steps = append(steps, func() error {
			return a.PutDrive(ctx, Drive{DriveID: "image", PathOnHost: "rootfs.ext4", IsReadOnly: true, RateLimiter: s.DiskRate.limiter(20000)})
		})
	}
	steps = append(steps, func() error {
		return a.PutDrive(ctx, Drive{
			DriveID: "scratch", PathOnHost: "scratch.ext4", CacheType: "Unsafe", Discard: true, IOEngine: "Sync",
			RateLimiter: s.DiskRate.limiter(20000),
		})
	})
	for i := range s.Cache {
		id := fmt.Sprintf("cache-%d", i)
		steps = append(steps, func() error {
			return a.PutDrive(ctx, Drive{DriveID: id, PathOnHost: id + ".ext4", IsReadOnly: true})
		})
	}
	steps = append(steps,
		func() error {
			return a.PutNetworkInterface(ctx, NetworkInterface{
				IfaceID: "eth0", HostDevName: s.Tap, GuestMAC: s.GuestMAC.String(), MTU: 1500,
				TxRateLimiter: s.NetTx.limiter(0), RxRateLimiter: s.NetRx.limiter(0),
			})
		},
		func() error {
			return a.PutMMDSConfig(ctx, MMDSConfig{Version: "V2", NetworkInterfaces: []string{"eth0"}, IPv4Address: mmds.Address})
		},
		func() error { return a.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "v.sock"}) },
		func() error {
			return a.PutEntropy(ctx, Entropy{RateLimiter: &RateLimiter{Bandwidth: &TokenBucket{Size: 4096, RefillTime: 100}}})
		},
		func() error {
			doc := mmds.Document{OpenFactory: mmds.Payload{V: mmds.Version}}
			if s.Template {
				doc = mmds.Template()
			}
			return a.PutMMDS(ctx, doc)
		},
		func() error { return a.Action(ctx, ActionInstanceStart) },
	)
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (f *Firecracker) restore(ctx context.Context, v *fcVM) error {
	load := SnapshotLoad{
		SnapshotPath:     "vm.state",
		MemBackend:       MemBackend{BackendType: "File", BackendPath: "mem"},
		NetworkOverrides: []NetworkOverride{{IfaceID: "eth0", HostDevName: v.spec.Tap}},
	}
	if err := v.api.LoadSnapshot(ctx, load); err != nil {
		return err
	}
	// The data store is not in the snapshot; Go writes the session document.
	return v.api.PatchVM(ctx, StateResumed)
}

// Adopt finds the VMs of an earlier agent process whose Firecracker still
// runs, and listens for the reconnect of their vm-init.
func (f *Firecracker) Adopt(_ context.Context, recs []AdoptRecord) ([]VM, error) {
	var out []VM
	for _, r := range recs {
		pid, err := f.Jail.ReadPID(r.VMID)
		if err != nil || !f.alive(pid) {
			continue
		}
		root := f.Jail.Root(r.VMID)
		ln, err := f.listen(r.Spec, root)
		if err != nil {
			f.log().Warn("adopt: listen", "vm", r.VMID, "err", err)
			continue
		}
		s := r.Spec
		s.VMID = r.VMID
		v := f.newVM(s, ln)
		v.pid = pid
		v.key = r.StreamKey
		v.api = f.api(filepath.Join(root, "run", "firecracker.socket"))
		// The runner was running when the agent stopped.
		v.runningOnce.Do(func() { close(v.running) })
		go v.acceptResume()
		go v.watch()
		out = append(out, v)
	}
	return out, nil
}

func (f *Firecracker) removeJail(vmID string) {
	unmountAll(f.Jail.Root(vmID))
	_ = os.RemoveAll(f.Jail.JailDir(vmID))
}

func (f *Firecracker) newVM(s Spec, ln net.Listener) *fcVM {
	return &fcVM{
		f: f, spec: s, ln: ln,
		cg:       f.Jail.CgroupDir(s.VMID),
		running:  make(chan struct{}),
		mmdsRead: make(chan struct{}, 1),
		exit:     make(chan Exit, 1),
		gone:     make(chan struct{}),
	}
}

type fcVM struct {
	f    *Firecracker
	spec Spec
	api  API
	pid  int
	cg   string

	mu      sync.Mutex
	ln      net.Listener
	conn    *ctlstream.Conn
	key     string
	report  *ctlstream.Frame
	killed  bool
	running chan struct{}
	// runningOnce guards running.
	runningOnce sync.Once
	mmdsRead    chan struct{}
	exit        chan Exit
	gone        chan struct{}
	goneOnce    sync.Once
	cleanOnce   sync.Once
	cleanErr    error
}

func (v *fcVM) ID() string               { return v.spec.VMID }
func (v *fcVM) Running() <-chan struct{} { return v.running }
func (v *fcVM) Wait() <-chan Exit        { return v.exit }

func (v *fcVM) StreamKey() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.key
}

func (v *fcVM) closeListener() {
	v.mu.Lock()
	ln := v.ln
	v.ln = nil
	v.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// acceptBoot accepts the one connection of vm-init after a boot or a
// restore and reads its hello. No agent code runs in the guest yet.
func (v *fcVM) acceptBoot(timeout time.Duration) (ctlstream.Frame, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	v.mu.Lock()
	ln := v.ln
	v.mu.Unlock()
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-time.After(timeout):
		v.closeListener()
		return ctlstream.Frame{}, ErrGuestStart
	}
	v.closeListener()
	if r.err != nil {
		return ctlstream.Frame{}, fmt.Errorf("%w: %w", ErrGuestStart, r.err)
	}
	_ = r.c.SetReadDeadline(time.Now().Add(timeout))
	conn := ctlstream.New(r.c)
	hello, err := conn.Expect(ctlstream.TypeHello, nil)
	if err != nil {
		_ = conn.Close()
		return hello, fmt.Errorf("%w: %w", ErrGuestStart, err)
	}
	_ = r.c.SetReadDeadline(time.Time{})
	v.setConn(conn)
	return hello, nil
}

// acceptResume accepts connections until one carries a resume hello with
// the stream key of this VM, then closes the listener.
func (v *fcVM) acceptResume() {
	v.mu.Lock()
	ln := v.ln
	v.mu.Unlock()
	if ln == nil {
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		conn := ctlstream.New(c)
		hello, err := conn.Expect(ctlstream.TypeHello, nil)
		key := v.StreamKey()
		if err != nil || hello.Phase != ctlstream.PhaseResume || key == "" || hello.StreamKey != key {
			v.f.log().Warn("refused a control stream without the stream key", "vm", v.spec.VMID)
			_ = conn.Close()
			continue
		}
		_ = c.SetReadDeadline(time.Time{})
		v.closeListener()
		v.setConn(conn)
		return
	}
}

func (v *fcVM) setConn(c *ctlstream.Conn) {
	v.mu.Lock()
	v.conn = c
	v.mu.Unlock()
	go v.read(c)
}

func (v *fcVM) currentConn() *ctlstream.Conn {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.conn
}

// read handles the frames of vm-init on one connection.
func (v *fcVM) read(c *ctlstream.Conn) {
	for {
		fr, err := c.Read()
		if err != nil {
			v.mu.Lock()
			if v.conn == c {
				v.conn = nil
			}
			v.mu.Unlock()
			select {
			case <-v.gone:
				return
			default:
			}
			// The stream broke while the VM runs: vm-init reconnects.
			if !errors.Is(err, io.EOF) {
				v.f.log().Warn("control stream", "vm", v.spec.VMID, "err", err)
			}
			if ln, lerr := v.f.listen(v.spec, v.f.Jail.Root(v.spec.VMID)); lerr == nil {
				v.mu.Lock()
				v.ln = ln
				v.mu.Unlock()
				go v.acceptResume()
			}
			return
		}
		switch fr.Type {
		case ctlstream.TypeRunnerStarted:
			v.runningOnce.Do(func() { close(v.running) })
		case ctlstream.TypeMMDSRead:
			select {
			case v.mmdsRead <- struct{}{}:
			default:
			}
		case ctlstream.TypeExit:
			f := fr
			v.mu.Lock()
			v.report = &f
			v.mu.Unlock()
			_ = c.Write(ctlstream.Frame{Type: ctlstream.TypeExitAck})
		}
	}
}

func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Go writes the session document, sends go and clears MMDS after vm-init
// read it.
func (v *fcVM) Go(ctx context.Context, doc mmds.Document) error {
	c := v.currentConn()
	if c == nil {
		return errors.New("no control stream to vm-init")
	}
	if err := v.api.PutMMDS(ctx, doc); err != nil {
		return err
	}
	key := newKey()
	v.mu.Lock()
	v.key = key
	v.mu.Unlock()
	if err := c.Write(ctlstream.Frame{Type: ctlstream.TypeGo, StreamKey: key}); err != nil {
		return err
	}
	select {
	case <-v.mmdsRead:
	case <-v.gone:
		return errors.New("the VM ended before it read its document")
	case <-time.After(30 * time.Second):
		return errors.New("vm-init did not read its document")
	case <-ctx.Done():
		return ctx.Err()
	}
	return v.api.PutMMDS(ctx, mmds.Cleared())
}

func (v *fcVM) Cancel(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		return v.Kill(ctx)
	}
	sent := false
	if c := v.currentConn(); c != nil {
		sent = c.Write(ctlstream.Frame{Type: ctlstream.TypeCancel, GraceSeconds: int(grace / time.Second)}) == nil
	}
	if !sent && v.spec.Arch == "amd64" && v.api != nil {
		// vm-init turned Ctrl-Alt-Del into SIGINT for PID 1: a cancel with 10 s grace.
		_ = v.api.Action(ctx, ActionSendCtrlAltDel)
	}
	time.AfterFunc(grace+5*time.Second, func() {
		select {
		case <-v.gone:
		default:
			_ = v.Kill(context.Background())
		}
	})
	return nil
}

func (v *fcVM) Kill(context.Context) error {
	v.mu.Lock()
	v.killed = true
	v.mu.Unlock()
	select {
	case <-v.gone:
		return nil
	default:
	}
	if err := v.f.Cgroups.Kill(v.cg); err == nil {
		return nil
	}
	if v.pid > 1 {
		if err := syscall.Kill(v.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

// watch waits until the Firecracker process ended and sends the Exit.
func (v *fcVM) watch() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if v.pid > 1 && v.f.alive(v.pid) {
			continue
		}
		break
	}
	v.goneOnce.Do(func() { close(v.gone) })
	// Give the reader a moment to store a report that raced with the end.
	time.Sleep(50 * time.Millisecond)
	v.mu.Lock()
	e := Exit{Report: v.report, Killed: v.killed}
	v.mu.Unlock()
	e.HostOOMKills = v.f.Cgroups.OOMKills(v.cg)
	v.exit <- e
	close(v.exit)
}

func (v *fcVM) Cleanup(ctx context.Context) error {
	v.cleanOnce.Do(func() {
		_ = v.Kill(ctx)
		if v.pid > 1 {
			deadline := time.Now().Add(5 * time.Second)
			for v.f.alive(v.pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
		}
		v.goneOnce.Do(func() { close(v.gone) })
		if err := v.f.Cgroups.WaitEmpty(v.cg, 5*time.Second); err != nil {
			v.cleanErr = err
		}
		v.closeListener()
		if c := v.currentConn(); c != nil {
			_ = c.Close()
		}
		v.f.removeJail(v.spec.VMID)
	})
	return v.cleanErr
}

// Snapshot pauses a template VM and writes its snapshot in the jail.
func (v *fcVM) Snapshot(ctx context.Context) (SnapshotFiles, error) {
	if err := v.api.PatchVM(ctx, StatePaused); err != nil {
		return SnapshotFiles{}, err
	}
	if err := v.api.CreateSnapshot(ctx, SnapshotCreate{SnapshotType: "Full", SnapshotPath: "vm.state", MemFilePath: "mem", SyncSnapshotFiles: true}); err != nil {
		return SnapshotFiles{}, err
	}
	root := v.f.Jail.Root(v.spec.VMID)
	return SnapshotFiles{State: filepath.Join(root, "vm.state"), Mem: filepath.Join(root, "mem"), Scratch: filepath.Join(root, "scratch.ext4")}, nil
}

var _ Template = (*fcVM)(nil)
