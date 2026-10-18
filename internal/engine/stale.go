package engine

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// retryStale runs fn, a full read, change and write cycle, again when it
// fails with core.ErrStale. See storeutil.RetryStale.
func retryStale(ctx context.Context, fn func() error) error {
	return storeutil.RetryStale(ctx, fn)
}

// atomicRetry runs fn in one transaction (see Atomic) and runs the whole
// transaction again when it fails with core.ErrStale.
func (e *Engine) atomicRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	return retryStale(ctx, func() error { return e.Atomic(ctx, fn) })
}

// updateRun reads a run, applies fn and writes the run when fn returns true.
// It reads and applies again when the write fails with core.ErrStale. It
// joins a transaction carried by ctx.
func (e *Engine) updateRun(ctx context.Context, id string, fn func(r *core.Run) bool) (*core.Run, bool, error) {
	var out *core.Run
	wrote := false
	err := retryStale(ctx, func() error {
		wrote = false
		r, err := e.store.GetRun(ctx, id)
		if err != nil {
			return err
		}
		out = r
		if !fn(r) {
			return nil
		}
		if err := e.store.UpdateRun(ctx, r); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return out, wrote, err
}
