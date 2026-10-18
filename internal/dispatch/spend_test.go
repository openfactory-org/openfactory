package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/spend"
)

func (e *env) withSpendLimit(lim core.SpendLimit) {
	e.d.cfg.Spend = spend.New(spend.Options{
		Store: e.mem,
		Now:   func() time.Time { return *e.clock },
		FactoryLimit: func(_ context.Context, id string) (*core.SpendLimit, error) {
			if id == "fac" {
				return &lim, nil
			}
			return nil, nil
		},
	})
}

func (e *env) spentRun(cost float64) *core.Run {
	e.t.Helper()
	r := &core.Run{TeamID: "team", FactoryID: "fac", State: core.RunSucceeded, CreatedAt: *e.clock, Usage: core.Usage{CostUSD: cost}}
	if err := e.mem.CreateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// TestSpendLimitHoldsQueuedRun: a factory with a low limit cannot start a new
// run after the limit is used. The run waits in queued with the reason
// spend_limit, without the queue timeout, and starts in the next period.
func TestSpendLimitHoldsQueuedRun(t *testing.T) {
	e := newEnv(t)
	if err := e.mem.CreateUser(e.ctx, &core.User{ID: "u_admin", TeamID: "team", Email: "a@example.com", Role: core.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	e.withSpendLimit(core.SpendLimit{DailyUSD: 1})
	e.spentRun(1.25)
	run := e.newRun(core.RunQueued, "p")
	other := e.newRunIn("other") // a factory without a limit is not held
	e.tick()
	got := e.run(run.ID)
	if got.State != core.RunQueued || got.QueuedReason != core.QueuedSpendLimit {
		t.Fatalf("run = %s reason %q", got.State, got.QueuedReason)
	}
	if e.hasSession(run.ID) {
		t.Fatal("a run over the limit must get no session")
	}
	if e.run(other.ID).State != core.RunPending {
		t.Fatalf("other factory = %s", e.run(other.ID).State)
	}
	found := false
	for _, ev := range e.timelineOf(run.ID) {
		if ev.Type == "run.queued" {
			var data map[string]any
			_ = json.Unmarshal(ev.Data, &data)
			if data["reason"] == core.QueuedSpendLimit {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no run.queued event with the reason spend_limit")
	}
	items, _ := e.mem.ListInboxItems(e.ctx, "fac", true)
	if len(items) != 1 || items[0].Kind != core.InboxSpendLimit || items[0].AssigneeID != "u_admin" {
		t.Fatalf("admin inbox = %+v", items)
	}

	// The queue timeout does not apply while the run waits for the limit.
	e.advance(2 * time.Hour)
	e.tick()
	if got := e.run(run.ID); got.State != core.RunQueued || got.QueuedReason != core.QueuedSpendLimit {
		t.Fatalf("after 2h: %s %q %s", got.State, got.QueuedReason, got.FailureCode)
	}

	// The next UTC day the run starts.
	e.advance(12 * time.Hour)
	e.tick()
	got = e.run(run.ID)
	if got.State != core.RunPending || got.QueuedReason != "" {
		t.Fatalf("next day: %s %q", got.State, got.QueuedReason)
	}
}

// TestSpendLimitStopsTreeAtTurnEnd: a turn end over the limit stops the run
// tree with error and spend_limit_exceeded.
func TestSpendLimitStopsTreeAtTurnEnd(t *testing.T) {
	e := newEnv(t)
	e.withSpendLimit(core.SpendLimit{MonthlyUSD: 5})
	root := e.activeSession(core.RunRunning, "turn_1")
	child := &core.Run{TeamID: "team", FactoryID: "fac", ParentRunID: root.ID, State: core.RunQueued, CreatedAt: *e.clock}
	if err := e.mem.CreateRun(e.ctx, child); err != nil {
		t.Fatal(err)
	}
	s := e.session(root.ID, 1)
	e.events(s,
		event(t, 1, runnerproto.EventUsage, "turn_1", runnerproto.UsageData{Cumulative: true, CostUSD: 6}),
		event(t, 2, runnerproto.EventTurnEnded, "turn_1", runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone}),
	)
	for _, id := range []string{root.ID, child.ID} {
		got := e.run(id)
		if got.State != core.RunError || got.FailureCode != core.FailureSpendLimitExceeded {
			t.Fatalf("run %s = %s %s", id, got.State, got.FailureCode)
		}
		if got.Error == "" || got.Error == string(core.FailureSpendLimitExceeded) {
			t.Fatalf("run %s has no message: %q", id, got.Error)
		}
	}
	cancelled := false
	for _, c := range e.commands(root.ID, 1) {
		if c.Type == string(runnerproto.CommandSessionCancel) {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("the live session of the root run must be stopped")
	}
}

func TestNoSpendLimitNoChange(t *testing.T) {
	e := newEnv(t)
	e.withSpendLimit(core.SpendLimit{})
	e.spentRun(1000)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	if got := e.run(run.ID); got.State != core.RunPending {
		t.Fatalf("run = %s", got.State)
	}
}
