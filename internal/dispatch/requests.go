package dispatch

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
)

// hasOpenRequest reports whether the run has an unresolved question or
// approval for a person.
func (d *Dispatcher) hasOpenRequest(ctx context.Context, runID string) (bool, error) {
	run, err := d.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return false, err
	}
	items, err := d.cfg.Store.ListInboxItems(ctx, run.FactoryID, false)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if it.RunID != runID || it.Resolved {
			continue
		}
		if it.Kind == core.InboxQuestion || it.Kind == core.InboxApproval {
			return true, nil
		}
	}
	return false, nil
}
