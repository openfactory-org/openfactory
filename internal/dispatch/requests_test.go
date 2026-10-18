package dispatch

import (
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func (e *env) inbox(run *core.Run, runID string, kind core.InboxKind, resolved bool) {
	e.t.Helper()
	it := &core.InboxItem{ID: core.NewID("inb"), RunID: runID, FactoryID: run.FactoryID, Kind: kind, Title: "q", Resolved: resolved, CreatedAt: time.Now()}
	if err := e.mem.CreateInboxItem(e.ctx, it); err != nil {
		e.t.Fatal(err)
	}
}

func TestTurnEndedOpenRequest(t *testing.T) {
	tests := []struct {
		name        string
		item        func(e *env, run, other *core.Run)
		wantState   core.RunState
		wantBlocked string
	}{
		{"open question", func(e *env, run, _ *core.Run) { e.inbox(run, run.ID, core.InboxQuestion, false) }, core.RunBlocked, "human_request"},
		{"open approval", func(e *env, run, _ *core.Run) { e.inbox(run, run.ID, core.InboxApproval, false) }, core.RunBlocked, "human_request"},
		{"resolved question", func(e *env, run, _ *core.Run) { e.inbox(run, run.ID, core.InboxQuestion, true) }, core.RunSucceeded, ""},
		{"other run", func(e *env, run, other *core.Run) { e.inbox(run, other.ID, core.InboxQuestion, false) }, core.RunSucceeded, ""},
		{"other kind", func(e *env, run, _ *core.Run) { e.inbox(run, run.ID, core.InboxHandoff, false) }, core.RunSucceeded, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			run, turn := liveRun(t, e)
			other := e.newRun(core.RunSucceeded, "other")
			tc.item(e, run, other)
			s := e.session(run.ID, 1)
			e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
				event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "ok"}))
			r := e.run(run.ID)
			if r.State != tc.wantState || r.BlockedReason != tc.wantBlocked || r.Turns != 1 {
				t.Fatalf("run = %+v", r)
			}
			if tc.wantState == core.RunSucceeded && r.FinishedAt == nil {
				t.Error("FinishedAt not set")
			}
			if tc.wantState == core.RunBlocked && r.FinishedAt != nil {
				t.Error("FinishedAt set on blocked run")
			}
		})
	}
}

func TestTurnEndedAgentFailed(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeError, FailureCode: "agent_failed"}))
	r := e.run(run.ID)
	if r.State != core.RunFailed || r.FailureCode != core.FailureCode("agent_failed") || r.Error != "agent_failed" || r.FinishedAt == nil {
		t.Fatalf("run = %+v", r)
	}
}

func TestMessageStartsTurn(t *testing.T) {
	tests := []struct {
		name string
		m    core.RunInput
		want bool
	}{
		{"system", core.RunInput{Kind: "system"}, false},
		{"lifecycle started", core.RunInput{Kind: "lifecycle", Data: []byte(`{"event":"started"}`)}, false},
		{"lifecycle succeeded", core.RunInput{Kind: "lifecycle", Data: []byte(`{"event":"succeeded"}`)}, true},
		{"user", core.RunInput{Kind: "user"}, true},
		{"lifecycle invalid json", core.RunInput{Kind: "lifecycle", Data: []byte(`{nope`)}, true},
		{"lifecycle no data", core.RunInput{Kind: "lifecycle"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageStartsTurn(tc.m); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	if startsTurn([]core.RunInput{{Kind: "system"}, {Kind: "lifecycle", Data: []byte(`{"event":"started"}`)}}) {
		t.Error("startsTurn true for non-starting messages")
	}
	if !startsTurn([]core.RunInput{{Kind: "system"}, {Kind: "user"}}) {
		t.Error("startsTurn false with a user message")
	}
}

func TestSendLifecycleStartedStored(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunSucceeded, "p")
	if err := e.mem.CreateRunSession(e.ctx, &core.RunSession{RunID: run.ID, Session: 1, TaskID: core.TaskID(run.ID, 1)}); err != nil {
		t.Fatal(err)
	}
	e.endSessionRaw(run.ID, 1)
	out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "lifecycle", Data: []byte(`{"event":"started"}`)})
	if err != nil || out != OutcomeStored {
		t.Fatalf("send = %q, %v", out, err)
	}
	if got := e.run(run.ID).State; got != core.RunSucceeded {
		t.Fatalf("state = %s", got)
	}
}
