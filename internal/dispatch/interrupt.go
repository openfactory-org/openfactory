package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// Interrupt asks the runner of a run to stop the active turn and keep the
// session. It returns *core.InterruptError if the run cannot be interrupted.
func (d *Dispatcher) Interrupt(ctx context.Context, runID string) error {
	s, err := d.cfg.Runner.LatestRunSession(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return &core.InterruptError{Reason: core.InterruptNoActiveTurn}
		}
		return err
	}
	if s.State != core.SessionActive || s.ActiveTurnID == "" {
		return &core.InterruptError{Reason: core.InterruptNoActiveTurn}
	}
	if !hasCapability(s, runnerproto.CapInterrupt) {
		return &core.InterruptError{Reason: core.InterruptUnsupportedHarness}
	}
	c := d.getCommander()
	if c == nil {
		return fmt.Errorf("dispatch: no commander, cannot interrupt run %s", runID)
	}
	_, err = c.EnqueueCommand(ctx, s.RunID, s.Session, runnerproto.CommandTurnInterrupt,
		runnerproto.TurnInterruptData{TurnID: s.ActiveTurnID})
	return err
}
