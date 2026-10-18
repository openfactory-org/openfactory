package dispatch

import (
	"context"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// leaderLease is the name of the leader lease of the dispatcher.
const leaderLease = "dispatch"

// leaderTTL is the life of the leader lease. Each tick renews it.
const leaderTTL = 30 * time.Second

// isLeader takes or renews the leader lease. A store without coordination
// has one replica, so that replica is the leader. An error of the store
// means "not the leader" for this tick.
func (d *Dispatcher) isLeader(ctx context.Context) bool {
	c, ok := d.cfg.Store.(core.Coordinator)
	if !ok {
		return true
	}
	now := d.now()
	ok, err := c.AcquireLeadership(ctx, leaderLease, d.cfg.ReplicaID, now, now.Add(leaderTTL))
	if err != nil {
		d.log.Warn("dispatch: leader lease", "err", err)
		return false
	}
	return ok
}

// ReleaseLeadership gives the leader lease up when this replica holds it. A
// replica that stops cleanly calls it, so that another replica takes the
// lease at its next tick, and not when the lease ends (leaderTTL). It does
// nothing for a store without coordination, and when another replica holds
// the lease.
func (d *Dispatcher) ReleaseLeadership(ctx context.Context) {
	c, ok := d.cfg.Store.(core.Coordinator)
	if !ok {
		return
	}
	if err := c.ReleaseLeadership(ctx, leaderLease, d.cfg.ReplicaID); err != nil {
		d.log.Warn("dispatch: release leader lease", "err", err)
	}
}
