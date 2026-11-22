// Package agent runs a microVM worker host: it wires the configuration, the
// identity, the image store, the VM network, Firecracker, the git cache and
// the snapshot templates into the backend, serves the local control socket
// and connects to the control plane with worker protocol v2.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/backend"
	"github.com/openfactory-org/openfactory/internal/machine/cache"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/hostcheck"
	"github.com/openfactory-org/openfactory/internal/machine/identity"
	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/snapshot"
	"github.com/openfactory-org/openfactory/internal/machine/store"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

// Options configures Run.
type Options struct {
	ConfigPath string
	Version    string
	// Exe is the runtime binary (os.Executable).
	Exe    string
	Logger *slog.Logger
}

// Run serves the worker until ctx ends.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return err
	}
	id, err := identity.Load(cfg.IdentityDir())
	if err != nil {
		return err
	}
	if cfg.ControlURL == "" {
		cfg.ControlURL = id.ControlURL
	}
	host := hostcheck.Host()
	if err := host.KVM(); err != nil {
		return fmt.Errorf("%w (run worker doctor)", err)
	}
	doc := hostcheck.Doctor(ctx, host, cfg)
	refuse := ""
	for _, c := range doc.Checks {
		switch c.Status {
		case hostcheck.Fail:
			log.Error("doctor check failed", "check", c.Name, "detail", c.Detail, "fix", c.Fix)
		case hostcheck.Warn:
			log.Warn("doctor check", "check", c.Name, "detail", c.Detail, "fix", c.Fix)
		}
	}
	if doc.ForwardDropped {
		refuse = "a host firewall drops forwarded traffic; run worker doctor"
	}

	procCg, _ := os.ReadFile("/proc/self/cgroup")
	self, err := vm.SelfCgroup(string(procCg))
	if err != nil {
		return err
	}
	cgroups := vm.Cgroups{Root: "/sys/fs/cgroup"}
	parent, err := cgroups.Setup(self, os.Getpid())
	if err != nil {
		return err
	}
	kernel, err := cfg.KernelPath()
	if err != nil {
		return err
	}
	st, err := store.New(filepath.Join(cfg.WorkerDir(), "tasks"))
	if err != nil {
		return err
	}
	images := &image.Store{Dir: cfg.DataDir, Chown: true, Log: log}
	images.CleanTmp()
	images.TarInput = image.ProbeTarInput(ctx, nil, os.TempDir())

	netCfg := NetConfig(cfg, readFile("/etc/resolv.conf"))
	applier := &vmnet.HostApplier{}
	if err := applyBase(ctx, applier, netCfg, st); err != nil {
		return fmt.Errorf("apply the base ruleset: %w", err)
	}

	fcDir := cfg.FirecrackerDir()
	vmm := &vm.Firecracker{
		Jail: vm.JailerConfig{
			Jailer: filepath.Join(fcDir, "jailer"), Firecracker: filepath.Join(fcDir, "firecracker"),
			ChrootBase: filepath.Join(cfg.DataDir, "jail"), ParentCgroup: parent, CgroupRoot: cgroups.Root,
		},
		Cgroups: cgroups, Log: log.With("component", "vmm"),
	}

	tok := &tokenBox{}
	gc := &cache.Cache{
		Dir: filepath.Join(cfg.DataDir, "gitcache"), Log: log.With("component", "gitcache"),
		Helper: o.Exe + " worker git-credential",
		HelperEnv: func() []string {
			return []string{cache.EnvControlURL + "=" + cfg.ControlURL, cache.EnvWorkerID + "=" + id.WorkerID, cache.EnvAccessToken + "=" + tok.get()}
		},
	}

	var snaps *snapshot.Manager
	var snapKey backend.SnapshotKey
	if cfg.Snapshots {
		snaps = &snapshot.Manager{Dir: filepath.Join(cfg.DataDir, "snapshots"), VMM: vmm, Log: log.With("component", "snapshot"),
			Net: templateNet(applier, netCfg, cfg)}
		snapKey = newSnapKey(cfg, kernel)
	}

	arch := hostcheck.GoArch(machine(host))
	// prune reads be: the backend calls it only after New returned.
	var be *backend.Backend
	prune := func() {
		pruneDisk(cfg, images, gc, snaps, snapKey, o.Version, arch, be.InUse, log)
	}
	be, err = backend.New(ctx, backend.Options{
		Config: cfg, VMM: vmm, Images: images, Net: applier, NetConfig: netCfg, Cache: gc, Store: st,
		Snapshots: snaps, SnapKey: snapKey, Kernel: kernel, Exe: o.Exe, Version: o.Version,
		Arch: arch, BootID: store.BootID("/proc/sys/kernel/random/boot_id"), Prune: prune,
		FreeDisk: func() (int64, error) {
			free, _, err := image.FreeBytes(cfg.DataDir)
			return free, err
		},
		RefuseUnrestricted: refuse, Logger: log.With("component", "backend"),
		Helpers: func(ctx context.Context, spec vmnet.HelperSpec, onFail func(error)) (backend.Helper, error) {
			return vmnet.StartHelper(ctx, spec, vmnet.HelperOptions{Logger: log, OnFail: onFail})
		},
	})
	if err != nil {
		return err
	}
	cleanOrphans(ctx, applier, netCfg, cfg, be, vmm.Jail, log)

	status := &statusBox{cfg: cfg, id: id, be: be}
	stopCtl, err := ServeControl(cfg.ControlSocket(), status)
	if err != nil {
		log.Warn("control socket", "err", err)
	} else {
		defer stopCtl()
	}
	go every(ctx, cache.MaxAge, func() { gc.Refresh(ctx) })
	go every(ctx, time.Hour, prune)

	facts := hostcheck.Facts(ctx, host, cfg, o.Version)
	features := []string{FeatureGitCache}
	if cfg.Snapshots {
		features = append(features, FeatureSnapshotRestore)
	}
	err = direct.RunWorkerV2(ctx, direct.WorkerV2Options{
		ServerURL: strings.TrimRight(cfg.ControlURL, "/"),
		WorkerID:  id.WorkerID, Key: id.Key, Name: cfg.Name, Labels: cfg.Labels,
		Capacity: cfg.Slots, Slots: cfg.Slots,
		Slot:    &direct.SlotSize{VCPUs: cfg.Slot.VCPUs, MemoryMiB: cfg.Slot.MemoryMiB, ScratchGiB: cfg.Slot.ScratchGiB},
		Backend: be, Version: o.Version, Facts: &facts, Features: features,
		OnWelcome: func(w direct.Welcome) {
			tok.set(w.AccessToken)
			status.welcome(w)
		},
		Logger: log.With("component", "worker", "worker_id", id.WorkerID),
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// SIGTERM: VMs keep running; the next start adopts them.
		return nil
	}
	return err
}

// pruneDisk frees disk: the least recently used images and git mirrors
// above their limits, then the snapshot templates of images that are no
// longer cached. inUse protects what live VMs use.
func pruneDisk(cfg config.Config, images *image.Store, gc *cache.Cache, snaps *snapshot.Manager, snapKey backend.SnapshotKey,
	version, arch string, inUse func(string) bool, log *slog.Logger,
) {
	if _, err := images.Prune(int64(cfg.Disk.ImagesMaxGiB)<<30, inUse); err != nil {
		log.Warn("image prune", "err", err)
	}
	gc.Prune(int64(cfg.Disk.GitcacheMaxGiB)<<30, inUse)
	if snaps == nil || snapKey == nil {
		return
	}
	imgs, err := images.List()
	if err != nil {
		log.Warn("snapshot prune", "err", err)
		return
	}
	tools := images.ToolsPath(version, arch)
	keep := map[string]bool{}
	for _, img := range imgs {
		if k := snapKey(img, tools); k != "" {
			keep[k] = true
		}
	}
	snaps.Prune(func(key string) bool { return keep[key] })
}

func machine(e hostcheck.Env) string {
	_, m := e.Uname()
	return m
}

func readFile(p string) string {
	data, _ := os.ReadFile(p) //nolint:gosec // G304: fixed system path
	return string(data)
}

// Features that the microVM worker announces in hello.
const (
	// FeatureGitCache: the worker keeps a git mirror cache and takes the
	// git_cache extras of task.assign.
	FeatureGitCache = "git_cache"
	// FeatureSnapshotRestore: the worker boots VMs from a template snapshot.
	FeatureSnapshotRestore = "snapshot_restore"
)

// NetConfig builds the VM network configuration.
func NetConfig(cfg config.Config, resolvConf string) vmnet.Config {
	n := vmnet.Config{
		Subnet: cfg.Subnet(), OfnetUID: cfg.Jail.OfnetUID, BlockSMTP: cfg.SMTPBlocked(),
		MaxConns: cfg.Network.Rate.MaxConns, Resolvers: vmnet.ParseResolvers(resolvConf), MTU: 1500,
	}
	for _, a := range cfg.Network.InternalAllow {
		if ap, err := netip.ParseAddrPort(a); err == nil {
			n.InternalAllow = append(n.InternalAllow, ap)
		}
	}
	return n
}

// applyBase replaces the table when no VM may run, and refreshes the base
// chains and sets when tasks of an earlier agent may still run.
func applyBase(ctx context.Context, a vmnet.Applier, n vmnet.Config, st *store.Store) error {
	recs, _ := st.List()
	live := false
	for _, r := range recs {
		live = live || r.Status.State != rtcore.TaskExited
	}
	var rules string
	var err error
	if live {
		rules, err = vmnet.RenderRefresh(n)
	} else {
		rules, err = vmnet.RenderFullApply(n)
	}
	if err != nil {
		return err
	}
	return a.ApplyBase(ctx, rules)
}

// cleanOrphans removes taps, rules and jails that belong to no live VM.
func cleanOrphans(ctx context.Context, a vmnet.Applier, n vmnet.Config, cfg config.Config, be *backend.Backend, jail vm.JailerConfig, log *slog.Logger) {
	live := map[string]bool{}
	used := map[int]bool{}
	list, _ := be.List(ctx)
	for _, s := range list {
		if s.State != rtcore.TaskExited {
			live[s.SandboxID] = true
			var slot int
			if _, err := fmt.Sscanf(s.SandboxID, "vm-%d-", &slot); err == nil {
				used[slot] = true
			}
		}
	}
	for slot := 0; slot < cfg.Slots; slot++ {
		if used[slot] {
			continue
		}
		for _, mode := range []vmnet.Mode{vmnet.Restricted, vmnet.Unrestricted} {
			v, err := vmnet.Alloc(n.Subnet, slot, cfg.Jail.UIDBase+slot, mode)
			if err != nil {
				continue
			}
			_, rm, _ := vmnet.RenderVM(n, v)
			_ = a.RemoveVM(ctx, v, rm)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(jail.ChrootBase, filepath.Base(jail.Firecracker), "*"))
	for _, d := range dirs {
		id := filepath.Base(d)
		if live[id] || strings.HasPrefix(id, "vm-tpl-") {
			continue
		}
		// Failed tasks keep their jail until destroy; only remove jails of no record.
		if _, err := os.Stat(filepath.Join(d, "root", "firecracker.pid")); err == nil && keptForFailure(list, id) {
			continue
		}
		log.Info("removing orphan jail", "vm", id)
		_ = os.RemoveAll(d)
	}
}

func keptForFailure(list []rtcore.TaskStatus, vmID string) bool {
	for _, s := range list {
		if s.SandboxID == vmID {
			return true
		}
	}
	return false
}

// templateNet gives a template VM the tap oftpl0 with a "drop everything" rule.
func templateNet(a vmnet.Applier, n vmnet.Config, cfg config.Config) snapshot.TemplateNet {
	return func(ctx context.Context) (string, func(), error) {
		v := vmnet.VMNet{Slot: 255, Tap: vmnet.TemplateTap, Owner: cfg.Jail.UIDBase + 255,
			HostIP: netip.MustParseAddr("169.254.255.1"), GuestIP: netip.MustParseAddr("169.254.255.2"), GuestMAC: vm.TemplateMAC}
		add := "add chain inet openfactory tpl_in\nadd rule inet openfactory tpl_in drop\n" +
			`add element inet openfactory vm_in  { "` + vmnet.TemplateTap + `" : jump tpl_in }` + "\n" +
			`add element inet openfactory vm_fwd { "` + vmnet.TemplateTap + `" : jump tpl_in }` + "\n"
		rm := `delete element inet openfactory vm_in  { "` + vmnet.TemplateTap + `" }` + "\n" +
			`delete element inet openfactory vm_fwd { "` + vmnet.TemplateTap + `" }` + "\n" +
			"flush chain inet openfactory tpl_in\ndelete chain inet openfactory tpl_in\n"
		_ = a.RemoveVM(ctx, v, rm)
		if err := a.AddVM(ctx, v, add); err != nil {
			return "", nil, err
		}
		return v.Tap, func() { _ = a.RemoveVM(context.Background(), v, rm) }, nil
	}
}

func newSnapKey(cfg config.Config, kernel string) backend.SnapshotKey {
	kernelSum, _ := snapshot.FileSHA256(kernel)
	cpu := snapshot.CPUModel(readFile("/proc/cpuinfo"))
	release, _ := hostcheck.Host().Uname()
	var mu sync.Mutex
	toolSums := map[string]string{}
	return func(img image.Image, tools string) string {
		mu.Lock()
		sum, ok := toolSums[tools]
		mu.Unlock()
		if !ok {
			s, err := snapshot.FileSHA256(tools)
			if err != nil {
				return ""
			}
			sum = s
			mu.Lock()
			toolSums[tools] = sum
			mu.Unlock()
		}
		return snapshot.Key(snapshot.KeyParts{
			FirecrackerVersion: cfg.Firecracker.Version, KernelSHA256: kernelSum, ToolsSHA256: sum, ImageDigest: img.Digest,
			VCPUs: cfg.Slot.VCPUs, MemoryMiB: cfg.Slot.MemoryMiB, ScratchGiB: cfg.Slot.ScratchGiB, HostCPUModel: cpu, HostKernel: release,
		})
	}
}

func every(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

type tokenBox struct {
	mu  sync.Mutex
	tok string
}

func (t *tokenBox) set(s string) {
	t.mu.Lock()
	t.tok = s
	t.mu.Unlock()
}

func (t *tokenBox) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tok
}

// Status is the answer of the control socket and of worker status.
type Status struct {
	WorkerID    string              `json:"worker_id"`
	Name        string              `json:"name,omitempty"`
	ControlURL  string              `json:"control_url"`
	Draining    bool                `json:"draining"`
	Slots       int                 `json:"slots"`
	SlotsUsed   int                 `json:"slots_used"`
	LastWelcome *time.Time          `json:"last_welcome,omitempty"`
	Running     bool                `json:"running"`
	Tasks       []rtcore.TaskStatus `json:"tasks"`
}

// Controller is what the control socket serves.
type Controller interface {
	Status(ctx context.Context) Status
	SetDraining(bool)
}

type statusBox struct {
	cfg config.Config
	id  identity.Identity
	be  *backend.Backend

	mu   sync.Mutex
	last *time.Time
}

func (s *statusBox) welcome(direct.Welcome) {
	now := time.Now().UTC()
	s.mu.Lock()
	s.last = &now
	s.mu.Unlock()
}

func (s *statusBox) Status(ctx context.Context) Status {
	tasks, _ := s.be.List(ctx)
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	return Status{
		WorkerID: s.id.WorkerID, Name: s.cfg.Name, ControlURL: s.cfg.ControlURL, Draining: s.be.Draining(),
		Slots: s.cfg.Slots, SlotsUsed: s.be.SlotsUsed(), LastWelcome: last, Running: true, Tasks: tasks,
	}
}

func (s *statusBox) SetDraining(d bool) { s.be.SetDraining(d) }

// ServeControl serves GET /status and POST /drain on a root-only unix socket.
func ServeControl(path string, c Controller) (func(), error) {
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status(r.Context()))
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Draining bool `json:"draining"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		c.SetDraining(body.Draining)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status(r.Context()))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close(); _ = os.Remove(path) }, nil
}

// Client talks to the control socket of a running agent.
type Client struct {
	hc *http.Client
}

// NewClient returns a client of the control socket at path.
func NewClient(path string) *Client {
	return &Client{hc: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}}
}

// Status reads the status of the agent.
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.do(ctx, http.MethodGet, "/status", nil)
}

// Drain changes the drain state of the agent.
func (c *Client) Drain(ctx context.Context, draining bool) (Status, error) {
	body, _ := json.Marshal(map[string]bool{"draining": draining})
	return c.do(ctx, http.MethodPost, "/drain", body)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (Status, error) {
	var st Status
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, strings.NewReader(string(body)))
	if err != nil {
		return st, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return st, fmt.Errorf("the worker agent does not answer (is openfactory-worker running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("worker agent: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// OfflineStatus reads the status from the task records when the agent
// does not run.
func OfflineStatus(cfg config.Config) (Status, error) {
	id, err := identity.Load(cfg.IdentityDir())
	if err != nil {
		return Status{}, err
	}
	st := Status{WorkerID: id.WorkerID, Name: cfg.Name, ControlURL: id.ControlURL, Slots: cfg.Slots}
	s := &store.Store{Dir: filepath.Join(cfg.WorkerDir(), "tasks")}
	recs, _ := s.List()
	for _, r := range recs {
		st.Tasks = append(st.Tasks, r.Status)
		if r.Status.State != rtcore.TaskExited {
			st.SlotsUsed++
		}
	}
	return st, nil
}
