package runstate

import (
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// Event is something that happens to a run.
type Event string

const (
	// EventAssigned means a replica gave the task to a backend.
	EventAssigned Event = "assigned"
	// EventConfigFault means the control plane found a configuration fault before the assignment.
	EventConfigFault Event = "config_fault"
	// EventQueueTimeout means the queue timeout passed.
	EventQueueTimeout Event = "queue_timeout"
	// EventSessionStarted means the runner called session start.
	EventSessionStarted Event = "session_started"
	// EventSetupFailed means the setup of the runner failed.
	EventSetupFailed Event = "setup_failed"
	// EventTurnStarted means the runner reported the start of a turn.
	EventTurnStarted Event = "turn_started"
	// EventTurnEnded means the runner reported the end of a turn.
	EventTurnEnded Event = "turn_ended"
	// EventSessionLost means the sandbox or the runner is gone without a session end.
	EventSessionLost Event = "session_lost"
	// EventSessionDeadline means the maximum session time passed.
	EventSessionDeadline Event = "session_deadline"
	// EventMessage means a message arrived in the queue of the run.
	EventMessage Event = "message"
	// EventCancel means a cancel request.
	EventCancel Event = "cancel"
	// EventSpendLimit means a turn end found the spend over its limit. The
	// control plane stops the run tree with spend_limit_exceeded.
	EventSpendLimit Event = "spend_limit"
)

// Blocked reasons.
const (
	// BlockedHumanRequest means the run has an open request to a person.
	BlockedHumanRequest = "human_request"
	// BlockedHarnessNeedsInput means the harness stopped and asked for input.
	BlockedHarnessNeedsInput = "harness_needs_input"
)

// Input is one event with the facts that the decision needs.
type Input struct {
	// Event is the event.
	Event Event
	// Outcome is the outcome of the turn, for EventTurnEnded.
	Outcome runnerproto.TurnOutcome
	// FailureCode is the failure code, for EventConfigFault, EventSetupFailed,
	// EventSessionLost, and EventTurnEnded with the outcome error.
	FailureCode core.FailureCode
	// HasOpenRequest tells that the run has an open request to a person, for EventTurnEnded.
	HasOpenRequest bool
	// SandboxRunning tells that a session of the run is alive, for EventMessage.
	SandboxRunning bool
	// PriorRetries is the number of sessions directly before this one that
	// ended with a retry, for EventSessionLost.
	PriorRetries int
}

// Result is the decision.
type Result struct {
	// State is the state after the event.
	State core.RunState
	// Changed is false when the run stays in the same state and nothing is written.
	Changed bool
	// BlockedReason is set when State is blocked.
	BlockedReason string
	// FailureCode is set when State is failed or error, and for a retry.
	FailureCode core.FailureCode
	// Retry is true when the run goes to queued for a new session after a lost session.
	Retry bool
}

// ErrInvalidTransition means that the event cannot happen in the state.
var ErrInvalidTransition = errors.New("runstate: invalid transition")

// Known reports whether s is one of the nine states of a run.
func Known(s core.RunState) bool {
	switch s {
	case core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning,
		core.RunBlocked, core.RunSucceeded, core.RunFailed, core.RunError,
		core.RunCancelled:
		return true
	}
	return false
}

// Finished reports whether s is a finished state.
func Finished(s core.RunState) bool {
	switch s {
	case core.RunSucceeded, core.RunFailed, core.RunError, core.RunCancelled:
		return true
	}
	return false
}

func invalid(cur core.RunState, ev Event) (Result, error) {
	return Result{}, fmt.Errorf("%w: event %q in state %q", ErrInvalidTransition, ev, cur)
}

func stay(cur core.RunState) (Result, error) {
	return Result{State: cur}, nil
}

func move(s core.RunState) (Result, error) {
	return Result{State: s, Changed: true}, nil
}

func moveFail(s core.RunState, code core.FailureCode) (Result, error) {
	return Result{State: s, Changed: true, FailureCode: code}, nil
}

// Next returns the result of the event in the state cur.
func Next(cur core.RunState, in Input) (Result, error) {
	ev := in.Event
	if !Known(cur) {
		return invalid(cur, ev)
	}
	switch ev {
	case EventAssigned:
		if cur == core.RunQueued {
			return move(core.RunPending)
		}
		return invalid(cur, ev)

	case EventConfigFault:
		if cur != core.RunQueued {
			return invalid(cur, ev)
		}
		code := core.FailureInvalidConfiguration
		if info, ok := core.LookupFailure(in.FailureCode); ok && info.Class == core.FailureClassUser {
			code = in.FailureCode
		}
		return moveFail(core.RunFailed, code)

	case EventQueueTimeout:
		if cur == core.RunQueued {
			return moveFail(core.RunError, core.FailureInfrastructureTimeout)
		}
		return stay(cur)

	case EventSessionStarted:
		if cur == core.RunPending {
			return move(core.RunClaimed)
		}
		return invalid(cur, ev)

	case EventSetupFailed:
		if cur != core.RunClaimed {
			return invalid(cur, ev)
		}
		code := core.FailureEnvironmentSetupFailed
		switch in.FailureCode {
		case core.FailureEnvironmentSetupFailed, core.FailureHarnessAuthFailed, core.FailureInvalidConfiguration:
			code = in.FailureCode
		}
		return moveFail(core.RunFailed, code)

	case EventTurnStarted:
		switch cur {
		case core.RunClaimed, core.RunSucceeded, core.RunBlocked, core.RunFailed:
			return move(core.RunRunning)
		case core.RunRunning:
			return stay(cur)
		}
		return invalid(cur, ev)

	case EventTurnEnded:
		if cur != core.RunRunning {
			return invalid(cur, ev)
		}
		switch in.Outcome {
		case runnerproto.OutcomeDone, runnerproto.OutcomeInterrupted:
			if in.HasOpenRequest {
				return Result{State: core.RunBlocked, Changed: true, BlockedReason: BlockedHumanRequest}, nil
			}
			return move(core.RunSucceeded)
		case runnerproto.OutcomeNeedsInput:
			return Result{State: core.RunBlocked, Changed: true, BlockedReason: BlockedHarnessNeedsInput}, nil
		case runnerproto.OutcomeError:
			return moveFail(core.RunFailed, core.RunnerFailureCode(string(in.FailureCode)))
		}
		return invalid(cur, ev)

	case EventSessionLost:
		switch cur {
		case core.RunPending, core.RunClaimed, core.RunRunning:
			code := in.FailureCode
			info, ok := core.LookupFailure(code)
			if !ok {
				code = core.FailureInternalError
				info, _ = core.LookupFailure(code)
			}
			if info.Retry && in.PriorRetries < core.MaxRetrySessions-1 {
				return Result{State: core.RunQueued, Changed: true, FailureCode: code, Retry: true}, nil
			}
			if info.Class == core.FailureClassUser {
				return moveFail(core.RunFailed, code)
			}
			return moveFail(core.RunError, code)
		}
		return stay(cur)

	case EventSessionDeadline:
		if cur == core.RunRunning {
			return moveFail(core.RunError, core.FailureInfrastructureTimeout)
		}
		return stay(cur)

	case EventMessage:
		switch cur {
		case core.RunSucceeded, core.RunBlocked, core.RunFailed, core.RunError, core.RunCancelled:
			if !in.SandboxRunning {
				return move(core.RunQueued)
			}
		}
		return stay(cur)

	case EventSpendLimit:
		switch cur {
		case core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning,
			core.RunBlocked, core.RunSucceeded, core.RunFailed:
			return moveFail(core.RunError, core.FailureSpendLimitExceeded)
		}
		return stay(cur)

	case EventCancel:
		switch cur {
		case core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning, core.RunBlocked:
			return move(core.RunCancelled)
		}
		return stay(cur)
	}
	return invalid(cur, ev)
}
