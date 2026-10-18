package storeutil

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// StaleAttempts is how many times RetryStale runs a read, change and write
// cycle before it gives up.
const StaleAttempts = 8

// RetryStale runs fn, a full read, change and write cycle, again when it
// fails with core.ErrStale: another writer changed the row after fn read it.
// fn must read the row itself each time it runs, and must have no side
// effect before its write succeeds, except effects that roll back with a
// failed transaction. Other errors, including a plain core.ErrConflict,
// return at once.
func RetryStale(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = fn()
		if !errors.Is(err, core.ErrStale) || attempt == StaleAttempts {
			return err
		}
		// A short wait with jitter lets the other writer finish.
		wait := time.Duration(attempt) * time.Duration(1+rand.IntN(3)) * time.Millisecond //nolint:gosec // retry jitter, not a secret
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// UpdateWorkItem reads a work item, applies fn and writes it. It reads and
// applies again when the write fails with core.ErrStale. An error from fn
// stops it without a write. It returns the stored work item.
func UpdateWorkItem(ctx context.Context, st core.Store, id string, fn func(w *core.WorkItem) error) (*core.WorkItem, error) {
	var out *core.WorkItem
	err := RetryStale(ctx, func() error {
		w, err := st.GetWorkItem(ctx, id)
		if err != nil {
			return err
		}
		if err := fn(w); err != nil {
			return err
		}
		if err := st.UpdateWorkItem(ctx, w); err != nil {
			return err
		}
		out = w
		return nil
	})
	return out, err
}
