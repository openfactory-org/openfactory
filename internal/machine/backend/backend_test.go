package backend

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/openfactory-org/openfactory/internal/gitcachekey"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/machine/store"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// fakeVM is a VM that the test ends.
type fakeVM struct {
	id      string
	key     string
	mu      sync.Mutex
	doc     mmds.Document
	running chan struct{}
	exit    chan vm.Exit
	once    sync.Once
	cancel  time.Duration
	killed  bool
	cleaned bool
}

func newFakeVM(id string) *fakeVM {
	return &fakeVM{id: id, key: "k-" + id, running: make(chan struct{}), exit: make(chan vm.Exit, 1)}
}

func (f *fakeVM) ID() string               { return f.id }
func (f *fakeVM) StreamKey() string        { return f.key }
func (f *fakeVM) Running() <-chan struct{} { return f.running }
func (f *fakeVM) Wait() <-chan vm.Exit     { return f.exit }
func (f *fakeVM) Go(_ context.Context, d mmds.Document) error {
	f.mu.Lock()
	f.doc = d
	f.mu.Unlock()
	close(f.running)
	return nil
}
func (f *fakeVM) end(e vm.Exit) {
	f.once.Do(func() { f.exit <- e; close(f.exit) })
}
func (f *fakeVM) Cancel(_ context.Context, g time.Duration) error {
	f.mu.Lock()
	f.cancel = g
	f.mu.Unlock()
	code := 0
	f.end(vm.Exit{Report: &ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseRunner, ExitCode: &code}})
	return nil
}
func (f *fakeVM) Kill(context.Context) error {
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
	f.end(vm.Exit{Killed: true})
	return nil
}
func (f *fakeVM) Cleanup(context.Context) error {
	f.mu.Lock()
	f.cleaned = true
	f.mu.Unlock()
	return nil
}

type fakeVMM struct {
	mu       sync.Mutex
	specs    []vm.Spec
	vms      map[string]*fakeVM
	adopt    []vm.VM
	startErr error
}

func (f *fakeVMM) Start(_ context.Context, s vm.Spec) (vm.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.specs = append(f.specs, s)
	v := newFakeVM(s.VMID)
	if f.vms == nil {
		f.vms = map[string]*fakeVM{}
	}
	f.vms[s.VMID] = v
	return v, nil
}

func (f *fakeVMM) Adopt(_ context.Context, recs []vm.AdoptRecord) ([]vm.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []vm.VM
	for _, r := range recs {
		for _, v := range f.adopt {
			if v.ID() == r.VMID && v.StreamKey() == r.StreamKey {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func (f *fakeVMM) vm(id string) *fakeVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vms[id]
}

type fakeImages struct{ err error }

func (f fakeImages) Ensure(_ context.Context, ref, _ string, _ authn.Authenticator) (image.Image, error) {
	if f.err != nil {
		return image.Image{}, f.err
	}
	return image.Image{Digest: "sha256:abc", Ref: ref, Path: "/img/rootfs.ext4", Env: []string{"PATH=/bin"}, WorkingDir: "/workspace", User: "agent"}, nil
}
func (fakeImages) EnsureTools(context.Context, string, string, string) (string, error) {
	return "/tools.ext4", nil
}
func (fakeImages) EnsureScratch(context.Context, int) (string, error) { return "/scratch.ext4", nil }

type fakeHelper struct{ stopped bool }

func (h *fakeHelper) Stop()    { h.stopped = true }
func (h *fakeHelper) PID() int { return 0 }

type fakeCache struct{}

func (fakeCache) Latest(key string) string                     { return "/gc/" + key + ".ext4" }
func (fakeCache) Ensure(context.Context, string, string) error { return nil }

type env struct {
	b     *Backend
	vmm   *fakeVMM
	net   *vmnet.RecordingApplier
	store *store.Store
	opts  Options
	specs []vmnet.HelperSpec
	mu    sync.Mutex
}

func newEnv(t *testing.T, mut func(*Options)) *env {
	t.Helper()
	// Launch goroutines may still write records when the test ends, so the
	// store is not in t.TempDir (its cleanup fails on a non-empty dir).
	dir, err := os.MkdirTemp("", "ofbackend")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { time.Sleep(20 * time.Millisecond); _ = os.RemoveAll(dir) })
	st, err := store.New(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Slots = 2
	e := &env{vmm: &fakeVMM{}, net: &vmnet.RecordingApplier{}, store: st}
	e.opts = Options{
		Config: cfg, VMM: e.vmm, Images: fakeImages{}, Net: e.net, Store: st, Cache: fakeCache{},
		NetConfig: vmnet.Config{Subnet: netip.MustParsePrefix(cfg.Network.Subnet), OfnetUID: 899999, BlockSMTP: true},
		Helpers: func(_ context.Context, s vmnet.HelperSpec, _ func(error)) (Helper, error) {
			e.mu.Lock()
			e.specs = append(e.specs, s)
			e.mu.Unlock()
			return &fakeHelper{}, nil
		},
		Kernel: "/vmlinux", Exe: "/bin/openfactory-runtime", Version: "1.0.0", Arch: "amd64", BootID: "boot-1",
	}
	if mut != nil {
		mut(&e.opts)
	}
	e.b, err = New(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func testTask(n int) rtcore.Task {
	run := "run_" + string(rune('a'+n))
	return rtcore.Task{
		TaskID: rtcore.TaskID(run, 1), RunID: run, Session: 1, TeamID: "team_1", Image: "registry.example.com/sandbox:1",
		Platform: rtcore.TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			rtcore.TaskEnvControlURL: "https://factory.example.com", rtcore.TaskEnvRunID: run,
			rtcore.TaskEnvSession: "1", rtcore.TaskEnvBootstrapToken: "ofb_secret",
		},
		NetworkPolicy:     &rtcore.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{"github.com"}},
		MaxSessionSeconds: 3600,
	}
}

func waitState(t *testing.T, b *Backend, id string, want rtcore.TaskState) rtcore.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := b.Status(context.Background(), id)
		if err == nil && st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: state %+v, %v; want %s", id, st, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAssignRunAndComplete(t *testing.T) {
	e := newEnv(t, nil)
	task := testTask(1)
	repo := "https://github.com/acme/app.git"
	extras := []byte(`{"git_cache":[{"key":"` + gitcachekey.Key(repo) + `","clone_url":"` + repo + `"}]}`)
	st, err := e.b.StartWithExtras(context.Background(), task, extras)
	if err != nil || st.State != rtcore.TaskAccepted || !strings.HasPrefix(st.SandboxID, "vm-0-") {
		t.Fatalf("start = %+v, %v", st, err)
	}
	ch, _ := e.b.Watch(context.Background(), task.TaskID)
	waitState(t, e.b, task.TaskID, rtcore.TaskRunning)
	v := e.vmm.vm(st.SandboxID)
	spec := e.vmm.specs[0]
	if spec.Tap != "ofvm0" || spec.UID != 900000 || len(spec.Cache) != 1 || spec.Image != "/img/rootfs.ext4" || spec.Kernel != "/vmlinux" {
		t.Fatalf("spec %+v", spec)
	}
	doc := v.doc.OpenFactory
	if doc.Env[rtcore.TaskEnvBootstrapToken] != "ofb_secret" || doc.Network.ProxyURL != "http://10.213.0.1:3128" || len(doc.Network.DNS) != 0 ||
		doc.Network.EgressReportURL != "http://10.213.0.1:3129/blocked" || doc.User.Name != "agent" || doc.Disks.Cache[0].Drive != "cache-0" ||
		len(doc.MachineID) != 32 || doc.Entropy == "" || doc.Network.Address != "10.213.0.2/30" {
		t.Fatalf("mmds %+v %+v", doc, doc.Network)
	}
	if hs := e.specs[0]; hs.AlwaysAllow[0] != "factory.example.com" || hs.Allowed[0] != "github.com" || hs.VM.Mode != vmnet.Restricted {
		t.Fatalf("helper %+v", hs)
	}
	added, _ := e.net.Snapshot()
	if !strings.Contains(added["ofvm0"], "tcp dport { 3128, 3129 } accept") {
		t.Fatalf("rules %q", added["ofvm0"])
	}
	// The record never holds the token.
	rec, _ := e.store.Get(task.TaskID)
	if rec.Task.Env[rtcore.TaskEnvBootstrapToken] != "***" || rec.StreamKey == "" {
		t.Fatalf("record %+v", rec)
	}
	code := 0
	v.end(vm.Exit{Report: &ctlstream.Frame{Phase: ctlstream.ExitPhaseRunner, ExitCode: &code}})
	fin := waitState(t, e.b, task.TaskID, rtcore.TaskExited)
	if fin.Reason != rtcore.SandboxCompleted {
		t.Fatalf("final %+v", fin)
	}
	var states []rtcore.TaskState
	for s := range ch {
		states = append(states, s.State)
	}
	if states[len(states)-1] != rtcore.TaskExited {
		t.Fatalf("watch states %v", states)
	}
	_, removed := e.net.Snapshot()
	if _, ok := removed["ofvm0"]; !ok {
		t.Fatal("network not removed")
	}
	if e.b.SlotsUsed() != 0 {
		t.Fatal("slot not released")
	}
	if err := e.b.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatal("second destroy", err)
	}
	if list, _ := e.b.List(context.Background()); len(list) != 0 {
		t.Fatalf("list after destroy %v", list)
	}
}

func TestUnrestrictedNetwork(t *testing.T) {
	e := newEnv(t, nil)
	task := testTask(2)
	task.NetworkPolicy = &rtcore.TaskNetworkPolicy{Mode: "unrestricted"}
	if _, err := e.b.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	waitState(t, e.b, task.TaskID, rtcore.TaskRunning)
	v := e.vmm.vm(vm.VMID(0, task.TaskID))
	if n := v.doc.OpenFactory.Network; n.DNS[0] != "10.213.0.1" || n.ProxyURL != "" {
		t.Fatalf("network %+v", n)
	}
	added, _ := e.net.Snapshot()
	if !strings.Contains(added["ofvm0"], "masquerade") && !strings.Contains(added["ofvm0"], "th dport 53 accept") {
		t.Fatalf("rules %q", added["ofvm0"])
	}
	// A foreign firewall makes the worker refuse unrestricted tasks.
	e2 := newEnv(t, func(o *Options) { o.RefuseUnrestricted = "docker drops forwarded traffic" })
	if _, err := e2.b.Start(context.Background(), task); !errors.Is(err, rtcore.ErrInvalid) {
		t.Fatalf("refuse: %v", err)
	}
}

func TestCapacityDrainAndDisk(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 2; i++ {
		if _, err := e.b.Start(context.Background(), testTask(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.b.Start(context.Background(), testTask(1)); err != nil {
		t.Fatal("idempotent start", err)
	}
	if _, err := e.b.Start(context.Background(), testTask(5)); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("third task: %v", err)
	}
	e2 := newEnv(t, nil)
	e2.b.SetDraining(true)
	if _, err := e2.b.Start(context.Background(), testTask(1)); !errors.Is(err, rtcore.ErrAtCapacity) || e2.b.Preflight(context.Background()) == nil {
		t.Fatalf("draining: %v", err)
	}
	pruned := false
	e3 := newEnv(t, func(o *Options) {
		o.FreeDisk = func() (int64, error) { return 10 << 30, nil }
		o.Prune = func() { pruned = true }
	})
	if _, err := e3.b.Start(context.Background(), testTask(1)); !errors.Is(err, rtcore.ErrAtCapacity) || !pruned {
		t.Fatalf("disk: %v, pruned %v", err, pruned)
	}
}

func TestInvalidSpec(t *testing.T) {
	e := newEnv(t, nil)
	muts := map[string]func(*rtcore.Task){
		"arch":   func(t *rtcore.Task) { t.Platform.Arch = "arm64" },
		"memory": func(t *rtcore.Task) { t.Resources.Memory = "64Gi" },
		"cpu":    func(t *rtcore.Task) { t.Resources.CPU = "16" },
		"root":   func(t *rtcore.Task) { t.Platform.User = "0" },
		"env":    func(t *rtcore.Task) { t.Env["BIG"] = strings.Repeat("x", 70<<10) },
		"token":  func(t *rtcore.Task) { delete(t.Env, rtcore.TaskEnvBootstrapToken) },
	}
	for name, m := range muts {
		task := testTask(1)
		m(&task)
		if _, err := e.b.Start(context.Background(), task); !errors.Is(err, rtcore.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := []byte(`{"git_cache":[{"key":"0000000000000000","clone_url":"https://github.com/acme/app"}]}`)
	if _, err := e.b.StartWithExtras(context.Background(), testTask(1), bad); !errors.Is(err, rtcore.ErrInvalid) {
		t.Errorf("git cache key: %v", err)
	}
}

func TestImagePullFailure(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Images = fakeImages{err: errors.New("401 unauthorized")} })
	task := testTask(1)
	if _, err := e.b.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e.b, task.TaskID, rtcore.TaskExited)
	if st.Reason != rtcore.SandboxImagePull || !strings.Contains(st.Message, "401") {
		t.Fatalf("status %+v", st)
	}
	e2 := newEnv(t, func(o *Options) { o.VMM = &fakeVMM{startErr: vm.ErrGuestStart} })
	if _, err := e2.b.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if st := waitState(t, e2.b, task.TaskID, rtcore.TaskExited); st.Reason != rtcore.SandboxProcessFailed || !strings.Contains(st.Message, "guest did not start") {
		t.Fatalf("no hello: %+v", st)
	}
	e3 := newEnv(t, func(o *Options) {
		o.Helpers = func(context.Context, vmnet.HelperSpec, func(error)) (Helper, error) {
			return nil, vmnet.ErrHelperNotReady
		}
	})
	if _, err := e3.b.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if st := waitState(t, e3.b, task.TaskID, rtcore.TaskExited); st.Reason != rtcore.SandboxNetworkPolicyTimeout {
		t.Fatalf("proxy: %+v", st)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, nil)
	task := testTask(1)
	if _, err := e.b.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	waitState(t, e.b, task.TaskID, rtcore.TaskRunning)
	if err := e.b.Cancel(context.Background(), task.TaskID, 30); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e.b, task.TaskID, rtcore.TaskExited)
	if st.Reason != rtcore.SandboxCancelled {
		t.Fatalf("status %+v", st)
	}
	v := e.vmm.vm(st.SandboxID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		v.mu.Lock()
		grace, cleaned := v.cancel, v.cleaned
		v.mu.Unlock()
		if grace == 30*time.Second && cleaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel grace %v cleaned %v", grace, cleaned)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.b.Cancel(context.Background(), "run_none.1", 0); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatal(err)
	}
}

// The agent restarts during a turn: the new backend adopts the VM and
// reports its end.
func TestWorkerRestartsDuringTurn(t *testing.T) {
	e := newEnv(t, nil)
	task := testTask(1)
	st, _ := e.b.Start(context.Background(), task)
	waitState(t, e.b, task.TaskID, rtcore.TaskRunning)
	old := e.vmm.vm(st.SandboxID)
	// The old agent process is gone; the new one sees the same VM through
	// a new control stream.
	v := newFakeVM(old.id)
	v.key = old.key
	e.b = nil

	vmm2 := &fakeVMM{adopt: []vm.VM{v}}
	o := e.opts
	o.VMM = vmm2
	b2, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := b2.Status(context.Background(), task.TaskID); s.State != rtcore.TaskRunning || b2.SlotsUsed() != 1 {
		t.Fatalf("adopted %+v", s)
	}
	code := 3
	v.end(vm.Exit{Report: &ctlstream.Frame{Phase: ctlstream.ExitPhaseRunner, ExitCode: &code}})
	fin := waitState(t, b2, task.TaskID, rtcore.TaskExited)
	if fin.Reason != rtcore.SandboxProcessFailed || *fin.ExitCode != 3 {
		t.Fatalf("final %+v", fin)
	}
}

func TestRestartAfterRebootAndLostVM(t *testing.T) {
	e := newEnv(t, nil)
	a, b := testTask(1), testTask(2)
	_, _ = e.b.Start(context.Background(), a)
	_, _ = e.b.Start(context.Background(), b)
	waitState(t, e.b, a.TaskID, rtcore.TaskRunning)
	waitState(t, e.b, b.TaskID, rtcore.TaskRunning)

	// Same boot, no live VM: unknown.
	o := e.opts
	o.VMM = &fakeVMM{}
	b2, _ := New(context.Background(), o)
	if s, _ := b2.Status(context.Background(), a.TaskID); s.Reason != rtcore.SandboxUnknown {
		t.Fatalf("lost VM %+v", s)
	}
	// Host rebooted: node_lost (a fresh store copy of running records).
	e3 := newEnv(t, nil)
	_, _ = e3.b.Start(context.Background(), a)
	waitState(t, e3.b, a.TaskID, rtcore.TaskRunning)
	o3 := e3.opts
	o3.BootID = "boot-2"
	b3, _ := New(context.Background(), o3)
	if s, _ := b3.Status(context.Background(), a.TaskID); s.Reason != rtcore.SandboxNodeLost {
		t.Fatalf("reboot %+v", s)
	}
}

func TestMapExit(t *testing.T) {
	code := func(n int) *int { return &n }
	runner := func(c *int, sig, oom int) *ctlstream.Frame {
		return &ctlstream.Frame{Phase: ctlstream.ExitPhaseRunner, ExitCode: c, Signal: sig, OOMKills: oom}
	}
	tests := []struct {
		name string
		e    vm.Exit
		f    endFlags
		want rtcore.SandboxReason
	}{
		{"completed", vm.Exit{Report: runner(code(0), 0, 0)}, endFlags{}, rtcore.SandboxCompleted},
		{"failed", vm.Exit{Report: runner(code(3), 0, 0)}, endFlags{}, rtcore.SandboxProcessFailed},
		{"guest oom", vm.Exit{Report: runner(nil, 9, 2)}, endFlags{}, rtcore.SandboxContainerOOM},
		{"killed no oom", vm.Exit{Report: runner(nil, 9, 0)}, endFlags{}, rtcore.SandboxProcessFailed},
		{"host oom", vm.Exit{HostOOMKills: 1}, endFlags{}, rtcore.SandboxContainerOOM},
		{"no report", vm.Exit{}, endFlags{}, rtcore.SandboxUnknown},
		{"init", vm.Exit{Report: &ctlstream.Frame{Phase: ctlstream.ExitPhaseInit, Error: "mount"}}, endFlags{}, rtcore.SandboxProcessFailed},
		{"cancel", vm.Exit{Report: runner(code(0), 0, 0)}, endFlags{cancelled: true}, rtcore.SandboxCancelled},
		{"deadline", vm.Exit{Killed: true}, endFlags{cancelled: true, deadline: true}, rtcore.SandboxDeadlineExceeded},
		{"net", vm.Exit{Killed: true}, endFlags{netFailed: true}, rtcore.SandboxNetworkPolicyTimeout},
	}
	for _, tt := range tests {
		if got, _, _ := MapExit(tt.e, tt.f); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestQuantities(t *testing.T) {
	for in, want := range map[string]int64{"2": 2000, "500m": 500, "1.5": 1500, "": 0} {
		if got, err := ParseCPU(in); err != nil || got != want {
			t.Errorf("cpu %q = %d, %v", in, got, err)
		}
	}
	for in, want := range map[string]int64{"4Gi": 4 << 30, "512Mi": 512 << 20, "1G": 1e9, "100": 100} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("memory %q = %d, %v", in, got, err)
		}
	}
	if _, err := ParseMemory("lots"); err == nil {
		t.Error("bad memory")
	}
}

func TestResolveUser(t *testing.T) {
	if u := resolveUser("", ""); u.UID != 1001 || u.Home != "/home/agent" {
		t.Fatalf("default %+v", u)
	}
	if u := resolveUser("1000", "agent"); u.UID != 1000 || u.Name != "" {
		t.Fatalf("numeric %+v", u)
	}
	if u := resolveUser("image", "2000:3000"); u.UID != 2000 || u.GID != 3000 {
		t.Fatalf("image numeric %+v", u)
	}
	if u := resolveUser("image", "node"); u.Name != "node" {
		t.Fatalf("image name %+v", u)
	}
}
