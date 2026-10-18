package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

// The outcomes of Send.
const (
	// OutcomeLiveSession means that a live session got the message.
	OutcomeLiveSession = "live_session"
	// OutcomeNewSession means that the message starts a new session.
	OutcomeNewSession = "new_session"
	// OutcomeQueued means that a session in setup or in the queue gets the message.
	OutcomeQueued = "queued"
	// OutcomeStored means that the message waits in the queue for the next turn, and nothing starts.
	OutcomeStored = "stored"
)

// Send adds a message to the queue of a run and delivers it. A message with a
// known dedupe key is not added again and gives the same kind of outcome.
func (d *Dispatcher) Send(ctx context.Context, m *core.RunInput) (string, error) {
	created, err := d.cfg.Runner.AppendRunInput(ctx, m)
	if err != nil {
		return "", err
	}
	run, err := d.cfg.Store.GetRun(ctx, m.RunID)
	if err != nil {
		return "", err
	}
	latest, err := d.cfg.Runner.LatestRunSession(ctx, m.RunID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return "", err
	}
	outcome := outcomeFor(run.State, latest)
	if outcome == OutcomeNewSession && !messageStartsTurn(*m) {
		outcome = OutcomeStored
	}
	if !created {
		return outcome, nil
	}
	switch outcome {
	case OutcomeLiveSession:
		d.deliver(ctx, m.RunID)
	case OutcomeNewSession:
		if err := d.requeue(ctx, m.RunID); err != nil {
			return "", err
		}
	}
	return outcome, nil
}

func outcomeFor(state core.RunState, latest *core.RunSession) string {
	if latest != nil && latest.State == core.SessionActive {
		return OutcomeLiveSession
	}
	switch state {
	case core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning:
		return OutcomeQueued
	}
	return OutcomeNewSession
}

// requeue moves a run without a live session back to queued.
func (d *Dispatcher) requeue(ctx context.Context, runID string) error {
	r, _, err := d.transition(ctx, runID, runstate.Input{Event: runstate.EventMessage, SandboxRunning: false}, nil)
	if err != nil {
		return err
	}
	if r.State != core.RunQueued {
		return fmt.Errorf("%w: run %s is %s and cannot continue", core.ErrConflict, runID, r.State)
	}
	return nil
}

// StopSession tells the live session of a run to cancel. For a session that
// is only assigned, it also cancels the task. It does not change the run.
func (d *Dispatcher) StopSession(ctx context.Context, runID string) error {
	latest, err := d.cfg.Runner.LatestRunSession(ctx, runID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	if latest != nil && latest.State != core.SessionEnded {
		d.cancelSession(ctx, latest)
	}
	return nil
}

// Cancel cancels a run. It does not cancel child runs; the engine does that.
func (d *Dispatcher) Cancel(ctx context.Context, runID string) error {
	if err := d.StopSession(ctx, runID); err != nil {
		return err
	}
	_, _, err := d.transition(ctx, runID, runstate.Input{Event: runstate.EventCancel}, nil)
	return err
}

func (d *Dispatcher) cancelSession(ctx context.Context, s *core.RunSession) {
	if c := d.getCommander(); c != nil {
		if _, err := c.EnqueueCommand(ctx, s.RunID, s.Session, runnerproto.CommandSessionCancel, nil); err != nil {
			d.log.Warn("dispatch: enqueue session.cancel", "run", s.RunID, "session", s.Session, "err", err)
		}
	} else {
		d.log.Error("dispatch: no commander, cannot send session.cancel", "run", s.RunID)
	}
	// A runner reads commands only after its setup. Before session.ready, the
	// command is not enough, so the task is cancelled too.
	if s.State == core.SessionAssigned || s.ReadyAt == nil {
		if err := d.backendOf(s).Cancel(ctx, s.TaskID, int(d.cfg.StopGrace.Seconds())); err != nil {
			d.log.Warn("dispatch: cancel task", "task", s.TaskID, "err", err)
		}
		// The runner may not be able to report the end: a signal during its
		// session start stops it before it holds a session token. The session
		// then stayed open until the liveness check. End it here. The orphan
		// sweep destroys the task.
		if _, _, err := d.endSession(ctx, s.RunID, s.Session, string(runnerproto.EndCancelled), ""); err != nil {
			d.log.Warn("dispatch: end cancelled session", "run", s.RunID, "session", s.Session, "err", err)
		}
	}
}
