package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// childRun creates a parent and a child run and returns the child.
func childRun(t *testing.T, eng *Engine, st core.Store) (parent, child *core.Run) {
	t.Helper()
	ctx := context.Background()
	p, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "p"}, "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "c"}, "child", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = st.GetRun(ctx, c.ID)
	c.ParentRunID = p.ID
	c.Name = "implement"
	c.AgentName = "coder"
	if err := st.UpdateRun(ctx, c); err != nil {
		t.Fatal(err)
	}
	return p, c
}

func setChild(t *testing.T, st core.Store, c *core.Run, f func(r *core.Run)) *core.Run {
	t.Helper()
	r, err := st.GetRun(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	f(r)
	if err := st.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func lifecycleSent(route *fakeRoute) []core.RunInput {
	route.mu.Lock()
	defer route.mu.Unlock()
	var out []core.RunInput
	for _, in := range route.sent {
		if in.Kind == "lifecycle" {
			out = append(out, in)
		}
	}
	return out
}

func decodeLifecycle(t *testing.T, in core.RunInput) lifecycleData {
	t.Helper()
	var d lifecycleData
	if err := json.Unmarshal(in.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLifecycleSucceededOnce(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	p, c := childRun(t, eng, st)
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1; r.Result = "done" })
	eng.onRunUpdated(ctx, r)
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 1 {
		t.Fatalf("sent %d lifecycle messages, want 1", len(got))
	}
	in := got[0]
	d := decodeLifecycle(t, in)
	if in.RunID != p.ID || d.Event != "succeeded" || d.RunID != c.ID || d.Name != "implement" || d.Agent != "coder" || d.Result != "done" {
		t.Errorf("input %+v data %+v", in, d)
	}
	if in.DedupeKey != "lifecycle:"+c.ID+":succeeded:1" {
		t.Errorf("dedupe = %q", in.DedupeKey)
	}
	if in.Content != `Child run "implement" succeeded.` {
		t.Errorf("content = %q", in.Content)
	}
	if in.Sender.Type != "run" || in.Sender.ID != c.ID {
		t.Errorf("sender = %+v", in.Sender)
	}
}

func TestLifecycleBlockedAndError(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, c := childRun(t, eng, st)
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunBlocked; r.Turns = 1 })
	eng.onRunUpdated(ctx, r)
	r = setChild(t, st, c, func(r *core.Run) {
		r.State = core.RunError
		r.Turns = 2
		r.FailureCode = core.FailureSandboxLost
	})
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 2 {
		t.Fatalf("sent %d, want 2", len(got))
	}
	if d := decodeLifecycle(t, got[0]); d.Event != "blocked" {
		t.Errorf("event = %q", d.Event)
	}
	d := decodeLifecycle(t, got[1])
	if d.Event != "error" || d.FailureCode != "sandbox_lost" {
		t.Errorf("data = %+v", d)
	}
	if !strings.Contains(got[1].Content, "Failure code: sandbox_lost.") {
		t.Errorf("content = %q", got[1].Content)
	}
}

func TestLifecycleTruncatesResult(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, c := childRun(t, eng, st)
	long := strings.Repeat("é", 4000) // 8000 bytes
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1; r.Result = long })
	eng.onRunUpdated(ctx, r)
	d := decodeLifecycle(t, lifecycleSent(route)[0])
	if !d.ResultTruncated || len(d.Result) > lifecycleResultMax || len(d.Result) < lifecycleResultMax-3 {
		t.Errorf("truncated=%v len=%d", d.ResultTruncated, len(d.Result))
	}
	if strings.ContainsRune(d.Result, '�') {
		t.Error("result was cut inside a rune")
	}
}

func TestLifecycleCancelled(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	p, c := childRun(t, eng, st)
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunCancelled; r.CancelledBy = "run:" + p.ID })
	eng.onRunUpdated(ctx, r)
	if n := len(lifecycleSent(route)); n != 0 {
		t.Fatalf("sent %d, want 0 when the parent cancelled", n)
	}

	_, c2 := childRun(t, eng, st)
	r = setChild(t, st, c2, func(r *core.Run) { r.State = core.RunCancelled; r.CancelledBy = "user:u1" })
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 1 || decodeLifecycle(t, got[0]).Event != "cancelled" {
		t.Fatalf("sent = %+v", got)
	}
}

func TestLifecycleStarted(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, c := childRun(t, eng, st)
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunRunning })
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 1 {
		t.Fatalf("sent %d, want 1", len(got))
	}
	if got[0].DedupeKey != "lifecycle:"+c.ID+":started" || decodeLifecycle(t, got[0]).Event != "started" {
		t.Errorf("input = %+v", got[0])
	}
}

func TestFollowUpCancelledRun(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	setChild(t, st, run, func(r *core.Run) { r.State = core.RunCancelled })
	if err := eng.FollowUp(ctx, run.ID, "again", "user_1"); err != nil {
		t.Fatalf("runner path: %v", err)
	}
	if len(route.sent) != 1 {
		t.Errorf("sent = %d, want 1", len(route.sent))
	}
}

// sideRun makes a work item with a root run and one side run, as an automation
// of a different agent starts it, and returns the work item and both runs.
func sideRun(t *testing.T, eng *Engine, st core.Store) (wi *core.WorkItem, root, side *core.Run) {
	t.Helper()
	ctx := context.Background()
	root, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "foreman"}, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	side, err = startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "reviewer"}, "side", nil)
	if err != nil {
		t.Fatal(err)
	}
	wi = &core.WorkItem{ID: "wi_1", FactoryID: "f1", Title: "x", Stage: core.StageBuilding, RootRunID: root.ID}
	if err := st.CreateWorkItem(ctx, wi); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*core.Run{root, side} {
		cur := setChild(t, st, r, func(r *core.Run) { r.WorkItemID = wi.ID; r.FactoryID = "f1" })
		*r = *cur
	}
	setChild(t, st, side, func(r *core.Run) { r.Name = "review"; r.AgentName = "reviewer" })
	return wi, root, side
}

func TestLifecycleSideRunGoesToRootRun(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, root, side := sideRun(t, eng, st)

	r := setChild(t, st, side, func(r *core.Run) { r.State = core.RunRunning })
	eng.onRunUpdated(ctx, r)
	r = setChild(t, st, side, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1; r.Result = "looks good" })
	eng.onRunUpdated(ctx, r)
	eng.onRunUpdated(ctx, r) // the same turn end is reported once

	got := lifecycleSent(route)
	if len(got) != 2 {
		t.Fatalf("sent %d lifecycle messages, want 2 (started, succeeded): %+v", len(got), got)
	}
	for _, in := range got {
		if in.RunID != root.ID || in.Sender.Type != "run" || in.Sender.ID != side.ID {
			t.Errorf("input %+v", in)
		}
	}
	if d := decodeLifecycle(t, got[0]); d.Event != "started" || d.RunID != side.ID || d.Agent != "reviewer" {
		t.Errorf("started = %+v", d)
	}
	d := decodeLifecycle(t, got[1])
	if d.Event != "succeeded" || d.Result != "looks good" || d.Name != "review" {
		t.Errorf("succeeded = %+v", d)
	}
	if got[1].Content != `Side run "review" succeeded.` || got[1].DedupeKey != "lifecycle:"+side.ID+":succeeded:1" {
		t.Errorf("content %q dedupe %q", got[1].Content, got[1].DedupeKey)
	}
}

func TestLifecycleSideRunFailureAndCancel(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, root, side := sideRun(t, eng, st)

	r := setChild(t, st, side, func(r *core.Run) {
		r.State = core.RunError
		r.Turns = 1
		r.FailureCode = core.FailureSandboxLost
	})
	eng.onRunUpdated(ctx, r)
	// A person cancels the side run alone: the root run learns about it.
	r = setChild(t, st, side, func(r *core.Run) { r.State = core.RunCancelled; r.CancelledBy = "user:u1" })
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 2 || decodeLifecycle(t, got[0]).Event != "error" || decodeLifecycle(t, got[1]).Event != "cancelled" {
		t.Fatalf("sent = %+v", got)
	}
	if got[0].RunID != root.ID || decodeLifecycle(t, got[0]).FailureCode != "sandbox_lost" {
		t.Errorf("error message = %+v", got[0])
	}
}

func TestLifecycleSideRunSilentWhenNotActive(t *testing.T) {
	ctx := context.Background()

	t.Run("work item is complete", func(t *testing.T) {
		route := &fakeRoute{}
		eng, st := newRouteEngine(t, route)
		wi, _, side := sideRun(t, eng, st)
		wi, _ = st.GetWorkItem(ctx, wi.ID)
		wi.Stage = core.StageComplete
		if err := st.UpdateWorkItem(ctx, wi); err != nil {
			t.Fatal(err)
		}
		r := setChild(t, st, side, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1 })
		eng.onRunUpdated(ctx, r)
		r = setChild(t, st, side, func(r *core.Run) { r.State = core.RunRunning })
		eng.onRunUpdated(ctx, r)
		if n := len(lifecycleSent(route)); n != 0 {
			t.Fatalf("sent %d, want 0 after the work item completed", n)
		}
	})
	t.Run("root run is cancelled", func(t *testing.T) {
		route := &fakeRoute{}
		eng, st := newRouteEngine(t, route)
		_, root, side := sideRun(t, eng, st)
		setChild(t, st, root, func(r *core.Run) { r.State = core.RunCancelled })
		r := setChild(t, st, side, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1 })
		eng.onRunUpdated(ctx, r)
		if n := len(lifecycleSent(route)); n != 0 {
			t.Fatalf("sent %d, want 0 to a cancelled root run", n)
		}
	})
	t.Run("the root run itself and runs without a work item", func(t *testing.T) {
		route := &fakeRoute{}
		eng, st := newRouteEngine(t, route)
		_, root, _ := sideRun(t, eng, st)
		r := setChild(t, st, root, func(r *core.Run) { r.State = core.RunRunning })
		eng.onRunUpdated(ctx, r)
		r = setChild(t, st, root, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1 })
		eng.onRunUpdated(ctx, r)
		lone, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "lone"}, "alone", nil)
		if err != nil {
			t.Fatal(err)
		}
		r = setChild(t, st, lone, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1 })
		eng.onRunUpdated(ctx, r)
		if n := len(lifecycleSent(route)); n != 0 {
			t.Fatalf("sent %d, want 0", n)
		}
	})
	t.Run("no runner route", func(t *testing.T) {
		eng, st := newRouteEngine(t, nil)
		_, _, side := sideRun(t, eng, st)
		r := setChild(t, st, side, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1 })
		eng.onRunUpdated(ctx, r) // must not panic, and nothing is sent
	})
}
