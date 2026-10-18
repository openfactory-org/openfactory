package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

const deadlineStopReason = "deadline"

func deadlineKey(taskID string) string { return "dispatch:deadline:" + taskID }

// checkDeadlines enforces the maximum session time. For an
// active session that is older than MaxSessionSeconds, it
// sends session.stop with the reason deadline once. If a turn is active, the
// run goes to error with infrastructure_timeout. If the session is still open
// after MaxSessionSeconds, StopGrace and LostAfter, the task is cancelled and
// the session ends as lost. Then the run changes only when a turn was active.
//
// The marker in the key value store keeps two replicas and later ticks from
// sending the stop again. A race of two replicas in the same tick can still
// send two stops. The runner ignores the second one.
func (d *Dispatcher) checkDeadlines(ctx context.Context, open []core.RunSession) error {
	now := d.now()
	limit := time.Duration(d.cfg.MaxSessionSeconds) * time.Second
	var first error
	for _, s := range open {
		if s.State != core.SessionActive {
			continue // the start timeout covers an assigned session
		}
		age := now.Sub(s.AssignedAt)
		if age <= limit {
			continue
		}
		if err := d.enforceDeadline(ctx, s, age, limit); err != nil && first == nil {
			first = fmt.Errorf("deadline of session %s: %w", s.TaskID, err)
		}
	}
	return first
}

func (d *Dispatcher) enforceDeadline(ctx context.Context, s core.RunSession, age, limit time.Duration) error {
	// The list can be old. Read the session again.
	cur, err := d.cfg.Runner.GetRunSession(ctx, s.RunID, s.Session)
	if err != nil {
		return err
	}
	if cur.State != core.SessionActive {
		return nil
	}
	_, kvErr := d.cfg.Store.KVGet(ctx, deadlineKey(cur.TaskID))
	sent := kvErr == nil
	if kvErr != nil && !errors.Is(kvErr, core.ErrNotFound) {
		return kvErr
	}
	if !sent {
		return d.sendDeadlineStop(ctx, cur)
	}
	if age <= limit+d.cfg.StopGrace+d.cfg.LostAfter {
		return nil
	}
	// The runner did not end the session in time. Stop the sandbox.
	if err := d.backendOf(cur).Cancel(ctx, cur.TaskID, 0); err != nil {
		d.log.Warn("dispatch: cancel task after deadline", "task", cur.TaskID, "err", err)
	}
	return d.loseSession(ctx, *cur, core.SandboxDeadlineExceeded, cur.ActiveTurnID != "")
}

func (d *Dispatcher) sendDeadlineStop(ctx context.Context, s *core.RunSession) error {
	c := d.getCommander()
	if c == nil {
		return fmt.Errorf("no commander, cannot send session.stop")
	}
	_, err := c.EnqueueCommand(ctx, s.RunID, s.Session, runnerproto.CommandSessionStop,
		runnerproto.SessionStopData{Reason: deadlineStopReason, Snapshot: true})
	if err != nil {
		return fmt.Errorf("enqueue session.stop: %w", err)
	}
	if err := d.cfg.Store.KVSet(ctx, deadlineKey(s.TaskID), d.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("write deadline marker: %w", err)
	}
	if s.ActiveTurnID == "" {
		return nil
	}
	_, _, err = d.transition(ctx, s.RunID, runstate.Input{Event: runstate.EventSessionDeadline}, nil)
	return err
}
