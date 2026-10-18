package server

import (
	"context"
	"errors"

	"github.com/openfactory-org/openfactory/internal/core"
)

// activeTurnSource returns the function that gives the active turn of a run
// from the latest session in the runner store. It returns nil when the runner
// path is off, so that orchestration tools run without idempotency.
func (m *runnerMode) activeTurnSource() func(ctx context.Context, runID string) (string, error) {
	_, store := m.verifierParts()
	if store == nil {
		return nil
	}
	return func(ctx context.Context, runID string) (string, error) {
		s, err := store.LatestRunSession(ctx, runID)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return "", nil
			}
			return "", err
		}
		if s.State != core.SessionActive {
			return "", nil
		}
		return s.ActiveTurnID, nil
	}
}
