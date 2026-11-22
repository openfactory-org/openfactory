// Package backend is the task backend of a microVM worker. It implements
// rtcore.TaskBackend (and direct.ExtrasBackend) over the VMM, the image
// store, the VM network, the git cache and the task records, so the worker
// protocol client of internal/sandbox/direct drives it like any backend.
package backend

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/openfactory-org/openfactory/internal/gitcachekey"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/machine/snapshot"
	"github.com/openfactory-org/openfactory/internal/machine/store"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Name is the backend name in the hello of the worker.
const Name = "firecracker"

// MaxCacheDisks is the most git cache disks of one VM.
const MaxCacheDisks = 4

// tombstoneTTL keeps a destroyed task answerable as exited.
const tombstoneTTL = time.Hour

// Images converts OCI images to disks.
type Images interface {
	Ensure(ctx context.Context, ref, arch string, auth authn.Authenticator) (image.Image, error)
	EnsureTools(ctx context.Context, version, arch, exe string) (string, error)
	EnsureScratch(ctx context.Context, gib int) (string, error)
}

// GitCache gives the newest disk of a repository and refreshes mirrors.
type GitCache interface {
	Latest(key string) string
	Ensure(ctx context.Context, key, cloneURL string) error
}

// Helper is a running network helper.
type Helper interface {
	Stop()
	PID() int
}

// StartHelper starts the network helper of a VM and waits until it listens.
type StartHelper func(ctx context.Context, spec vmnet.HelperSpec, onFail func(error)) (Helper, error)

// SnapshotKey returns the template key of an image and a tools disk, or ""
// when snapshots are off.
type SnapshotKey func(img image.Image, tools string) string

// Options configures New.
type Options struct {
	Config    config.Config
	VMM       vm.VMM
	Images    Images
	Net       vmnet.Applier
	NetConfig vmnet.Config
	Helpers   StartHelper
	Cache     GitCache
	Store     *store.Store
	Snapshots *snapshot.Manager
	SnapKey   SnapshotKey
	// Kernel is the guest kernel; Exe is the runtime binary that goes on
	// the tools disk and runs the helpers; Version is its version.
	Kernel, Exe, Version string
	// Arch is the host arch (amd64 or arm64).
	Arch string
	// BootID is the boot ID of the host.
	BootID string
	// FreeDisk returns the free bytes of the data directory.
	FreeDisk func() (int64, error)
	// Prune frees disk (LRU of images and git cache) when a check fails.
	Prune func()
	// RefuseUnrestricted, when set, refuses unrestricted tasks with this
	// message (a foreign firewall drops forwarded traffic).
	RefuseUnrestricted string
	Logger             *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// Backend runs one microVM per task.
type Backend struct {
	o   Options
	log *slog.Logger
	ctx context.Context

	mu       sync.Mutex
	tasks    map[string]*task
	slots    []string // slot → task ID
	draining bool
}

type task struct {
	rec      store.Record
	full     rtcore.Task // with the token; memory only
	extras   Extras
	vm       vm.VM
	net      *vmnet.VMNet
	netRm    string
	helper   Helper
	flags    endFlags
	watchers []chan rtcore.TaskStatus
	deadline *time.Timer
	cleanup  *time.Timer
	gone     time.Time // destroyed at
	launched chan struct{}
}

// Extras are the worker extras of task.assign.
type Extras struct {
	GitCache []GitRepo `json:"git_cache,omitempty"`
}

// GitRepo is one repository of the session.
type GitRepo struct {
	Key      string `json:"key"`
	CloneURL string `json:"clone_url"`
}

// New creates the backend and adopts the tasks of an earlier agent process.
func New(ctx context.Context, o Options) (*Backend, error) {
	if o.VMM == nil || o.Images == nil || o.Net == nil || o.Store == nil || o.Helpers == nil {
		return nil, fmt.Errorf("%w: backend needs a VMM, images, network, helpers and a store", rtcore.ErrInvalid)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	b := &Backend{o: o, log: o.Logger, ctx: ctx, tasks: map[string]*task{}, slots: make([]string, o.Config.Slots)}
	if err := b.adopt(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// Name returns "firecracker".
func (b *Backend) Name() string { return Name }

// SetDraining makes Start refuse new tasks with at_capacity.
func (b *Backend) SetDraining(d bool) {
	b.mu.Lock()
	b.draining = d
	b.mu.Unlock()
}

// Draining reports whether the backend refuses new tasks.
func (b *Backend) Draining() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.draining
}

// SlotsUsed returns the number of slots with a task.
func (b *Backend) SlotsUsed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.slots {
		if s != "" {
			n++
		}
	}
	return n
}

// Start starts a task without extras.
func (b *Backend) Start(ctx context.Context, t rtcore.Task) (rtcore.TaskStatus, error) {
	return b.StartWithExtras(ctx, t, nil)
}

// StartWithExtras checks the task, reserves a slot, records the task and
// returns accepted. The VM starts in the background.
func (b *Backend) StartWithExtras(_ context.Context, t rtcore.Task, raw json.RawMessage) (rtcore.TaskStatus, error) {
	if err := t.Validate(); err != nil {
		return rtcore.TaskStatus{}, fmt.Errorf("%w: %w", rtcore.ErrInvalid, err)
	}
	var ex Extras
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &ex); err != nil {
			return rtcore.TaskStatus{}, fmt.Errorf("%w: worker extras: %w", rtcore.ErrInvalid, err)
		}
	}
	if err := b.checkSpec(t, ex); err != nil {
		return rtcore.TaskStatus{}, err
	}
	b.mu.Lock()
	if old, ok := b.tasks[t.TaskID]; ok {
		st := old.rec.Status
		b.mu.Unlock()
		return st, nil
	}
	if b.draining {
		b.mu.Unlock()
		return rtcore.TaskStatus{}, fmt.Errorf("worker is draining: %w", rtcore.ErrAtCapacity)
	}
	slot := -1
	live := 0
	for i, s := range b.slots {
		if s == "" && slot < 0 {
			slot = i
		}
		if s != "" {
			live++
		}
	}
	if slot < 0 {
		b.mu.Unlock()
		return rtcore.TaskStatus{}, fmt.Errorf("all %d slots are in use: %w", len(b.slots), rtcore.ErrAtCapacity)
	}
	if err := b.checkDisk(live); err != nil {
		b.mu.Unlock()
		return rtcore.TaskStatus{}, err
	}
	now := b.o.Now().UTC()
	st := rtcore.TaskStatus{TaskID: t.TaskID, State: rtcore.TaskAccepted, SandboxID: vm.VMID(slot, t.TaskID)}
	tk := &task{
		full: t, extras: ex, launched: make(chan struct{}),
		rec: store.Record{
			TaskID: t.TaskID, VMID: st.SandboxID, Slot: slot, Status: st, BootID: b.o.BootID, Task: t,
			Deadline: now.Add(time.Duration(t.MaxSessionSeconds+60) * time.Second),
		},
	}
	if err := b.o.Store.Put(tk.rec); err != nil {
		b.mu.Unlock()
		return rtcore.TaskStatus{}, fmt.Errorf("record task: %w", err)
	}
	b.slots[slot] = t.TaskID
	b.tasks[t.TaskID] = tk
	b.mu.Unlock()
	go b.launch(tk) //nolint:gosec // G118: the VM outlives the request; launch uses the backend context
	return st, nil
}

// checkSpec refuses tasks that cannot run on this worker (invalid_spec).
func (b *Backend) checkSpec(t rtcore.Task, ex Extras) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: "+f, append([]any{rtcore.ErrInvalid}, a...)...) }
	if b.o.Arch != "" && t.Platform.Arch != b.o.Arch {
		return bad("the task wants %s and this worker is %s", t.Platform.Arch, b.o.Arch)
	}
	slot := b.o.Config.Slot
	cpu, err := ParseCPU(t.Resources.CPU)
	if err != nil {
		return bad("%v", err)
	}
	if cpu > int64(slot.VCPUs)*1000 {
		return bad("the task wants %s CPU and a slot has %d vCPUs", t.Resources.CPU, slot.VCPUs)
	}
	mem, err := ParseMemory(t.Resources.Memory)
	if err != nil {
		return bad("%v", err)
	}
	if mem > int64(slot.MemoryMiB)<<20 {
		return bad("the task wants %s memory and a slot has %d MiB", t.Resources.Memory, slot.MemoryMiB)
	}
	if u := strings.TrimSpace(t.Platform.User); u == "0" || strings.HasPrefix(u, "0:") {
		if !b.o.Config.Guest.AllowRoot {
			return bad("platform.user 0 (root) is refused on this worker")
		}
	}
	if !rtcore.Restricted(t.NetworkPolicy) && b.o.RefuseUnrestricted != "" {
		return bad("unrestricted network refused: %s", b.o.RefuseUnrestricted)
	}
	if len(ex.GitCache) > MaxCacheDisks {
		return bad("at most %d git cache repositories, got %d", MaxCacheDisks, len(ex.GitCache))
	}
	for _, r := range ex.GitCache {
		if !gitcachekey.Valid(r.Key) || gitcachekey.Key(r.CloneURL) != r.Key {
			return bad("git cache key %q does not match %q", r.Key, r.CloneURL)
		}
	}
	// The MMDS document must fit the data store limit.
	doc := mmds.Document{OpenFactory: mmds.Payload{V: 1, Env: t.Env, TaskID: t.TaskID}}
	if doc.Size()+8<<10 > mmds.MaxDocument {
		return bad("the task environment is too large for the VM metadata (%d bytes)", doc.Size())
	}
	return nil
}

// checkDisk refuses a task when the disk cannot hold one more full scratch
// disk plus the reserve. Sparse scratch files can fill up, so each live VM
// counts with its full size. Call it with b.mu held.
func (b *Backend) checkDisk(live int) error {
	if b.o.FreeDisk == nil {
		return nil
	}
	c := b.o.Config
	need := int64(c.Slot.ScratchGiB)<<30*int64(live+1) + int64(c.Disk.ReserveGiB)<<30
	free, err := b.o.FreeDisk()
	if err != nil {
		return nil //nolint:nilerr // no statfs: do not block work on a failed check
	}
	if free >= need {
		return nil
	}
	if b.o.Prune != nil {
		b.o.Prune()
		if free, err = b.o.FreeDisk(); err == nil && free >= need {
			return nil
		}
	}
	return fmt.Errorf("disk: %d GiB free, %d GiB needed: %w", free>>30, need>>30, rtcore.ErrAtCapacity)
}

func (b *Backend) setStatus(tk *task, st rtcore.TaskStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setStatusLocked(tk, st)
}

func (b *Backend) setStatusLocked(tk *task, st rtcore.TaskStatus) {
	old := tk.rec.Status
	if old.State == rtcore.TaskExited {
		return
	}
	st.TaskID = tk.rec.TaskID
	if st.SandboxID == "" {
		st.SandboxID = old.SandboxID
	}
	if st.StartedAt == nil {
		st.StartedAt = old.StartedAt
	}
	tk.rec.Status = st
	if err := b.o.Store.Put(tk.rec); err != nil {
		b.log.Warn("record task", "task", tk.rec.TaskID, "err", err)
	}
	for _, ch := range tk.watchers {
		select {
		case ch <- st:
		default:
			// Drop the oldest status for a slow watcher; the final one must arrive.
			select {
			case <-ch:
			default:
			}
			ch <- st
		}
		if st.State == rtcore.TaskExited {
			close(ch)
		}
	}
	if st.State == rtcore.TaskExited {
		tk.watchers = nil
	}
}

// fail ends a task that never got a running VM.
func (b *Backend) fail(tk *task, reason rtcore.SandboxReason, msg string) {
	b.log.Warn("task failed", "task", tk.rec.TaskID, "reason", reason, "msg", msg)
	b.teardown(tk)
	b.finish(tk, reason, nil, msg)
}

func (b *Backend) finish(tk *task, reason rtcore.SandboxReason, code *int, msg string) {
	now := b.o.Now().UTC()
	b.mu.Lock()
	if tk.deadline != nil {
		tk.deadline.Stop()
	}
	if tk.rec.Slot >= 0 && tk.rec.Slot < len(b.slots) && b.slots[tk.rec.Slot] == tk.rec.TaskID {
		b.slots[tk.rec.Slot] = ""
	}
	b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: reason, ExitCode: code, Message: msg, EndedAt: &now})
	v := tk.vm
	b.mu.Unlock()
	if v == nil {
		return
	}
	if reason == rtcore.SandboxCompleted || reason == rtcore.SandboxCancelled {
		_ = v.Cleanup(context.Background())
		return
	}
	// A failed task keeps its jail (the scratch disk) for inspection.
	keep := time.Duration(tk.full.FailureRetentionSeconds) * time.Second
	b.mu.Lock()
	tk.cleanup = time.AfterFunc(keep, func() { _ = v.Cleanup(context.Background()) })
	b.mu.Unlock()
}

// teardown removes the network and the helper of a task.
func (b *Backend) teardown(tk *task) {
	b.mu.Lock()
	h, n, rm := tk.helper, tk.net, tk.netRm
	tk.helper, tk.net = nil, nil
	b.mu.Unlock()
	if h != nil {
		h.Stop()
	}
	if n != nil {
		if err := b.o.Net.RemoveVM(context.Background(), *n, rm); err != nil {
			b.log.Warn("remove VM network", "tap", n.Tap, "err", err)
		}
	}
}

func (b *Backend) cancelled(tk *task) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return tk.flags.cancelled
}

// launch prepares the disks and the network, boots the VM, hands it the
// session document and waits for its end.
func (b *Backend) launch(tk *task) {
	defer close(tk.launched)
	ctx := b.ctx
	t := tk.full
	b.setStatus(tk, rtcore.TaskStatus{State: rtcore.TaskStarting})
	c := b.o.Config

	img, err := b.o.Images.Ensure(ctx, t.Image, t.Platform.Arch, nil)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, image.ErrDiskFull) {
			msg = "disk full: " + msg
			if b.o.Prune != nil {
				b.o.Prune()
			}
		}
		b.fail(tk, rtcore.SandboxImagePull, msg)
		return
	}
	b.mu.Lock()
	tk.rec.ImageDigest = img.Digest
	b.mu.Unlock()
	tools, err := b.o.Images.EnsureTools(ctx, b.o.Version, t.Platform.Arch, b.o.Exe)
	if err != nil {
		b.fail(tk, rtcore.SandboxImagePull, "tools disk: "+err.Error())
		return
	}
	scratch, err := b.o.Images.EnsureScratch(ctx, c.Slot.ScratchGiB)
	if err != nil {
		b.fail(tk, rtcore.SandboxImagePull, "scratch disk: "+err.Error())
		return
	}
	if b.cancelled(tk) {
		b.fail(tk, rtcore.SandboxCancelled, "")
		return
	}

	// Git cache disks: only the session's repositories, never blocking.
	var cacheDisks []string
	var cacheKeys []string
	for _, r := range tk.extras.GitCache {
		if b.o.Cache == nil {
			break
		}
		if p := b.o.Cache.Latest(r.Key); p != "" {
			cacheDisks = append(cacheDisks, p)
			cacheKeys = append(cacheKeys, r.Key)
		}
		go func(r GitRepo) {
			if err := b.o.Cache.Ensure(context.WithoutCancel(ctx), r.Key, r.CloneURL); err != nil {
				b.log.Info("git cache", "key", r.Key, "err", err)
			}
		}(r)
	}

	// Snapshot restore when a template exists; otherwise build one for next time.
	var snap *vm.SnapshotFiles
	uid := c.Jail.UIDBase + tk.rec.Slot
	if b.o.Snapshots != nil && b.o.SnapKey != nil {
		if key := b.o.SnapKey(img, tools); key != "" {
			snap = b.o.Snapshots.Get(key)
			if snap == nil {
				b.o.Snapshots.EnsureAsync(key, vm.Spec{
					VMID: "vm-tpl-" + key[:8], Arch: t.Platform.Arch, Slot: 255, UID: c.Jail.UIDBase + 255, GID: c.Jail.UIDBase + 255,
					VCPUs: c.Slot.VCPUs, MemoryMiB: c.Slot.MemoryMiB, ScratchGiB: c.Slot.ScratchGiB,
					Kernel: b.o.Kernel, Tools: tools, Image: img.Path, Scratch: scratch, Debug: c.Debug.SerialConsole,
				})
			}
		}
	}
	// A restored VM keeps the template's drives: no cache disks.
	if snap != nil {
		cacheDisks, cacheKeys = nil, nil
	}

	mode := vmnet.Restricted
	if !rtcore.Restricted(t.NetworkPolicy) {
		mode = vmnet.Unrestricted
	}
	n, err := vmnet.Alloc(b.o.NetConfig.Subnet, tk.rec.Slot, uid, mode)
	if err != nil {
		b.fail(tk, rtcore.SandboxProcessFailed, "network: "+err.Error())
		return
	}
	if snap != nil {
		n.GuestMAC = vm.TemplateMAC
	}
	add, rm, err := vmnet.RenderVM(b.o.NetConfig, n)
	if err != nil {
		b.fail(tk, rtcore.SandboxProcessFailed, "network: "+err.Error())
		return
	}
	if err := b.o.Net.AddVM(ctx, n, add); err != nil {
		b.fail(tk, rtcore.SandboxProcessFailed, "network: "+err.Error())
		return
	}
	b.mu.Lock()
	tk.net, tk.netRm = &n, rm
	tk.rec.Net = mode.String()
	tk.rec.CacheKeys = cacheKeys
	b.mu.Unlock()
	hs := b.helperSpec(t, n)
	h, err := b.o.Helpers(ctx, hs, func(err error) { b.helperFailed(tk, err) })
	if err != nil {
		reason := rtcore.SandboxProcessFailed
		if mode == vmnet.Restricted {
			reason = rtcore.SandboxNetworkPolicyTimeout
		}
		b.fail(tk, reason, err.Error())
		return
	}
	b.mu.Lock()
	tk.helper = h
	tk.rec.HelperPID = h.PID()
	b.mu.Unlock()

	spec := vm.Spec{
		VMID: tk.rec.VMID, Arch: t.Platform.Arch, Slot: tk.rec.Slot, UID: uid, GID: uid,
		VCPUs: c.Slot.VCPUs, MemoryMiB: c.Slot.MemoryMiB, ScratchGiB: c.Slot.ScratchGiB,
		Kernel: b.o.Kernel, Tools: tools, Image: img.Path, Scratch: scratch, Cache: cacheDisks,
		Tap: n.Tap, GuestMAC: n.GuestMAC, Debug: c.Debug.SerialConsole, Snapshot: snap,
		DiskRate: 200 << 20,
		NetTx:    vm.RateLimit(int64(c.Network.Rate.TxMbit) * 1000 * 1000 / 8),
		NetRx:    vm.RateLimit(int64(c.Network.Rate.RxMbit) * 1000 * 1000 / 8),
	}
	specJSON, _ := json.Marshal(spec)
	b.mu.Lock()
	tk.rec.Spec = specJSON
	b.mu.Unlock()
	if b.cancelled(tk) {
		b.fail(tk, rtcore.SandboxCancelled, "")
		return
	}
	v, err := b.o.VMM.Start(ctx, spec)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, vm.ErrGuestStart) {
			msg = "guest did not start: " + msg
		}
		b.fail(tk, rtcore.SandboxProcessFailed, msg)
		return
	}
	b.mu.Lock()
	tk.vm = v
	b.mu.Unlock()
	doc := b.document(tk, img, n, cacheKeys)
	if err := v.Go(ctx, doc); err != nil {
		_ = v.Kill(ctx)
		b.fail(tk, rtcore.SandboxProcessFailed, "hand the session to the VM: "+err.Error())
		_ = v.Cleanup(context.Background())
		return
	}
	b.mu.Lock()
	tk.rec.StreamKey = v.StreamKey()
	b.mu.Unlock()
	b.supervise(tk, v)
}

// supervise reports running, arms the deadline and waits for the end.
func (b *Backend) supervise(tk *task, v vm.VM) {
	b.armDeadline(tk, v)
	exitCh := v.Wait()
	select {
	case <-v.Running():
		now := b.o.Now().UTC()
		b.mu.Lock()
		st := tk.rec.Status
		b.mu.Unlock()
		if st.State != rtcore.TaskRunning {
			if st.StartedAt == nil {
				st.StartedAt = &now
			}
			st.State = rtcore.TaskRunning
			b.setStatus(tk, st)
		}
	case e := <-exitCh:
		b.ended(tk, e)
		return
	}
	b.ended(tk, <-exitCh)
}

func (b *Backend) ended(tk *task, e vm.Exit) {
	b.mu.Lock()
	f := tk.flags
	b.mu.Unlock()
	reason, code, msg := MapExit(e, f)
	b.teardown(tk)
	b.finish(tk, reason, code, msg)
}

func (b *Backend) armDeadline(tk *task, v vm.VM) {
	d := tk.rec.Deadline.Sub(b.o.Now())
	if d < 0 {
		d = 0
	}
	b.mu.Lock()
	tk.deadline = time.AfterFunc(d, func() {
		b.mu.Lock()
		tk.flags.deadline = true
		b.mu.Unlock()
		b.log.Warn("task passed its deadline", "task", tk.rec.TaskID)
		_ = v.Cancel(context.Background(), 30*time.Second)
	})
	b.mu.Unlock()
}

func (b *Backend) helperFailed(tk *task, err error) {
	b.log.Warn("network helper failed", "task", tk.rec.TaskID, "err", err)
	b.mu.Lock()
	restricted := tk.rec.Net == vmnet.Restricted.String()
	if restricted {
		tk.flags.netFailed = true
	}
	v := tk.vm
	b.mu.Unlock()
	if restricted && v != nil {
		_ = v.Kill(context.Background())
	}
}

func (b *Backend) helperSpec(t rtcore.Task, n vmnet.VMNet) vmnet.HelperSpec {
	hs := vmnet.HelperSpec{VM: n, Exe: b.o.Exe, UID: b.o.Config.Jail.OfnetUID, Upstreams: b.o.NetConfig.Resolvers}
	if p := t.NetworkPolicy; p != nil {
		hs.Allowed, hs.Denied = p.AllowedHosts, p.DeniedHosts
	}
	if u, err := url.Parse(t.Env[rtcore.TaskEnvControlURL]); err == nil && u.Hostname() != "" {
		hs.AlwaysAllow = []string{u.Hostname()}
	}
	for _, a := range b.o.NetConfig.InternalAllow {
		hs.PrivateHosts = append(hs.PrivateHosts, a.Addr().String())
	}
	return hs
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// document builds the MMDS session document.
func (b *Backend) document(tk *task, img image.Image, n vmnet.VMNet, cacheKeys []string) mmds.Document {
	t := tk.full
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	p := mmds.Payload{
		V: mmds.Version, TaskID: t.TaskID,
		Hostname:          "of-" + strings.TrimPrefix(tk.rec.VMID[strings.LastIndex(tk.rec.VMID, "-")+1:], "-"),
		ClockUnixNs:       b.o.Now().UnixNano(),
		Entropy:           base64.StdEncoding.EncodeToString(entropy),
		MachineID:         randHex(16),
		User:              resolveUser(t.Platform.User, img.User),
		Image:             &mmds.Image{Env: img.Env, WorkingDir: img.WorkingDir},
		Env:               map[string]string{},
		Disks:             &mmds.Disks{Image: "image", Scratch: "scratch"},
		MaxSessionSeconds: t.MaxSessionSeconds,
	}
	for k, v := range t.Env {
		p.Env[k] = v
	}
	host := n.HostIP.String()
	nw := &mmds.Network{Mode: n.Mode.String(), Address: n.GuestPrefix(), Gateway: host, MTU: 1500, DNS: []string{}}
	if n.Mode == vmnet.Unrestricted {
		nw.DNS = []string{host}
	} else {
		nw.ProxyURL = "http://" + host + ":" + strconv.Itoa(vmnet.ProxyPort)
		nw.NoProxy = "localhost,127.0.0.1"
		nw.EgressReportURL = "http://" + host + ":" + strconv.Itoa(vmnet.ReportPort) + "/blocked"
	}
	p.Network = nw
	for i, k := range cacheKeys {
		p.Disks.Cache = append(p.Disks.Cache, mmds.CacheDisk{Drive: "cache-" + strconv.Itoa(i), Key: k})
	}
	return mmds.Document{OpenFactory: p}
}

// resolveUser picks the runner user: a numeric platform.user, else the
// image user (numeric, or a name that vm-init resolves), else 1001.
func resolveUser(platformUser, imageUser string) *mmds.User {
	u := &mmds.User{UID: 1001, GID: 1001, Home: "/home/agent"}
	pick := strings.TrimSpace(platformUser)
	if pick == "" || pick == "image" {
		pick = strings.TrimSpace(imageUser)
	}
	if pick == "" {
		return u
	}
	uidS, gidS, hasGID := strings.Cut(pick, ":")
	if uid, err := strconv.Atoi(uidS); err == nil && uid >= 0 {
		u.UID, u.GID = uid, uid
		if gid, err := strconv.Atoi(gidS); hasGID && err == nil && gid >= 0 {
			u.GID = gid
		}
		if uid == 0 {
			u.Home = "/root"
		}
		return u
	}
	u.Name = uidS
	return u
}

// Status returns the status of a task.
func (b *Backend) Status(_ context.Context, id string) (rtcore.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tk, ok := b.tasks[id]
	if !ok {
		return rtcore.TaskStatus{}, fmt.Errorf("task %s: %w", id, rtcore.ErrNotFound)
	}
	return tk.rec.Status, nil
}

// Watch sends the current status and each change until the task exited.
func (b *Backend) Watch(_ context.Context, id string) (<-chan rtcore.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tk, ok := b.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task %s: %w", id, rtcore.ErrNotFound)
	}
	ch := make(chan rtcore.TaskStatus, 8)
	ch <- tk.rec.Status
	if tk.rec.Status.State == rtcore.TaskExited {
		close(ch)
		return ch, nil
	}
	tk.watchers = append(tk.watchers, ch)
	return ch, nil
}

// Cancel asks the runner to stop. After grace seconds the VM is killed.
func (b *Backend) Cancel(ctx context.Context, id string, grace int) error {
	b.mu.Lock()
	tk, ok := b.tasks[id]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("task %s: %w", id, rtcore.ErrNotFound)
	}
	if tk.rec.Status.State == rtcore.TaskExited {
		b.mu.Unlock()
		return nil
	}
	tk.flags.cancelled = true
	v := tk.vm
	b.mu.Unlock()
	if v == nil {
		return nil // launch sees the flag
	}
	return v.Cancel(ctx, time.Duration(grace)*time.Second)
}

// Destroy kills the VM and removes its jail, network and record. It is safe
// to call twice and for an unknown task.
func (b *Backend) Destroy(ctx context.Context, id string) error {
	b.mu.Lock()
	tk, ok := b.tasks[id]
	if !ok || !tk.gone.IsZero() {
		b.mu.Unlock()
		return nil
	}
	tk.flags.cancelled = true
	v := tk.vm
	if tk.cleanup != nil {
		tk.cleanup.Stop()
	}
	b.mu.Unlock()
	if v != nil {
		_ = v.Kill(ctx)
		<-tk.launched
		_ = v.Cleanup(ctx)
	}
	b.teardown(tk)
	b.mu.Lock()
	if tk.rec.Status.State != rtcore.TaskExited {
		now := b.o.Now().UTC()
		b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxCancelled, EndedAt: &now})
	}
	if tk.rec.Slot >= 0 && tk.rec.Slot < len(b.slots) && b.slots[tk.rec.Slot] == id {
		b.slots[tk.rec.Slot] = ""
	}
	tk.gone = b.o.Now()
	b.mu.Unlock()
	if err := b.o.Store.Delete(id); err != nil {
		return err
	}
	time.AfterFunc(tombstoneTTL, func() {
		b.mu.Lock()
		if b.tasks[id] == tk {
			delete(b.tasks, id)
		}
		b.mu.Unlock()
	})
	return nil
}

// List returns every task that the worker holds, destroyed ones excluded.
func (b *Backend) List(context.Context) ([]rtcore.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]rtcore.TaskStatus, 0, len(b.tasks))
	for _, tk := range b.tasks {
		if tk.gone.IsZero() {
			out = append(out, tk.rec.Status)
		}
	}
	return out, nil
}

// Preflight reports whether the backend takes tasks.
func (b *Backend) Preflight(context.Context) error {
	if b.Draining() {
		return fmt.Errorf("worker is draining: %w", rtcore.ErrAtCapacity)
	}
	return nil
}

// InUse reports whether an image digest or a git cache key is in use.
func (b *Backend) InUse(v string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, tk := range b.tasks {
		if tk.rec.Status.State == rtcore.TaskExited {
			continue
		}
		if tk.rec.ImageDigest == v {
			return true
		}
		for _, k := range tk.rec.CacheKeys {
			if k == v {
				return true
			}
		}
	}
	return false
}

// adopt loads the records of an earlier agent process. After a host
// reboot every task that did not exit ends with node_lost. A VM that is
// gone without a reboot ends with unknown. Live VMs are adopted.
func (b *Backend) adopt(ctx context.Context) error {
	recs, err := b.o.Store.List()
	if err != nil {
		return err
	}
	var want []vm.AdoptRecord
	byVM := map[string]*task{}
	for _, r := range recs {
		tk := &task{rec: r, full: r.Task, launched: make(chan struct{})}
		close(tk.launched)
		b.tasks[r.TaskID] = tk
		if r.Status.State == rtcore.TaskExited {
			continue
		}
		now := b.o.Now().UTC()
		if r.BootID != "" && b.o.BootID != "" && r.BootID != b.o.BootID {
			b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxNodeLost, Message: "the host rebooted", EndedAt: &now})
			continue
		}
		var spec vm.Spec
		if len(r.Spec) == 0 || json.Unmarshal(r.Spec, &spec) != nil || r.StreamKey == "" {
			b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxUnknown, Message: "the agent restarted before the VM started", EndedAt: &now})
			continue
		}
		want = append(want, vm.AdoptRecord{VMID: r.VMID, Spec: spec, StreamKey: r.StreamKey})
		byVM[r.VMID] = tk
	}
	vms, err := b.o.VMM.Adopt(ctx, want)
	if err != nil {
		return err
	}
	live := map[string]vm.VM{}
	for _, v := range vms {
		live[v.ID()] = v
	}
	for id, tk := range byVM {
		v, ok := live[id]
		if !ok {
			now := b.o.Now().UTC()
			b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxUnknown, Message: "the VM is gone after an agent restart", EndedAt: &now})
			continue
		}
		if tk.rec.Slot >= 0 && tk.rec.Slot < len(b.slots) {
			b.slots[tk.rec.Slot] = tk.rec.TaskID
		}
		tk.vm = v
		b.adoptNetwork(ctx, tk)
		b.log.Info("adopted VM", "task", tk.rec.TaskID, "vm", id)
		go b.supervise(tk, v)
	}
	return nil
}

// adoptNetwork restarts the network helper of an adopted VM. The tap and
// the rules stay from the earlier agent.
func (b *Backend) adoptNetwork(ctx context.Context, tk *task) {
	mode := vmnet.Restricted
	if tk.rec.Net == vmnet.Unrestricted.String() {
		mode = vmnet.Unrestricted
	}
	n, err := vmnet.Alloc(b.o.NetConfig.Subnet, tk.rec.Slot, b.o.Config.Jail.UIDBase+tk.rec.Slot, mode)
	if err != nil {
		return
	}
	var spec vm.Spec
	if json.Unmarshal(tk.rec.Spec, &spec) == nil && len(spec.GuestMAC) == 6 {
		n.GuestMAC = spec.GuestMAC
	}
	_, rm, _ := vmnet.RenderVM(b.o.NetConfig, n)
	tk.net, tk.netRm = &n, rm
	if tk.rec.HelperPID > 0 {
		killProcess(tk.rec.HelperPID)
	}
	h, err := b.o.Helpers(ctx, b.helperSpec(tk.full, n), func(err error) { b.helperFailed(tk, err) })
	if err != nil {
		b.log.Warn("restart network helper", "task", tk.rec.TaskID, "err", err)
		if mode == vmnet.Restricted {
			tk.flags.netFailed = true
			go func() { _ = tk.vm.Kill(context.Background()) }() //nolint:gosec // G118: the kill must not end with the start context
		}
		return
	}
	tk.helper = h
	tk.rec.HelperPID = h.PID()
	_ = b.o.Store.Put(tk.rec)
}
