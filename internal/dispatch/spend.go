package dispatch

import (
	"context"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runstate"
	"github.com/openfactory-org/openfactory/internal/spend"
)

// maxTreeDepth bounds the walk to the root of a run tree.
const maxTreeDepth = 64

// spendHold tells whether a queued run must wait because its factory or team
// is at a spend limit. The first time, it sets the
// queued reason spend_limit and writes run.queued. When the limit no longer
// applies, it clears the reason and restarts the queue timeout. pass holds
// the results of this claim pass by team and factory.
func (d *Dispatcher) spendHold(ctx context.Context, run *core.Run, pass map[string]*spend.Limit) (bool, error) {
	key := run.TeamID + "/" + run.FactoryID
	lim, seen := pass[key]
	if !seen {
		var err error
		if lim, err = d.cfg.Spend.Exceeded(ctx, run); err != nil {
			return false, err
		}
		pass[key] = lim
	}
	if lim != nil {
		if run.QueuedReason == core.QueuedSpendLimit {
			return true, nil
		}
		r, wrote, err := d.mutateRun(ctx, run.ID, func(r *core.Run) bool {
			if r.State != core.RunQueued || r.QueuedReason == core.QueuedSpendLimit {
				return false
			}
			r.QueuedReason = core.QueuedSpendLimit
			return true
		})
		if err != nil {
			return true, err
		}
		if wrote {
			*run = *r
			d.log.Info("dispatch: run waits for the spend limit", "run", run.ID, "factory", run.FactoryID,
				"scope", lim.Scope, "period", lim.Period, "spent_usd", lim.SpentUSD, "limit_usd", lim.LimitUSD)
			d.timeline(ctx, run.ID, d.latestSessionNumber(ctx, run.ID), "run.queued",
				fmt.Sprintf("run.queued:%s:spend_limit:%s:%s", run.ID, lim.Period, lim.Start.Format("2006-01-02")),
				map[string]any{"reason": core.QueuedSpendLimit, "scope": lim.Scope, "period": lim.Period,
					"spent_usd": lim.SpentUSD, "limit_usd": lim.LimitUSD, "resets_at": lim.ResetsAt})
		}
		return true, nil
	}
	if run.QueuedReason == core.QueuedSpendLimit {
		now := d.now()
		r, wrote, err := d.mutateRun(ctx, run.ID, func(r *core.Run) bool {
			if r.QueuedReason != core.QueuedSpendLimit {
				return false
			}
			r.QueuedReason = ""
			if r.State == core.RunQueued {
				r.QueuedAt = &now // the queue timeout counts from the release
			}
			return true
		})
		if err != nil {
			return true, err
		}
		if wrote {
			*run = *r
		}
		if run.State != core.RunQueued {
			return true, nil
		}
	}
	return false, nil
}

// spendAtTurnEnd stops the run tree when the spend passed a limit at a turn end.
func (d *Dispatcher) spendAtTurnEnd(ctx context.Context, r *core.Run) {
	if d.cfg.Spend == nil {
		return
	}
	lim, err := d.cfg.Spend.Exceeded(ctx, r)
	if err != nil {
		d.log.Warn("dispatch: spend check at turn end", "run", r.ID, "err", err)
		return
	}
	if lim != nil {
		d.stopTreeForSpend(ctx, r, lim)
	}
}

// spendStopsTurn checks the spend before a new turn of a live session. When
// a limit is reached, it stops the run tree and returns true.
func (d *Dispatcher) spendStopsTurn(ctx context.Context, runID string) bool {
	if d.cfg.Spend == nil {
		return false
	}
	r, err := d.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return false
	}
	lim, err := d.cfg.Spend.Exceeded(ctx, r)
	if err != nil {
		d.log.Warn("dispatch: spend check before a turn", "run", runID, "err", err)
		return false
	}
	if lim == nil {
		return false
	}
	d.stopTreeForSpend(ctx, r, lim)
	return true
}

// stopTreeForSpend moves each run of the tree of r that is not finished (and
// each succeeded, blocked or failed run) to error with spend_limit_exceeded,
// and stops its live session.
func (d *Dispatcher) stopTreeForSpend(ctx context.Context, r *core.Run, lim *spend.Limit) {
	root := r
	for i := 0; i < maxTreeDepth && root.ParentRunID != ""; i++ {
		p, err := d.cfg.Store.GetRun(ctx, root.ParentRunID)
		if err != nil {
			break
		}
		root = p
	}
	msg := fmt.Sprintf("spend limit reached: the %s %s spend is $%.2f of $%.2f; the limit resets at %s",
		lim.Scope, lim.Period, lim.SpentUSD, lim.LimitUSD, lim.ResetsAt.Format("2006-01-02 15:04 MST"))
	d.log.Warn("dispatch: spend limit reached, stopping the run tree", "run", r.ID, "root", root.ID,
		"scope", lim.Scope, "period", lim.Period, "spent_usd", lim.SpentUSD, "limit_usd", lim.LimitUSD)
	queue := []string{root.ID}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		_, changed, err := d.transition(ctx, id, runstate.Input{Event: runstate.EventSpendLimit}, nil)
		if err != nil {
			d.log.Warn("dispatch: spend stop", "run", id, "err", err)
		}
		if changed {
			if _, err := d.update(ctx, id, func(x *core.Run) {
				if x.FailureCode == core.FailureSpendLimitExceeded {
					x.Error = msg
				}
			}); err != nil {
				d.log.Warn("dispatch: spend stop message", "run", id, "err", err)
			}
		}
		if err := d.StopSession(ctx, id); err != nil {
			d.log.Warn("dispatch: stop session for spend", "run", id, "err", err)
		}
		children, err := d.cfg.Store.ListRuns(ctx, core.RunFilter{ParentRunID: id})
		if err != nil {
			d.log.Warn("dispatch: list child runs", "run", id, "err", err)
			continue
		}
		for _, c := range children {
			queue = append(queue, c.ID)
		}
	}
}
