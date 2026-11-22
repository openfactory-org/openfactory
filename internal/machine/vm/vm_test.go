package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

type apiCall struct {
	Method, Path string
	Body         map[string]any
}

// fakeAPI serves the Firecracker API on a unix socket and records calls.
type fakeAPI struct {
	mu    sync.Mutex
	calls []apiCall
	fault string
	srv   *http.Server
	onPut func(path string, body map[string]any)
}

func startFakeAPI(t *testing.T, sock string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{r.Method, r.URL.Path, body})
		fault := f.fault
		on := f.onPut
		f.mu.Unlock()
		if fault != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"fault_message":"` + fault + `"}`))
			return
		}
		if on != nil {
			on(r.URL.Path, body)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fakeAPI) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func (f *fakeAPI) body(path string, n int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := 0
	for _, c := range f.calls {
		if c.Path == path {
			if i == n {
				return c.Body
			}
			i++
		}
	}
	return nil
}

func TestClientFault(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "api.sock")
	f := startFakeAPI(t, sock)
	c := NewClient(sock)
	if err := c.PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 2, MemSizeMiB: 4096, HugePages: "None"}); err != nil {
		t.Fatal(err)
	}
	b := f.body("/machine-config", 0)
	if b["vcpu_count"] != float64(2) || b["smt"] != false || b["huge_pages"] != "None" {
		t.Fatalf("body %v", b)
	}
	f.mu.Lock()
	f.fault = "bad drive"
	f.mu.Unlock()
	var ae *Error
	if err := c.PutDrive(context.Background(), Drive{DriveID: "x"}); !errors.As(err, &ae) || ae.Fault != "bad drive" || ae.Path != "/drives/x" {
		t.Fatalf("fault: %v", err)
	}
}

func testJail(root string) JailerConfig {
	return JailerConfig{
		Jailer: "/var/lib/openfactory/bin/firecracker-v1.18.0/jailer", Firecracker: "/var/lib/openfactory/bin/firecracker-v1.18.0/firecracker",
		ChrootBase: filepath.Join(root, "jail"), ParentCgroup: "system.slice/openfactory-worker.service/vms", CgroupRoot: filepath.Join(root, "cg"),
	}
}

func TestJailerArgs(t *testing.T) {
	j := testJail("/var/lib/openfactory")
	j.ChrootBase = "/var/lib/openfactory/jail"
	s := Spec{VMID: "vm-3-1f2e3d4c", UID: 900003, GID: 900003, VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 40}
	got := strings.Join(j.Args(s), " ")
	want := "/var/lib/openfactory/bin/firecracker-v1.18.0/jailer --id vm-3-1f2e3d4c --exec-file /var/lib/openfactory/bin/firecracker-v1.18.0/firecracker" +
		" --uid 900003 --gid 900003 --chroot-base-dir /var/lib/openfactory/jail --cgroup-version 2 --parent-cgroup system.slice/openfactory-worker.service/vms" +
		" --cgroup cpu.max=200000 100000 --cgroup memory.max=4429185024 --cgroup memory.swap.max=0 --cgroup pids.max=64" +
		" --resource-limit no-file=1024 --resource-limit fsize=42949672960 --new-pid-ns --daemonize --" +
		" --api-sock /run/firecracker.socket --level Warning --log-path /fc.log --http-api-max-payload-size 65536 --mmds-size-limit 65536"
	if got != want {
		t.Fatalf("args:\n%s\nwant\n%s", got, want)
	}
	// "cpu.max=200000 100000" is one argument.
	found := false
	for _, a := range j.Args(s) {
		found = found || a == "cpu.max=200000 100000"
	}
	if !found {
		t.Fatal("cpu.max is not one argument")
	}
	if j.Root(s.VMID) != "/var/lib/openfactory/jail/firecracker/vm-3-1f2e3d4c/root" {
		t.Fatalf("root %s", j.Root(s.VMID))
	}
}

func TestVMID(t *testing.T) {
	id := VMID(3, "run_p16abc.1")
	if !strings.HasPrefix(id, "vm-3-") || len(id) != len("vm-3-")+8 || !ValidVMID(id) {
		t.Fatalf("vm id %q", id)
	}
	if VMID(3, "run_p16abc.2") == id || ValidVMID("run_a.1") {
		t.Fatal("ids collide, or a task id passes")
	}
}

func TestBootArgs(t *testing.T) {
	if got := BootArgs(false, "amd64"); got != "reboot=k panic=1 8250.nr_uarts=0 quiet loglevel=1 ipv6.disable=1 random.trust_cpu=on rootfstype=ext4 init=/openfactory -- vm-init" {
		t.Fatalf("boot args %q", got)
	}
	if !strings.Contains(BootArgs(true, "arm64"), "console=ttyS0 keep_bootcon") {
		t.Fatal("debug console")
	}
}

func TestCgroupSetup(t *testing.T) {
	root := t.TempDir()
	c := Cgroups{Root: root}
	self, err := SelfCgroup("0::/system.slice/openfactory-worker.service\n")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := c.Setup(self, 42)
	if err != nil || parent != "system.slice/openfactory-worker.service/vms" {
		t.Fatalf("setup = %q, %v", parent, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "system.slice/openfactory-worker.service/agent/cgroup.procs")); string(b) != "42" {
		t.Fatalf("agent procs %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, parent, "cgroup.subtree_control")); string(b) != "+cpu +memory +pids +io" {
		t.Fatalf("subtree %q", b)
	}
	// A restart inside the agent leaf keeps the same parent.
	if p2, err := c.Setup("system.slice/openfactory-worker.service/agent", 43); err != nil || p2 != parent {
		t.Fatalf("restart setup = %q, %v", p2, err)
	}
	if _, err := c.Setup("", 1); err == nil {
		t.Fatal("root cgroup must fail")
	}
	vmcg := filepath.Join(root, parent, "vm-1-aa")
	_ = os.MkdirAll(vmcg, 0o755)
	_ = os.WriteFile(filepath.Join(vmcg, "memory.events"), []byte("low 0\noom 1\noom_kill 2\n"), 0o644)
	if c.OOMKills(vmcg) != 2 {
		t.Fatal("oom kills")
	}
	_ = os.WriteFile(filepath.Join(vmcg, "cgroup.procs"), nil, 0o644)
	// A real cgroup directory holds only kernel files; the fake needs them gone.
	_ = os.Remove(filepath.Join(vmcg, "memory.events"))
	_ = os.Remove(filepath.Join(vmcg, "cgroup.procs"))
	if err := c.WaitEmpty(vmcg, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := SelfCgroup("1:name=systemd:/x\n"); err == nil {
		t.Fatal("cgroup v1 must fail")
	}
}

func TestCloneFileSparse(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	f, _ := os.Create(src)
	_ = f.Truncate(8 << 20)
	_, _ = f.WriteAt([]byte("data"), 3<<20)
	_ = f.Close()
	dst := filepath.Join(d, "dst")
	if err := CloneFile(src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if len(a) != len(b) || string(b[3<<20:3<<20+4]) != "data" {
		t.Fatalf("copy differs: %d vs %d", len(a), len(b))
	}
}

// fakeGuest plays vm-init on the control stream.
type fakeGuest struct {
	exitCode int
	oomKills int
	dead     atomic.Bool
	keySeen  atomic.Value
}

func (g *fakeGuest) run(t *testing.T, root string, phase string) {
	t.Helper()
	path := filepath.Join(root, "v.sock_1024")
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("guest dial: %v", err)
		return
	}
	conn := ctlstream.New(c)
	_ = conn.Write(ctlstream.Frame{Type: ctlstream.TypeHello, V: 1, Phase: phase})
	for {
		f, err := conn.Read()
		if err != nil {
			return
		}
		switch f.Type {
		case ctlstream.TypeGo:
			g.keySeen.Store(f.StreamKey)
			_ = conn.Write(ctlstream.Frame{Type: ctlstream.TypeMMDSRead})
			_ = conn.Write(ctlstream.Frame{Type: ctlstream.TypeRunnerStarted, PID: 10})
		case ctlstream.TypeCancel:
			code := g.exitCode
			_ = conn.Write(ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseRunner, ExitCode: &code, OOMKills: g.oomKills})
		case ctlstream.TypeExitAck:
			g.dead.Store(true) // vm-init reboots; Firecracker ends
			_ = conn.Close()
			return
		}
	}
}

func newTestFirecracker(t *testing.T, root string, g *fakeGuest, phase string) (*Firecracker, **fakeAPI) {
	var api *fakeAPI
	f := &Firecracker{
		Jail:          testJail(root),
		Cgroups:       Cgroups{Root: filepath.Join(root, "cg")},
		Chown:         func(string, int, int) error { return nil },
		HelloTimeout:  3 * time.Second,
		SocketTimeout: 3 * time.Second,
	}
	f.Alive = func(int) bool {
		killed, _ := filepath.Glob(filepath.Join(f.Jail.CgroupRoot, f.Jail.ParentCgroup, "*", "cgroup.kill"))
		return !g.dead.Load() && len(killed) == 0
	}
	f.RunJailer = func(_ context.Context, args []string) error {
		var id string
		for i, a := range args {
			if a == "--id" {
				id = args[i+1]
			}
		}
		r := f.Jail.Root(id)
		_ = os.MkdirAll(f.Jail.CgroupDir(id), 0o755)
		if err := os.WriteFile(filepath.Join(r, "firecracker.pid"), []byte("424242\n"), 0o644); err != nil {
			return err
		}
		api = startFakeAPI(t, filepath.Join(r, "run", "firecracker.socket"))
		api.onPut = func(path string, body map[string]any) {
			if (path == "/actions" && body["action_type"] == ActionInstanceStart) || (path == "/vm" && body["state"] == StateResumed) {
				go g.run(t, r, phase)
			}
		}
		return nil
	}
	return f, &api
}

func testSpec(root string) Spec {
	for _, n := range []string{"vmlinux", "tools.ext4", "rootfs.ext4", "scratch.ext4", "cache.ext4"} {
		_ = os.WriteFile(filepath.Join(root, n), []byte(n), 0o644)
	}
	return Spec{
		VMID: "vm-1-0a0b0c0d", Arch: "amd64", Slot: 1, UID: 900001, GID: 900001, VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 1,
		Kernel: filepath.Join(root, "vmlinux"), Tools: filepath.Join(root, "tools.ext4"), Image: filepath.Join(root, "rootfs.ext4"),
		Scratch: filepath.Join(root, "scratch.ext4"), Cache: []string{filepath.Join(root, "cache.ext4")},
		Tap: "ofvm1", GuestMAC: net.HardwareAddr{6, 0, 10, 213, 0, 6}, DiskRate: 200 << 20, NetTx: 25 << 20,
	}
}

func TestFirecrackerBootGoCancel(t *testing.T) {
	root := shortTemp(t)
	g := &fakeGuest{exitCode: 0}
	f, apiP := newTestFirecracker(t, root, g, ctlstream.PhaseBoot)
	s := testSpec(root)
	v, err := f.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	api := *apiP
	wantOrder := []string{
		"PUT /machine-config", "PUT /boot-source", "PUT /drives/tools", "PUT /drives/image", "PUT /drives/scratch", "PUT /drives/cache-0",
		"PUT /network-interfaces/eth0", "PUT /mmds/config", "PUT /vsock", "PUT /entropy", "PUT /mmds", "PUT /actions",
	}
	if got := api.paths(); strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("API order:\n%v\nwant\n%v", got, wantOrder)
	}
	if b := api.body("/drives/scratch", 0); b["cache_type"] != "Unsafe" || b["discard"] != true || b["io_engine"] != "Sync" {
		t.Fatalf("scratch drive %v", b)
	}
	if b := api.body("/network-interfaces/eth0", 0); b["host_dev_name"] != "ofvm1" || b["guest_mac"] != "06:00:0a:d5:00:06" {
		t.Fatalf("nic %v", b)
	}
	if b := api.body("/mmds/config", 0); b["version"] != "V2" || b["ipv4_address"] != "169.254.169.254" {
		t.Fatalf("mmds config %v", b)
	}
	if b := api.body("/boot-source", 0); !strings.Contains(b["boot_args"].(string), "init=/openfactory -- vm-init") {
		t.Fatalf("boot %v", b)
	}
	// The jail has hard links and a private scratch copy.
	jr := f.Jail.Root(s.VMID)
	for _, n := range []string{"vmlinux", "tools.ext4", "rootfs.ext4", "cache-0.ext4", "scratch.ext4", "fc.log"} {
		if _, err := os.Stat(filepath.Join(jr, n)); err != nil {
			t.Errorf("jail lacks %s", n)
		}
	}
	if fi, _ := os.Stat(filepath.Join(jr, "scratch.ext4")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Error("scratch mode")
	}

	doc := mmds.Document{OpenFactory: mmds.Payload{V: 1, TaskID: "run_a.1", Env: map[string]string{"OF_BOOTSTRAP_TOKEN": "ofb_secret"}}}
	if err := v.Go(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.Running():
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	// The document went in, then MMDS was cleared.
	if b := api.body("/mmds", 1); b["openfactory"].(map[string]any)["task_id"] != "run_a.1" {
		t.Fatalf("session mmds %v", b)
	}
	if b := api.body("/mmds", 2); b["openfactory"].(map[string]any)["cleared"] != true {
		t.Fatalf("cleared mmds %v", b)
	}
	if k, _ := g.keySeen.Load().(string); k == "" || k != v.StreamKey() {
		t.Fatalf("stream key %q vs %q", k, v.StreamKey())
	}
	if err := v.Cancel(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-v.Wait():
		if e.Report == nil || *e.Report.ExitCode != 0 || e.Killed {
			t.Fatalf("exit %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exit")
	}
	if err := v.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.Jail.JailDir(s.VMID)); !os.IsNotExist(err) {
		t.Fatal("jail not removed")
	}
	if err := v.Cleanup(context.Background()); err != nil {
		t.Fatal("second cleanup", err)
	}
}

func TestFirecrackerWrongHelloPhase(t *testing.T) {
	root := shortTemp(t)
	g := &fakeGuest{}
	f, _ := newTestFirecracker(t, root, g, ctlstream.PhaseTemplate)
	f.HelloTimeout = time.Second
	s := testSpec(root)
	if _, err := f.Start(context.Background(), s); !errors.Is(err, ErrGuestStart) {
		t.Fatalf("wrong phase: %v", err)
	}
	if _, err := os.Stat(f.Jail.JailDir(s.VMID)); !os.IsNotExist(err) {
		t.Fatal("jail kept after a failed start")
	}
}

func TestFirecrackerRestoreAndAdopt(t *testing.T) {
	root := shortTemp(t)
	g := &fakeGuest{exitCode: 3}
	f, apiP := newTestFirecracker(t, root, g, ctlstream.PhaseRestored)
	s := testSpec(root)
	for _, n := range []string{"vm.state", "mem", "tscratch"} {
		_ = os.WriteFile(filepath.Join(root, n), []byte(n), 0o400)
	}
	s.Snapshot = &SnapshotFiles{State: filepath.Join(root, "vm.state"), Mem: filepath.Join(root, "mem"), Scratch: filepath.Join(root, "tscratch")}
	s.GuestMAC = TemplateMAC
	v, err := f.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	api := *apiP
	if got := strings.Join(api.paths(), ","); got != "PUT /snapshot/load,PATCH /vm" {
		t.Fatalf("restore calls %s", got)
	}
	load := api.body("/snapshot/load", 0)
	ov := load["network_overrides"].([]any)[0].(map[string]any)
	if ov["host_dev_name"] != "ofvm1" || load["resume_vm"] != false || load["mem_backend"].(map[string]any)["backend_type"] != "File" {
		t.Fatalf("load %v", load)
	}
	if b, _ := os.ReadFile(filepath.Join(f.Jail.Root(s.VMID), "scratch.ext4")); string(b) != "tscratch" {
		t.Fatal("restore must copy the template scratch")
	}
	if err := v.Go(context.Background(), mmds.Document{OpenFactory: mmds.Payload{V: 1}}); err != nil {
		t.Fatal(err)
	}
	key := v.StreamKey()

	// A new agent adopts the VM: a client without the key is refused, and
	// vm-init with the key is accepted.
	f2 := *f
	vms, err := f2.Adopt(context.Background(), []AdoptRecord{{VMID: s.VMID, Spec: s, StreamKey: key}})
	if err != nil || len(vms) != 1 {
		t.Fatalf("adopt = %v, %v", vms, err)
	}
	sock := filepath.Join(f.Jail.Root(s.VMID), "v.sock_1024")
	bad, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	bc := ctlstream.New(bad)
	_ = bc.Write(ctlstream.Frame{Type: ctlstream.TypeHello, Phase: ctlstream.PhaseResume, StreamKey: "wrong"})
	if _, err := bc.Read(); err == nil {
		t.Fatal("a stream without the key must be closed")
	}
	good, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gc := ctlstream.New(good)
	_ = gc.Write(ctlstream.Frame{Type: ctlstream.TypeHello, Phase: ctlstream.PhaseResume, StreamKey: key, Running: true})
	av := vms[0]
	select {
	case <-av.Running():
	default:
		t.Fatal("an adopted VM is running")
	}
	go func() {
		f, err := gc.Expect(ctlstream.TypeCancel, nil)
		if err == nil {
			code := 3
			_ = gc.Write(ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseRunner, ExitCode: &code, GraceSeconds: f.GraceSeconds})
			if _, err := gc.Expect(ctlstream.TypeExitAck, nil); err == nil {
				g.dead.Store(true)
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for av.(*fcVM).currentConn() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := av.Cancel(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-av.Wait():
		if e.Report == nil || *e.Report.ExitCode != 3 {
			t.Fatalf("exit %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exit after adopt")
	}
	_ = av.Cleanup(context.Background())
	_ = v.Cleanup(context.Background())
}
