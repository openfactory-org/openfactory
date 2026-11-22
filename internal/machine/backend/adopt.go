package backend

import (
	"context"
	"encoding/json"

	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

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
		tk := b.newTask(r, r.Task, Extras{})
		b.tasks[r.TaskID] = tk
		if r.Status.State == rtcore.TaskExited {
			tk.stop()
			close(tk.launched)
			continue
		}
		now := b.o.Now().UTC()
		if r.BootID != "" && b.o.BootID != "" && r.BootID != b.o.BootID {
			b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxNodeLost, Message: "the host rebooted", EndedAt: &now})
			tk.stop()
			close(tk.launched)
			continue
		}
		var spec vm.Spec
		if len(r.Spec) == 0 || json.Unmarshal(r.Spec, &spec) != nil || r.StreamKey == "" {
			b.setStatusLocked(tk, rtcore.TaskStatus{State: rtcore.TaskExited, Reason: rtcore.SandboxUnknown, Message: "the agent restarted before the VM started", EndedAt: &now})
			tk.stop()
			close(tk.launched)
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
			tk.stop()
			close(tk.launched)
			continue
		}
		if tk.rec.Slot >= 0 && tk.rec.Slot < len(b.slots) {
			b.slots[tk.rec.Slot] = tk.rec.TaskID
		}
		tk.vm = v
		b.adoptNetwork(ctx, tk)
		b.log.Info("adopted VM", "task", tk.rec.TaskID, "vm", id)
		go func() {
			defer close(tk.launched)
			b.supervise(tk, v)
		}()
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
