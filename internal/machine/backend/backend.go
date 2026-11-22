// Package backend is the task backend of a microVM worker. It implements
// rtcore.TaskBackend (and direct.ExtrasBackend) over the VMM, the image
// store, the VM network, the git cache and the task records, so the worker
// protocol client of internal/sandbox/direct drives it like any backend.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/image"
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
	// The backend calls it without holding its lock, so it may call InUse.
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
	watchers []*watcher
	deadline *time.Timer
	cleanup  *time.Timer
	gone     time.Time // destroyed at
	// ctx is the context of the launch; stop ends it (Destroy, or the end
	// of the task).
	ctx  context.Context
	stop context.CancelFunc
	// launched closes when the VM of the task ended, or when the task ended
	// without a VM. Until then the task holds its slot and its network.
	launched chan struct{}
}

// watcher is one Watch channel. done closes with ch on the exited status.
type watcher struct {
	ch   chan rtcore.TaskStatus
	done chan struct{}
}

func (b *Backend) newTask(rec store.Record, full rtcore.Task, ex Extras) *task {
	ctx, stop := context.WithCancel(b.ctx)
	return &task{rec: rec, full: full, extras: ex, ctx: ctx, stop: stop, launched: make(chan struct{})}
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

var _ rtcore.Drainable = (*Backend)(nil)

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
		if s == t.TaskID {
			// Another Start of this task checks the disk now.
			b.mu.Unlock()
			return rtcore.TaskStatus{}, fmt.Errorf("task %s is starting: %w", t.TaskID, rtcore.ErrAtCapacity)
		}
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
	// Reserve the slot, then check the disk without the lock: Prune calls InUse.
	b.slots[slot] = t.TaskID
	b.mu.Unlock()
	release := func() {
		if b.slots[slot] == t.TaskID {
			b.slots[slot] = ""
		}
	}
	if err := b.checkDisk(live); err != nil {
		b.mu.Lock()
		release()
		b.mu.Unlock()
		return rtcore.TaskStatus{}, err
	}
	now := b.o.Now().UTC()
	st := rtcore.TaskStatus{TaskID: t.TaskID, State: rtcore.TaskAccepted, SandboxID: vm.VMID(slot, t.TaskID)}
	tk := b.newTask(store.Record{
		TaskID: t.TaskID, VMID: st.SandboxID, Slot: slot, Status: st, BootID: b.o.BootID, Task: t,
		Deadline: now.Add(time.Duration(t.MaxSessionSeconds+60) * time.Second),
	}, t, ex)
	b.mu.Lock()
	if err := b.o.Store.Put(tk.rec); err != nil {
		release()
		b.mu.Unlock()
		tk.stop()
		return rtcore.TaskStatus{}, fmt.Errorf("record task: %w", err)
	}
	b.tasks[t.TaskID] = tk
	b.mu.Unlock()
	go b.launch(tk) //nolint:gosec // G118: the VM outlives the request; launch uses the task context
	return st, nil
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
	for _, w := range tk.watchers {
		select {
		case w.ch <- st:
		default:
			// Drop the oldest status for a slow watcher; the final one must arrive.
			select {
			case <-w.ch:
			default:
			}
			w.ch <- st
		}
		if st.State == rtcore.TaskExited {
			close(w.ch)
			close(w.done)
		}
	}
	if st.State == rtcore.TaskExited {
		tk.watchers = nil
	}
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

// Watch sends the current status and each change. The channel closes after
// the exited status or when ctx ends.
func (b *Backend) Watch(ctx context.Context, id string) (<-chan rtcore.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tk, ok := b.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task %s: %w", id, rtcore.ErrNotFound)
	}
	w := &watcher{ch: make(chan rtcore.TaskStatus, 8), done: make(chan struct{})}
	w.ch <- tk.rec.Status
	if tk.rec.Status.State == rtcore.TaskExited {
		close(w.ch)
		return w.ch, nil
	}
	tk.watchers = append(tk.watchers, w)
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			defer b.mu.Unlock()
			if i := slices.Index(tk.watchers, w); i >= 0 {
				tk.watchers = slices.Delete(tk.watchers, i, i+1)
				close(w.ch)
			}
		case <-w.done:
		}
	}()
	return w.ch, nil
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
	b.mu.Unlock()
	// End a launch in progress: the context stops the image pull and the VM
	// start, and a VM that starts anyway sees the flag and is killed.
	tk.stop()
	if v != nil {
		_ = v.Kill(ctx)
	}
	// launch returns after the VM ended. Release nothing before that: a new
	// task could get the slot and the tap of a VM that still runs.
	select {
	case <-tk.launched:
	case <-ctx.Done():
		return fmt.Errorf("destroy task %s: wait for the VM to stop: %w", id, ctx.Err())
	}
	b.mu.Lock()
	v = tk.vm
	if tk.cleanup != nil {
		tk.cleanup.Stop()
	}
	b.mu.Unlock()
	if v != nil {
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
