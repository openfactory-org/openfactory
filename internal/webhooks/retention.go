package webhooks

import (
	"context"
	"log/slog"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// DefaultRetentionEvery is the interval of the delivery retention sweep.
const DefaultRetentionEvery = time.Hour

// SweepDeliveries applies the retention of webhook deliveries:
// for each webhook, the newest
// core.WebhookKeepNewest rows keep their body. Older rows lose their body and
// headers but keep their identity (the delivery ID), so a retry is still found
// as a duplicate. They are deleted after core.WebhookIdentityRetention. A store
// without core.WebhookDeliveryPruner has no retention: the sweep does nothing.
func SweepDeliveries(ctx context.Context, store core.Store, now time.Time) (core.WebhookPruneResult, error) {
	p, ok := store.(core.WebhookDeliveryPruner)
	if !ok {
		return core.WebhookPruneResult{}, nil
	}
	return p.PruneWebhookDeliveries(ctx, core.WebhookKeepNewest, now.Add(-core.WebhookIdentityRetention))
}

// RetentionLoop runs SweepDeliveries every `every` (default
// DefaultRetentionEvery) until ctx ends. Only the leader sweeps: isLeader nil
// means that this process is the only one. It returns at once when the store
// has no retention.
func RetentionLoop(ctx context.Context, store core.Store, isLeader func() bool, every time.Duration, now func() time.Time, log *slog.Logger) {
	if _, ok := store.(core.WebhookDeliveryPruner); !ok {
		return
	}
	if every <= 0 {
		every = DefaultRetentionEvery
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if isLeader != nil && !isLeader() {
			continue
		}
		res, err := SweepDeliveries(ctx, store, now())
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("webhook delivery retention", "err", err)
			}
			continue
		}
		if res.Stripped > 0 || res.Deleted > 0 {
			log.Info("webhook delivery retention", "stripped", res.Stripped, "deleted", res.Deleted)
		}
	}
}
