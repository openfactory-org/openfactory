package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// orphanMinAge is the age that a task must have before the sweep destroys it.
// A task that is younger can be a task that the dispatcher just started.
const orphanMinAge = 2 * time.Minute

// sweepIfDue runs the orphan sweep when SweepEvery passed since the last one.
func (d *Dispatcher) sweepIfDue(ctx context.Context) error {
	now := d.now()
	d.sweepMu.Lock()
	if !d.lastSweep.IsZero() && now.Sub(d.lastSweep) < d.cfg.SweepEvery {
		d.sweepMu.Unlock()
		return nil
	}
	d.lastSweep = now
	d.sweepMu.Unlock()
	return d.sweep(ctx)
}

// sweep destroys the tasks of the backend that have no open session.
// A task with an ID of an unknown
// format can belong to someone else. The sweep skips it.
func (d *Dispatcher) sweep(ctx context.Context) error {
	var first error
	for _, b := range d.allBackends() {
		if err := d.sweepBackend(ctx, b); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// sweepBackend sweeps the tasks of one backend. A backend that gives no
// answer, for example a worker that is not connected, is skipped.
func (d *Dispatcher) sweepBackend(ctx context.Context, backend core.TaskBackend) error {
	tasks, err := backend.List(ctx)
	if errors.Is(err, core.ErrUnavailable) {
		d.log.Debug("dispatch: sweep skips backend", "backend", backend.Name(), "err", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("sweep: list tasks of %s: %w", backend.Name(), err)
	}
	now := d.now()
	var first error
	for _, t := range tasks {
		runID, n, err := core.ParseTaskID(t.TaskID)
		if err != nil {
			d.log.Debug("dispatch: sweep skips task with unknown id", "task", t.TaskID)
			continue
		}
		if t.StartedAt != nil && now.Sub(*t.StartedAt) < orphanMinAge {
			continue
		}
		s, err := d.cfg.Runner.GetRunSession(ctx, runID, n)
		switch {
		case err == nil && s.State != core.SessionEnded:
			continue
		case err != nil && !errors.Is(err, core.ErrNotFound):
			if first == nil {
				first = fmt.Errorf("sweep: session of task %s: %w", t.TaskID, err)
			}
			continue
		}
		d.log.Info("dispatch: sweep destroys orphan task", "task", t.TaskID)
		if err := backend.Destroy(ctx, t.TaskID); err != nil {
			d.log.Warn("dispatch: sweep destroy task", "task", t.TaskID, "err", err)
			if first == nil {
				first = fmt.Errorf("sweep: destroy task %s: %w", t.TaskID, err)
			}
		}
	}
	return first
}
