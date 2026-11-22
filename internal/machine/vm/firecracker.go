package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/rtcore"
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
	if err := rtcore.WaitDial(ctx, "unix", sock, st); err != nil {
		return fail(fmt.Errorf("firecracker API socket did not appear: %w", err))
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
	hello, err := v.acceptBoot(ctx, ht)
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

// boot configures a new VM through the API and starts it.
func (f *Firecracker) boot(ctx context.Context, v *fcVM) error {
	s, a := v.spec, v.api
	if err := a.PutMachineConfig(ctx, MachineConfig{VCPUCount: s.VCPUs, MemSizeMiB: s.MemoryMiB, HugePages: "None"}); err != nil {
		return err
	}
	if err := a.PutBootSource(ctx, BootSource{KernelImagePath: "vmlinux", BootArgs: BootArgs(s.Debug, s.Arch)}); err != nil {
		return err
	}
	for _, d := range bootDrives(s) {
		if err := a.PutDrive(ctx, d); err != nil {
			return err
		}
	}
	if err := a.PutNetworkInterface(ctx, NetworkInterface{
		IfaceID: "eth0", HostDevName: s.Tap, GuestMAC: s.GuestMAC.String(), MTU: 1500,
		TxRateLimiter: s.NetTx.limiter(0), RxRateLimiter: s.NetRx.limiter(0),
	}); err != nil {
		return err
	}
	if err := a.PutMMDSConfig(ctx, MMDSConfig{Version: "V2", NetworkInterfaces: []string{"eth0"}, IPv4Address: mmds.Address}); err != nil {
		return err
	}
	if err := a.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "v.sock"}); err != nil {
		return err
	}
	if err := a.PutEntropy(ctx, Entropy{RateLimiter: &RateLimiter{Bandwidth: &TokenBucket{Size: 4096, RefillTime: 100}}}); err != nil {
		return err
	}
	doc := mmds.Document{OpenFactory: mmds.Payload{V: mmds.Version}}
	if s.Template {
		doc = mmds.Template()
	}
	if err := a.PutMMDS(ctx, doc); err != nil {
		return err
	}
	return a.Action(ctx, ActionInstanceStart)
}

// bootDrives returns the drives of a booted VM in order: tools (the root
// device), image, scratch, then the git cache disks.
func bootDrives(s Spec) []Drive {
	drives := []Drive{{DriveID: "tools", PathOnHost: "tools.ext4", IsRootDevice: true, IsReadOnly: true}}
	if s.Image != "" {
		drives = append(drives, Drive{DriveID: "image", PathOnHost: "rootfs.ext4", IsReadOnly: true, RateLimiter: s.DiskRate.limiter(20000)})
	}
	drives = append(drives, Drive{
		DriveID: "scratch", PathOnHost: "scratch.ext4", CacheType: "Unsafe", Discard: true, IOEngine: "Sync",
		RateLimiter: s.DiskRate.limiter(20000),
	})
	for i := range s.Cache {
		id := fmt.Sprintf("cache-%d", i)
		drives = append(drives, Drive{DriveID: id, PathOnHost: id + ".ext4", IsReadOnly: true})
	}
	return drives
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
