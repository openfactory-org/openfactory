package server

import (
	"context"
	"log/slog"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/spend"
)

// newSpendTracker builds the spend limit tracker: the team limit of
// the server settings and spendLimit of each factory's active definition.
func newSpendTracker(cfg *config.Config, st core.Store, bus core.EventBus, log *slog.Logger) *spend.Tracker {
	daily, monthly := cfg.TeamSpendLimitUSD()
	return spend.New(spend.Options{
		Store:     st,
		TeamLimit: core.SpendLimit{DailyUSD: daily, MonthlyUSD: monthly},
		FactoryLimit: func(ctx context.Context, factoryID string) (*core.SpendLimit, error) {
			rt, err := engine.LoadFactoryRuntime(ctx, st, factoryID)
			if err != nil {
				return nil, err
			}
			return rt.SpendLimit, nil
		},
		OnInbox: func(ctx context.Context, item *core.InboxItem) {
			if bus != nil {
				cp := *item
				bus.Publish(ctx, core.TopicInbox, &cp)
			}
		},
		Logger: log,
	})
}
