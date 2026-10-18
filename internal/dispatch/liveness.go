package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

// checkSilent applies the liveness rules of a session:
//   - A session that did not call session start before the start timeout
//     loses its task, and the run gets sandbox_create_failed.
//   - For a session that is silent for LostAfter, the control plane asks the
//     backend. "exited" gives the failure code of the end reason. "not found"
//     gives sandbox_lost.
//   - "running", or no answer, gives a second wait of LostAfter. Then the
//     control plane cancels the task and sets runner_lost.
//
// A session that waits for its runner (assigned) is only lost when its task
// exited or is gone, or at the start timeout. An image pull can take long.
func (d *Dispatcher) checkSilent(ctx context.Context, open []core.RunSession) error {
	now := d.now()
	var first error
	keep := func(s core.RunSession, err error) {
		if err != nil && first == nil {
			first = fmt.Errorf("session %s: %w", s.TaskID, err)
		}
	}
	for _, s := range open {
		assigned := s.State == core.SessionAssigned
		if assigned && now.After(s.BootstrapExpiresAt) {
			d.cancelTask(ctx, &s)
			keep(s, d.loseCode(ctx, s, core.SandboxDeadlineExceeded, core.FailureSandboxCreateFailed, true))
			continue
		}
		silentFor := now.Sub(s.LastSeenAt)
		if silentFor <= d.cfg.LostAfter {
			continue
		}
		st, err := d.backendOf(&s).Status(ctx, s.TaskID)
		if errors.Is(err, core.ErrUnavailable) {
			// The backend gives no answer, for example a worker that is not
			// connected. The runner talks to the control plane directly.
			err = nil
			st = core.TaskStatus{State: core.TaskRunning}
		}
		switch {
		case errors.Is(err, core.ErrNotFound):
			keep(s, d.loseCode(ctx, s, core.SandboxUnknown, core.FailureSandboxLost, true))
		case err == nil && st.State == core.TaskExited:
			reason := st.Reason
			if reason == "" {
				reason = core.SandboxUnknown
			}
			keep(s, d.lose(ctx, s, reason))
		case assigned:
			if err != nil {
				keep(s, fmt.Errorf("task status: %w", err))
			}
		case silentFor > 2*d.cfg.LostAfter:
			// Running, or no answer, after the second wait.
			if err != nil {
				d.log.Warn("dispatch: task status", "task", s.TaskID, "err", err)
			}
			d.cancelTask(ctx, &s)
			keep(s, d.loseCode(ctx, s, core.SandboxUnknown, core.FailureRunnerLost, true))
		case err != nil:
			keep(s, fmt.Errorf("task status: %w", err))
		}
	}
	return first
}

// cancelTask stops a task at once. Errors are logged.
func (d *Dispatcher) cancelTask(ctx context.Context, s *core.RunSession) {
	if err := d.backendOf(s).Cancel(ctx, s.TaskID, 0); err != nil && !errors.Is(err, core.ErrNotFound) {
		d.log.Warn("dispatch: cancel task", "task", s.TaskID, "err", err)
	}
}

// lose ends a session that the sandbox lost and applies the lost event.
func (d *Dispatcher) lose(ctx context.Context, s core.RunSession, reason core.SandboxReason) error {
	return d.loseSession(ctx, s, reason, true)
}

// loseSession ends a session as "lost", resets its messages and destroys its
// task. It applies the lost event to the run only when applyEvent is true.
func (d *Dispatcher) loseSession(ctx context.Context, s core.RunSession, reason core.SandboxReason, applyEvent bool) error {
	code, ok := core.FailureForSandboxReason(reason)
	if !ok {
		code = ""
	}
	return d.loseCode(ctx, s, reason, code, applyEvent)
}

// loseCode is loseSession with a failure code chosen by the caller. An empty
// code applies no run event.
func (d *Dispatcher) loseCode(ctx context.Context, s core.RunSession, reason core.SandboxReason, code core.FailureCode, applyEvent bool) error {
	_, applied, err := d.endSession(ctx, s.RunID, s.Session, "lost", reason)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if !applied {
		return nil // another replica did it
	}
	var first error
	if _, err := d.cfg.Runner.ResetRunInputsOfSession(ctx, s.RunID, s.Session); err != nil {
		first = fmt.Errorf("reset messages: %w", err)
	}
	if code != "" && applyEvent {
		in := runstate.Input{
			Event:        runstate.EventSessionLost,
			FailureCode:  code,
			PriorRetries: d.priorRetries(ctx, s.RunID, s.Session),
		}
		if _, _, err := d.transition(ctx, s.RunID, in, nil); err != nil && first == nil {
			first = fmt.Errorf("lost event: %w", err)
		}
	}
	d.destroy(ctx, &s)
	return first
}
