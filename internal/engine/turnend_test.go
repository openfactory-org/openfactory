package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/events"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func turnEndedJob(t *testing.T, runID string) core.Job {
	t.Helper()
	b, err := json.Marshal(core.RunTurnEnded{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	return core.Job{ID: "turn:" + runID, Topic: core.TopicRunTurnEnded, Payload: b, Attempt: 1}
}

// A failed send of the lifecycle message must not mark the turn end as
// reported: the durable job tries again and the parent gets one message.
func TestTurnEndRetriesFailedLifecycleSend(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{sendErr: errors.New("database is down")}
	eng, st := newRouteEngine(t, route)
	p, c := childRun(t, eng, st)
	r := setChild(t, st, c, func(r *core.Run) { r.State = core.RunSucceeded; r.Turns = 1; r.Result = "done" })

	eng.onRunUpdated(ctx, r) // the live path fails and only logs
	if err := eng.handleTurnEndedJob(ctx, turnEndedJob(t, c.ID)); err == nil {
		t.Fatal("job with a failed send: want an error, so the queue tries again")
	}
	if got, _ := st.KVGet(ctx, "engine:turn:"+c.ID); got != core.TurnMarker(r)+turnPending {
		t.Fatalf("marker after failed send = %q, want the pending marker", got)
	}
	if n := len(lifecycleSent(route)); n != 0 {
		t.Fatalf("sent %d lifecycle messages before the fix, want 0", n)
	}

	route.mu.Lock()
	route.sendErr = nil
	route.mu.Unlock()
	if err := eng.handleTurnEndedJob(ctx, turnEndedJob(t, c.ID)); err != nil {
		t.Fatalf("retried job: %v", err)
	}
	if err := eng.handleTurnEndedJob(ctx, turnEndedJob(t, c.ID)); err != nil {
		t.Fatalf("repeated job: %v", err)
	}
	eng.onRunUpdated(ctx, r)
	got := lifecycleSent(route)
	if len(got) != 1 || got[0].RunID != p.ID || decodeLifecycle(t, got[0]).Event != "succeeded" {
		t.Fatalf("lifecycle messages = %+v, want one succeeded message to the parent", got)
	}
	if m, _ := st.KVGet(ctx, "engine:turn:"+c.ID); m != core.TurnMarker(r) {
		t.Fatalf("marker = %q, want %q", m, core.TurnMarker(r))
	}
}

// A turn end that no live update announced is still reported by its job.
func TestTurnEndedJobWithoutLiveUpdate(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	p, c := childRun(t, eng, st)
	setChild(t, st, c, func(r *core.Run) { r.State = core.RunFailed; r.Turns = 1 })
	if err := eng.handleTurnEndedJob(ctx, turnEndedJob(t, c.ID)); err != nil {
		t.Fatal(err)
	}
	got := lifecycleSent(route)
	if len(got) != 1 || got[0].RunID != p.ID {
		t.Fatalf("lifecycle messages = %+v, want one to the parent", got)
	}
	// A job for a run that does not exist is acknowledged.
	if err := eng.handleTurnEndedJob(ctx, turnEndedJob(t, "run_missing")); err != nil {
		t.Fatalf("missing run: %v", err)
	}
}

// The cancel of a run enqueues its turn-end job in the cancel transaction.
func TestCancelEnqueuesTurnEndedJob(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	_, c := childRun(t, eng, st)
	if err := eng.CancelOne(ctx, c.ID, "user:alice"); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.(core.Coordinator).OutboxList(ctx, core.OutboxPending, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.Topic == core.TopicRunTurnEnded {
			var p core.RunTurnEnded
			if err := json.Unmarshal(m.Payload, &p); err != nil || p.RunID != c.ID {
				t.Fatalf("job payload %s: %v", m.Payload, err)
			}
			if m.PartitionKey != core.TopicRunTurnEnded+"|run:"+c.ID {
				t.Errorf("partition key = %q", m.PartitionKey)
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("turn-end jobs = %d, want 1", n)
	}
}

// failRootLinkStore fails the write that links the root run to its work item.
type failRootLinkStore struct {
	*sqlstore.Store
	fail bool
}

func (s *failRootLinkStore) UpdateWorkItem(ctx context.Context, w *core.WorkItem) error {
	if s.fail && w.RootRunID != "" {
		return errors.New("crash before the root run link")
	}
	return s.Store.UpdateWorkItem(ctx, w)
}

// The root run and its link to the work item commit together. Before, a
// failure between them left a run that the work item did not know, and the
// next dispatch for the same source started a second root run.
func TestDispatchLinksRootRunInRunTransaction(t *testing.T) {
	ctx := context.Background()
	st := &failRootLinkStore{Store: testdb.New(t), fail: true}
	bus := events.NewForStore(st, events.DurableOptions{ReplicaID: "r"})
	t.Cleanup(func() { _ = bus.Close() })
	eng, f := newTxEngine(t, st, bus)
	req := core.DispatchRequest{
		FactoryID: f.ID, Prompt: "Fix the invoice bug.", TriggeredBy: "api",
		Source: core.SourceRef{Provider: "github", Kind: "issue", Repo: "acme/api", Number: 7},
	}
	if _, _, err := eng.Dispatch(ctx, req); err == nil {
		t.Fatal("dispatch with a failed root link: want an error")
	}
	runs, err := st.ListRuns(ctx, core.RunFilter{FactoryID: f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs after the failed link = %d, want 0 (rolled back)", len(runs))
	}

	st.fail = false
	wi, run, err := eng.Dispatch(ctx, req)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if wi.RootRunID != run.ID {
		t.Fatalf("root run = %q, want %q", wi.RootRunID, run.ID)
	}
	runs, _ = st.ListRuns(ctx, core.RunFilter{FactoryID: f.ID})
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	stored, _ := st.GetWorkItem(ctx, wi.ID)
	if stored.RootRunID != run.ID {
		t.Fatalf("stored root run = %q, want %q", stored.RootRunID, run.ID)
	}
}
