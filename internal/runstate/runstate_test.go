package runstate

import (
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

var allStates = []core.RunState{
	core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning,
	core.RunBlocked, core.RunSucceeded, core.RunFailed, core.RunError, core.RunCancelled,
}

var allEvents = []Event{
	EventAssigned, EventConfigFault, EventQueueTimeout, EventSessionStarted,
	EventSetupFailed, EventTurnStarted, EventTurnEnded, EventSessionLost,
	EventSessionDeadline, EventMessage, EventCancel, EventSpendLimit,
}

// want is the expected outcome of one row.
type want struct {
	state   core.RunState
	changed bool
	blocked string
	code    core.FailureCode
	retry   bool
	err     bool
}

type row struct {
	name string
	cur  core.RunState
	in   Input
	want want
}

var (
	invalidW = want{err: true}
)

func nc(s core.RunState) want { return want{state: s} }
func to(s core.RunState) want { return want{state: s, changed: true} }
func toCode(s core.RunState, c core.FailureCode) want {
	return want{state: s, changed: true, code: c}
}

// defaultInputs gives the input of the 99 base rows.
func defaultInput(ev Event) Input {
	switch ev {
	case EventTurnEnded:
		return Input{Event: ev, Outcome: runnerproto.OutcomeDone}
	case EventSessionLost:
		return Input{Event: ev, FailureCode: core.FailureSandboxLost}
	}
	return Input{Event: ev}
}

// baseWant is the expected result for each (event, state) pair, written from
// the run state rules. A state that is not in the map for an event is Invalid.
func baseWant(ev Event, s core.RunState) want {
	set := func(m map[core.RunState]want) want {
		if w, ok := m[s]; ok {
			return w
		}
		return invalidW
	}
	all := func(w func(core.RunState) want) want { return w(s) }
	switch ev {
	case EventAssigned:
		return set(map[core.RunState]want{core.RunQueued: to(core.RunPending)})
	case EventConfigFault:
		return set(map[core.RunState]want{core.RunQueued: toCode(core.RunFailed, core.FailureInvalidConfiguration)})
	case EventQueueTimeout:
		if s == core.RunQueued {
			return toCode(core.RunError, core.FailureInfrastructureTimeout)
		}
		return all(nc)
	case EventSessionStarted:
		return set(map[core.RunState]want{core.RunPending: to(core.RunClaimed)})
	case EventSetupFailed:
		return set(map[core.RunState]want{core.RunClaimed: toCode(core.RunFailed, core.FailureEnvironmentSetupFailed)})
	case EventTurnStarted:
		return set(map[core.RunState]want{
			core.RunClaimed:   to(core.RunRunning),
			core.RunSucceeded: to(core.RunRunning),
			core.RunBlocked:   to(core.RunRunning),
			core.RunFailed:    to(core.RunRunning),
			core.RunRunning:   nc(core.RunRunning),
		})
	case EventTurnEnded:
		return set(map[core.RunState]want{core.RunRunning: to(core.RunSucceeded)})
	case EventSessionLost:
		switch s {
		case core.RunPending, core.RunClaimed, core.RunRunning:
			return want{state: core.RunQueued, changed: true, code: core.FailureSandboxLost, retry: true}
		}
		return all(nc)
	case EventSessionDeadline:
		if s == core.RunRunning {
			return toCode(core.RunError, core.FailureInfrastructureTimeout)
		}
		return all(nc)
	case EventMessage:
		switch s {
		case core.RunSucceeded, core.RunBlocked, core.RunFailed, core.RunError, core.RunCancelled:
			return to(core.RunQueued)
		}
		return all(nc)
	case EventCancel:
		switch s {
		case core.RunQueued, core.RunPending, core.RunClaimed, core.RunRunning, core.RunBlocked:
			return to(core.RunCancelled)
		}
		return all(nc)
	case EventSpendLimit:
		switch s {
		case core.RunError, core.RunCancelled:
			return all(nc)
		}
		return toCode(core.RunError, core.FailureSpendLimitExceeded)
	}
	return invalidW
}

func tableRows() []row {
	var rows []row
	for _, ev := range allEvents {
		for _, s := range allStates {
			rows = append(rows, row{
				name: string(ev) + "/" + string(s),
				cur:  s,
				in:   defaultInput(ev),
				want: baseWant(ev, s),
			})
		}
	}

	add := func(name string, cur core.RunState, in Input, w want) {
		rows = append(rows, row{name: name, cur: cur, in: in, want: w})
	}
	te := func(o runnerproto.TurnOutcome, open bool, code core.FailureCode) Input {
		return Input{Event: EventTurnEnded, Outcome: o, HasOpenRequest: open, FailureCode: code}
	}
	blocked := func(reason string) want {
		return want{state: core.RunBlocked, changed: true, blocked: reason}
	}
	R := core.RunRunning

	add("turn_ended/done no request", R, te(runnerproto.OutcomeDone, false, ""), to(core.RunSucceeded))
	add("turn_ended/done with request", R, te(runnerproto.OutcomeDone, true, ""), blocked(BlockedHumanRequest))
	add("turn_ended/interrupted no request", R, te(runnerproto.OutcomeInterrupted, false, ""), to(core.RunSucceeded))
	add("turn_ended/interrupted with request", R, te(runnerproto.OutcomeInterrupted, true, ""), blocked(BlockedHumanRequest))
	add("turn_ended/needs_input", R, te(runnerproto.OutcomeNeedsInput, false, ""), blocked(BlockedHarnessNeedsInput))
	add("turn_ended/error agent_failed", R, te(runnerproto.OutcomeError, false, core.FailureAgentFailed), toCode(core.RunFailed, core.FailureAgentFailed))
	add("turn_ended/error harness_auth_failed", R, te(runnerproto.OutcomeError, false, core.FailureHarnessAuthFailed), toCode(core.RunFailed, core.FailureHarnessAuthFailed))
	add("turn_ended/error sandbox_lost", R, te(runnerproto.OutcomeError, false, core.FailureSandboxLost), toCode(core.RunFailed, core.FailureAgentFailed))
	add("turn_ended/error empty code", R, te(runnerproto.OutcomeError, false, ""), toCode(core.RunFailed, core.FailureAgentFailed))
	add("turn_ended/unknown outcome", R, te("weird", false, ""), invalidW)

	sl := func(code core.FailureCode, prior int) Input {
		return Input{Event: EventSessionLost, FailureCode: code, PriorRetries: prior}
	}
	retry := func(code core.FailureCode) want {
		return want{state: core.RunQueued, changed: true, code: code, retry: true}
	}
	add("session_lost/running sandbox_lost prior 0", R, sl(core.FailureSandboxLost, 0), retry(core.FailureSandboxLost))
	add("session_lost/running sandbox_lost prior 1", R, sl(core.FailureSandboxLost, 1), retry(core.FailureSandboxLost))
	add("session_lost/running sandbox_lost prior 2", R, sl(core.FailureSandboxLost, 2), toCode(core.RunError, core.FailureSandboxLost))
	add("session_lost/running sandbox_lost prior 5", R, sl(core.FailureSandboxLost, 5), toCode(core.RunError, core.FailureSandboxLost))
	add("session_lost/running agent_process_failed prior 0", R, sl(core.FailureAgentProcessFailed, 0), toCode(core.RunError, core.FailureAgentProcessFailed))
	add("session_lost/pending sandbox_create_failed prior 0", core.RunPending, sl(core.FailureSandboxCreateFailed, 0), retry(core.FailureSandboxCreateFailed))
	add("session_lost/claimed out_of_memory", core.RunClaimed, sl(core.FailureOutOfMemory, 0), toCode(core.RunFailed, core.FailureOutOfMemory))
	add("session_lost/running unknown code", R, sl("nope", 0), toCode(core.RunError, core.FailureInternalError))

	cf := func(code core.FailureCode) Input { return Input{Event: EventConfigFault, FailureCode: code} }
	add("config_fault/integration_disabled", core.RunQueued, cf(core.FailureIntegrationDisabled), toCode(core.RunFailed, core.FailureIntegrationDisabled))
	add("config_fault/sandbox_lost platform", core.RunQueued, cf(core.FailureSandboxLost), toCode(core.RunFailed, core.FailureInvalidConfiguration))
	add("config_fault/empty code", core.RunQueued, cf(""), toCode(core.RunFailed, core.FailureInvalidConfiguration))

	sf := func(code core.FailureCode) Input { return Input{Event: EventSetupFailed, FailureCode: code} }
	add("setup_failed/harness_auth_failed", core.RunClaimed, sf(core.FailureHarnessAuthFailed), toCode(core.RunFailed, core.FailureHarnessAuthFailed))
	add("setup_failed/agent_failed", core.RunClaimed, sf(core.FailureAgentFailed), toCode(core.RunFailed, core.FailureEnvironmentSetupFailed))

	for _, s := range []core.RunState{core.RunSucceeded, core.RunBlocked, core.RunFailed, core.RunError, core.RunCancelled} {
		add("message/"+string(s)+" sandbox running", s, Input{Event: EventMessage, SandboxRunning: true}, nc(s))
		add("message/"+string(s)+" sandbox gone", s, Input{Event: EventMessage, SandboxRunning: false}, to(core.RunQueued))
	}
	return rows
}

func TestNextTable(t *testing.T) {
	rows := tableRows()
	if len(rows) < 99 {
		t.Fatalf("table has %d rows, want at least 99", len(rows))
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, err := Next(r.cur, r.in)
			if r.want.err {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v, want ErrInvalidTransition", err)
				}
				if got != (Result{}) {
					t.Fatalf("result = %+v, want zero", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			exp := Result{State: r.want.state, Changed: r.want.changed, BlockedReason: r.want.blocked, FailureCode: r.want.code, Retry: r.want.retry}
			if got != exp {
				t.Fatalf("got %+v, want %+v", got, exp)
			}
		})
	}
}

func TestUnknownState(t *testing.T) {
	for _, s := range []core.RunState{core.RunStarting, core.RunIdle, "nope"} {
		for _, ev := range allEvents {
			_, err := Next(s, defaultInput(ev))
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("state %q event %q: err = %v, want ErrInvalidTransition", s, ev, err)
			}
		}
	}
}

func TestUnknownEvent(t *testing.T) {
	for _, s := range allStates {
		_, err := Next(s, Input{Event: "nope"})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("state %q: err = %v, want ErrInvalidTransition", s, err)
		}
	}
}

func TestFinishedAndKnown(t *testing.T) {
	finished := map[core.RunState]bool{
		core.RunSucceeded: true, core.RunFailed: true, core.RunError: true, core.RunCancelled: true,
	}
	for _, s := range allStates {
		if got := Finished(s); got != finished[s] {
			t.Errorf("Finished(%q) = %v, want %v", s, got, finished[s])
		}
		if !Known(s) {
			t.Errorf("Known(%q) = false, want true", s)
		}
	}
	for _, s := range []core.RunState{core.RunStarting, core.RunIdle, "nope", ""} {
		if Known(s) {
			t.Errorf("Known(%q) = true, want false", s)
		}
		if Finished(s) {
			t.Errorf("Finished(%q) = true, want false", s)
		}
	}
}

func TestResultFieldsAreClean(t *testing.T) {
	for _, s := range allStates {
		for _, ev := range allEvents {
			got, err := Next(s, defaultInput(ev))
			if err != nil {
				continue
			}
			if got.BlockedReason != "" && (got.State != core.RunBlocked || !got.Changed) {
				t.Errorf("%s/%s: BlockedReason %q set in %+v", s, ev, got.BlockedReason, got)
			}
			if got.FailureCode != "" && got.State != core.RunFailed && got.State != core.RunError && !got.Retry {
				t.Errorf("%s/%s: FailureCode %q set in %+v", s, ev, got.FailureCode, got)
			}
		}
	}
}

func TestWalk(t *testing.T) {
	steps := []struct {
		in   Input
		want core.RunState
	}{
		{Input{Event: EventAssigned}, core.RunPending},
		{Input{Event: EventSessionStarted}, core.RunClaimed},
		{Input{Event: EventTurnStarted}, core.RunRunning},
		{Input{Event: EventTurnEnded, Outcome: runnerproto.OutcomeDone}, core.RunSucceeded},
		{Input{Event: EventMessage}, core.RunQueued},
		{Input{Event: EventAssigned}, core.RunPending},
		{Input{Event: EventSessionStarted}, core.RunClaimed},
		{Input{Event: EventTurnStarted}, core.RunRunning},
		{Input{Event: EventTurnEnded, Outcome: runnerproto.OutcomeDone, HasOpenRequest: true}, core.RunBlocked},
		{Input{Event: EventCancel}, core.RunCancelled},
		{Input{Event: EventMessage}, core.RunQueued},
	}
	cur := core.RunQueued
	for i, st := range steps {
		got, err := Next(cur, st.in)
		if err != nil {
			t.Fatalf("step %d (%s): %v", i, st.in.Event, err)
		}
		if got.State != st.want || !got.Changed {
			t.Fatalf("step %d (%s): got %+v, want state %q changed", i, st.in.Event, got, st.want)
		}
		cur = got.State
	}
}
