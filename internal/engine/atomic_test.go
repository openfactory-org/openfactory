package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/events"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
)

// txTestStore is a real transactional store.
type txTestStore interface {
	core.Store
	core.Transactor
	core.Coordinator
}

func txTestStores() map[string]func(t *testing.T) txTestStore {
	return map[string]func(t *testing.T) txTestStore{
		"sqlite": func(t *testing.T) txTestStore {
			s, err := sqlite.OpenStore(context.Background(), filepath.Join(t.TempDir(), "of.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
}

// newTxEngine applies the default test definition on st with bus.
func newTxEngine(t *testing.T, st core.Store, bus core.EventBus) (*Engine, *core.Factory) {
	t.Helper()
	ctx := context.Background()
	team := &core.Team{ID: "team1", Name: "team"}
	if err := st.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	sec := NewFakeSecrets()
	_, _ = sec.Put(ctx, "team1", "", "ANTHROPIC_KEY", core.SecretAnthropicKey, []byte("sk-ant-test\n"))
	f := &core.Factory{ID: core.NewID("fac"), TeamID: team.ID, Name: "payments", Alias: core.NewID("bot"), Source: core.DefinitionManaged}
	if err := st.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	eng := New(Options{Store: st, Bus: bus, Compile: CompilerFor(DefaultTestDefinition()), Match: SimpleMatch,
		Secrets: sec, Forges: FakeForges{}, Logger: DiscardLogger()})
	if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
		t.Fatalf("apply definition: %v", err)
	}
	return eng, f
}

func dispatchIssue(t *testing.T, eng *Engine, f *core.Factory) (*core.WorkItem, *core.Run) {
	t.Helper()
	wi, run, err := eng.Dispatch(context.Background(), core.DispatchRequest{
		FactoryID: f.ID, Prompt: "Fix the invoice bug.", TriggeredBy: "api",
		Source: core.SourceRef{Provider: "github", Kind: "issue", Repo: "acme/api", Number: 7},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return wi, run
}

// TestEngineFlowsOnTransactionalStores drives the engine's atomic paths on
// real transactional stores (deadlocks or lost writes would surface here).
func TestEngineFlowsOnTransactionalStores(t *testing.T) {
	for name, open := range txTestStores() {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			bus := events.NewForStore(st, events.DurableOptions{ReplicaID: "r", PollInterval: 10 * time.Millisecond})
			defer bus.Close()
			notices, unsub := bus.Subscribe(core.TopicOutboundNotice)
			defer unsub()
			eng, f := newTxEngine(t, st, bus)
			ctx := context.Background()

			wi, run := dispatchIssue(t, eng, f)
			if wi.RootRunID != run.ID {
				t.Fatalf("root run %q, want %q", wi.RootRunID, run.ID)
			}
			it, err := eng.RequestHuman(ctx, run.ID, core.InboxApproval, "Plan", "the plan", nil)
			if err != nil {
				t.Fatalf("request human: %v", err)
			}
			if n := recvNotice(t, notices); n.InboxID != it.ID || n.Kind != "approval_request" {
				t.Fatalf("notice %+v", n)
			}
			cur, _ := st.GetWorkItem(ctx, wi.ID)
			if cur.Stage != core.StageAwaitingApproval {
				t.Fatalf("stage %s", cur.Stage)
			}
			if err := eng.ResolveInbox(ctx, it.ID, "approved", "alice"); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			msgs, _ := st.ListRunMessages(ctx, run.ID, 0)
			if len(msgs) != 2 {
				t.Fatalf("run messages after follow-up: %d", len(msgs))
			}
			if err := eng.PostUpdate(ctx, run.ID, "progress", "update"); err != nil {
				t.Fatal(err)
			}
			if n := recvNotice(t, notices); n.Text != "progress" {
				t.Fatalf("notice %+v", n)
			}
			if err := eng.LinkPullRequest(ctx, wi.ID, core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 9}); err != nil {
				t.Fatal(err)
			}
			if err := eng.SetStage(ctx, wi.ID, core.StageReviewing, "alice", ""); err != nil {
				t.Fatal(err)
			}
			if err := eng.Cancel(ctx, run.ID, "alice"); err != nil {
				t.Fatal(err)
			}
			cur, _ = st.GetWorkItem(ctx, wi.ID)
			if cur.Stage != core.StageCancelled || len(cur.PullRequests) != 1 {
				t.Fatalf("work item %+v", cur)
			}
			evs, _ := st.ListWorkItemEvents(ctx, wi.ID)
			if len(evs) < 6 {
				t.Fatalf("timeline has %d entries", len(evs))
			}
		})
	}
}

func recvNotice(t *testing.T, ch <-chan any) *core.OutboundMessage {
	t.Helper()
	select {
	case v := <-ch:
		return v.(*core.OutboundMessage)
	case <-time.After(5 * time.Second):
		t.Fatal("no outbound notice")
		return nil
	}
}

// failingOutbox is a coordinator whose outbox writes fail.
type failingOutbox struct{ core.Coordinator }

var errOutboxDown = errors.New("outbox unavailable")

func (failingOutbox) OutboxEnqueue(context.Context, *core.OutboxMessage) (bool, error) {
	return false, errOutboxDown
}

// A failed outbox write rolls back the state change it announces: no inbox
// item, no timeline entry, no in-process fallback delivery.
func TestOutboxFailureRollsBackStateChange(t *testing.T) {
	for name, open := range txTestStores() {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			bus := events.NewDurable(failingOutbox{st}, st, events.DurableOptions{ReplicaID: "r", PollInterval: 10 * time.Millisecond})
			defer bus.Close()
			notices, unsub := bus.Subscribe(core.TopicOutboundNotice)
			defer unsub()
			eng, f := newTxEngine(t, st, bus)
			ctx := context.Background()
			wi, run := dispatchIssue(t, eng, f)
			before, _ := st.ListWorkItemEvents(ctx, wi.ID)

			_, err := eng.RequestHuman(ctx, run.ID, core.InboxQuestion, "Which?", "A or B?", nil)
			if !errors.Is(err, errOutboxDown) {
				t.Fatalf("err = %v, want the outbox error", err)
			}
			items, _ := st.ListInboxItems(ctx, f.ID, true)
			if len(items) != 0 {
				t.Fatalf("inbox item survived the failed outbox write: %+v", items)
			}
			after, _ := st.ListWorkItemEvents(ctx, wi.ID)
			if len(after) != len(before) {
				t.Fatalf("timeline grew from %d to %d", len(before), len(after))
			}
			select {
			case n := <-notices:
				t.Fatalf("notice delivered in process: %+v", n)
			case <-time.After(150 * time.Millisecond):
			}

			if err := eng.PostUpdate(ctx, run.ID, "progress", "update"); !errors.Is(err, errOutboxDown) {
				t.Fatalf("post update err = %v", err)
			}
			if after2, _ := st.ListWorkItemEvents(ctx, wi.ID); len(after2) != len(before) {
				t.Fatalf("timeline grew to %d", len(after2))
			}
		})
	}
}

// failingKVSet fails KVSet; everything else (InTx included) is the real
// store.
type failingKVSet struct{ txTestStore }

var errKVSet = errors.New("kv write failed")

func (failingKVSet) KVSet(context.Context, string, string) error { return errKVSet }

// A failed state change emits no event: a follow-up whose event context
// cannot be stored leaves neither the message nor its run.message event
// behind.
func TestFailedStateChangeEmitsNoEvent(t *testing.T) {
	for name, open := range txTestStores() {
		t.Run(name, func(t *testing.T) {
			real := open(t)
			bus := NewFakeBus()
			eng, f := newTxEngine(t, real, bus)
			ctx := context.Background()
			_, run := dispatchIssue(t, eng, f)
			eng.store = failingKVSet{real}
			msgsBefore := len(bus.Records(core.TopicRunMessage))
			runsBefore := len(bus.Records(core.TopicRunUpdated))

			ev := IssueEvent("issue_commented", "more", "alice")
			if err := eng.followUpEvent(ctx, run.ID, "more", "alice", ev); !errors.Is(err, errKVSet) {
				t.Fatalf("err = %v", err)
			}
			msgs, _ := real.ListRunMessages(ctx, run.ID, 0)
			if len(msgs) != 1 {
				t.Fatalf("run has %d messages; the follow-up message survived the failed write", len(msgs))
			}
			if n := len(bus.Records(core.TopicRunMessage)); n != msgsBefore {
				t.Fatalf("run.message published for a rolled-back message (%d -> %d)", msgsBefore, n)
			}
			if n := len(bus.Records(core.TopicRunUpdated)); n != runsBefore {
				t.Fatalf("run.updated published (%d -> %d)", runsBefore, n)
			}
		})
	}
}

// Without a Transactor the engine keeps its previous behavior.
func TestAtomicFallsBackWithoutTransactor(t *testing.T) {
	env := NewTestEnv(t)
	// The wrapper hides the transactions of the SQL store.
	eng := New(Options{Store: struct{ core.Store }{env.Store}, Bus: env.Bus, Logger: DiscardLogger()})
	called := false
	err := eng.Atomic(context.Background(), func(ctx context.Context) error {
		called = true
		if core.InTransaction(ctx) {
			t.Error("the store has no transactions")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
