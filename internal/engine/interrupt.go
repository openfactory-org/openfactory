package engine

import "context"

// Interrupt stops the active turn of a run and keeps its session.
func (e *Engine) Interrupt(ctx context.Context, runID string) error {
	if _, err := e.store.GetRun(ctx, runID); err != nil {
		return err
	}
	return e.runner.Interrupt(ctx, runID)
}
