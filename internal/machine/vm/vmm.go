package vm

import (
	"context"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// VMM starts and adopts VMs.
type VMM interface {
	// Start boots a VM for spec (or restores it from spec.Snapshot) and
	// returns when vm-init sent hello, or an error.
	Start(ctx context.Context, spec Spec) (VM, error)
	// Adopt finds the VMs of an earlier agent process. It returns the VMs
	// whose process still runs; vm-init reconnects to them.
	Adopt(ctx context.Context, recs []AdoptRecord) ([]VM, error)
}

// AdoptRecord is what the agent stored about a VM.
type AdoptRecord struct {
	VMID      string
	Spec      Spec
	StreamKey string
}

// VM is one running microVM.
type VM interface {
	ID() string
	// StreamKey is the key that vm-init presents when it reconnects.
	StreamKey() string
	// Go writes the session document to MMDS, sends go, waits for
	// mmds_read and clears MMDS.
	Go(ctx context.Context, doc mmds.Document) error
	// Running is closed when the runner process started.
	Running() <-chan struct{}
	// Cancel asks vm-init to stop the runner with a grace period, and
	// kills the VM 5 s after the grace.
	Cancel(ctx context.Context, grace time.Duration) error
	// Kill ends the VM at once.
	Kill(ctx context.Context) error
	// Wait sends one Exit when the VM ended, then closes.
	Wait() <-chan Exit
	// Cleanup removes the jail and the cgroup. It is idempotent.
	Cleanup(ctx context.Context) error
}

// Exit is what the host knows when a VM ended. The backend maps it to a
// sandbox reason.
type Exit struct {
	// Report is the exit frame of vm-init, or nil when none arrived.
	Report *ctlstream.Frame
	// HostOOMKills is the oom_kill counter of the VM cgroup: the VMM
	// passed its host memory limit.
	HostOOMKills int
	// Killed: the host killed the VM (cancel timeout or destroy).
	Killed bool
	// Message describes a host-side failure, for example "guest did not start".
	Message string
}

// Template is a running template VM that the snapshot manager pauses and
// saves.
type Template interface {
	VM
	// Snapshot pauses the VM, writes vm.state and mem in the jail and
	// returns their host paths and the jail scratch disk.
	Snapshot(ctx context.Context) (SnapshotFiles, error)
}
