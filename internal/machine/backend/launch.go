package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// fail ends a task that never got a running VM. A cancelled task ends as
// cancelled: its launch fails because Destroy ended the launch context.
func (b *Backend) fail(tk *task, reason rtcore.SandboxReason, msg string) {
	if b.cancelled(tk) {
		reason, msg = rtcore.SandboxCancelled, ""
	}
	b.log.Warn("task failed", "task", tk.rec.TaskID, "reason", reason, "msg", msg)
	b.teardown(tk)
	b.finish(tk, reason, nil, msg)
}

func (b *Backend) finish(tk *task, reason rtcore.SandboxReason, code *int, msg string) {
	now := b.o.Now().UTC()
	tk.stop()
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
	ctx := tk.ctx
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
		// The mirror serves later tasks too: it lives with the backend, not the task.
		go func(r GitRepo) {
			if err := b.o.Cache.Ensure(b.ctx, r.Key, r.CloneURL); err != nil {
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
				b.o.Snapshots.EnsureAsync(b.ctx, key, vm.Spec{
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
	cancelled := tk.flags.cancelled
	b.mu.Unlock()
	if cancelled {
		// Destroy or Cancel came while the VM started and saw no VM to stop.
		_ = v.Kill(context.Background())
		select {
		case <-v.Wait():
		case <-time.After(30 * time.Second):
		}
		b.fail(tk, rtcore.SandboxCancelled, "")
		return
	}
	doc := b.document(tk, img, n, cacheKeys)
	if err := v.Go(ctx, doc); err != nil {
		// fail keeps the jail for the failure retention time, then cleans it.
		_ = v.Kill(context.Background())
		b.fail(tk, rtcore.SandboxProcessFailed, "hand the session to the VM: "+err.Error())
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
