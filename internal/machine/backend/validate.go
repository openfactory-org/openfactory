package backend

import (
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/gitcachekey"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

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
// counts with its full size. Call it without b.mu: Prune calls InUse.
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
