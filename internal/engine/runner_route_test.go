package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/events"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type fakeRoute struct {
	mu           sync.Mutex
	sent         []core.RunInput // messages after the first prompt
	prompts      []core.RunInput // first prompts of new runs (key "prompt")
	outcome      string          // result of Send; default live_session
	stopped      []string
	interrupted  []string
	interruptErr error
}

func (f *fakeRoute) Interrupt(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupted = append(f.interrupted, runID)
	return f.interruptErr
}

func (f *fakeRoute) Send(_ context.Context, m *core.RunInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.DedupeKey == "prompt" {
		f.prompts = append(f.prompts, *m)
	} else {
		f.sent = append(f.sent, *m)
	}
	if f.outcome != "" {
		return f.outcome, nil
	}
	return "live_session", nil
}

// sentTo returns the messages after the first prompt that were sent to a run.
func (f *fakeRoute) sentTo(runID string) []core.RunInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.RunInput
	for _, m := range f.sent {
		if m.RunID == runID {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeRoute) StopSession(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, runID)
	return nil
}

func newRouteEngine(t *testing.T, route RunnerRoute) (*Engine, core.Store) {
	t.Helper()
	st := testdb.New(t)
	bus := events.NewForStore(st, events.DurableOptions{ReplicaID: "r"})
	t.Cleanup(func() { _ = bus.Close() })
	if err := st.CreateTeam(context.Background(), &core.Team{ID: "team1", Name: "team"}); err != nil {
		t.Fatal(err)
	}
	eng := New(Options{Store: st, Bus: bus, Logger: DiscardLogger(), Runner: route})
	return eng, st
}

func TestFollowUpUsesRunnerRoute(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Make the run idle so that a follow-up reaches an idle run.
	r, _ := st.GetRun(ctx, run.ID)
	r.State = core.RunIdle
	if err := st.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		actor, kind string
		sender      core.RunInputSender
	}{
		{"user_1", "user", core.RunInputSender{Type: "user", ID: "user_1"}},
		{"run:run_9", "agent", core.RunInputSender{Type: "run", ID: "run_9"}},
		{"system", "user", core.RunInputSender{Type: "platform"}},
	}
	for _, tc := range cases {
		if err := eng.FollowUp(ctx, run.ID, "again "+tc.actor, tc.actor); err != nil {
			t.Fatal(err)
		}
	}
	if len(route.sent) != len(cases) {
		t.Fatalf("sent %d messages, want %d", len(route.sent), len(cases))
	}
	msgs, err := st.ListRunMessages(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		in := route.sent[i]
		if in.Kind != tc.kind || in.Sender != tc.sender || in.RunID != run.ID || in.Content != "again "+tc.actor {
			t.Errorf("input %d = %+v", i, in)
		}
		if in.ID == "" || in.DedupeKey != "msg:"+in.ID {
			t.Errorf("input %d id %q dedupe %q", i, in.ID, in.DedupeKey)
		}
		found := false
		for _, m := range msgs {
			found = found || m.ID == in.ID
		}
		if !found {
			t.Errorf("input %d has no stored run message", i)
		}
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != core.RunIdle {
		t.Errorf("run state = %s, want idle (the dispatcher queues it, not the engine)", got.State)
	}
}

func TestCancelStopsRunnerSession(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Cancel(ctx, run.ID, "user_1"); err != nil {
		t.Fatal(err)
	}
	if len(route.stopped) != 1 || route.stopped[0] != run.ID {
		t.Fatalf("stopped = %v", route.stopped)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != core.RunCancelled {
		t.Errorf("state = %s", got.State)
	}
	if err := eng.FollowUp(ctx, run.ID, "late", "user_1"); err != nil {
		t.Errorf("follow-up of a cancelled run on the runner path: %v", err)
	}
	if len(route.sent) != 1 {
		t.Errorf("sent = %v", route.sent)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.CancelledBy != "user_1" {
		t.Errorf("cancelled_by = %q", got.CancelledBy)
	}
}

func TestCancelStopsSessionOfFinishedRun(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetRun(ctx, run.ID)
	r.State = core.RunSucceeded
	if err := st.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := eng.Cancel(ctx, run.ID, "user_1"); err != nil {
		t.Fatal(err)
	}
	if len(route.stopped) != 1 {
		t.Fatalf("stopped = %v", route.stopped)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != core.RunSucceeded {
		t.Errorf("state = %s, want succeeded", got.State)
	}
}

func TestInterruptRoutes(t *testing.T) {
	ctx := context.Background()
	eng, _ := newRouteEngine(t, &fakeRoute{})
	if err := eng.Interrupt(ctx, "run_missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	route := &fakeRoute{}
	eng2, _ := newRouteEngine(t, route)
	run2, err := startTestRun(ctx, eng2, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng2.Interrupt(ctx, run2.ID); err != nil {
		t.Fatal(err)
	}
	if len(route.interrupted) != 1 || route.interrupted[0] != run2.ID {
		t.Fatalf("interrupted = %v", route.interrupted)
	}
	route.interruptErr = &core.InterruptError{Reason: core.InterruptNoActiveTurn}
	var ie *core.InterruptError
	if err := eng2.Interrupt(ctx, run2.ID); !errors.As(err, &ie) || ie.Reason != core.InterruptNoActiveTurn {
		t.Fatalf("route err = %v", err)
	}
}

// The first prompt of a new run goes to the queue when the run is created,
// with the ID of the stored user message, so a message that arrives before the
// first turn is behind it.
func TestNewRunQueuesFirstPrompt(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(route.prompts) != 1 || len(route.sent) != 0 {
		t.Fatalf("prompts %d sent %d", len(route.prompts), len(route.sent))
	}
	in := route.prompts[0]
	msgs, _ := st.ListRunMessages(ctx, run.ID, 0)
	if len(msgs) != 1 || in.ID != msgs[0].ID || in.RunID != run.ID || in.Kind != "user" || in.Content != "first" || in.DedupeKey != "prompt" {
		t.Errorf("input %+v messages %+v", in, msgs)
	}
}

// SendMessage returns the record of the message: the ID and the outcome.
func TestSendMessageReturnsRecord(t *testing.T) {
	ctx := context.Background()

	t.Run("record", func(t *testing.T) {
		route := &fakeRoute{outcome: "new_session"}
		eng, st := newRouteEngine(t, route)
		run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := eng.SendMessage(ctx, run.ID, "again", "user_1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Outcome != core.MessageNewSession || rec.MessageID == "" || len(route.sent) != 1 || route.sent[0].ID != rec.MessageID {
			t.Fatalf("record %+v, sent %+v", rec, route.sent)
		}
		msgs, _ := st.ListRunMessages(ctx, run.ID, 0)
		if last := msgs[len(msgs)-1]; last.ID != rec.MessageID || last.Content != "again" {
			t.Errorf("stored %+v", last)
		}
		route.outcome = "live_session"
		if rec, _ = eng.SendMessage(ctx, run.ID, "more", "user_1", nil); rec.Outcome != core.MessageLiveSession {
			t.Errorf("outcome = %q", rec.Outcome)
		}
	})
}

// An engine without a runner route refuses each message, stop and interrupt
// with core.ErrUnavailable. It never panics.
func TestEngineWithoutRunnerRoute(t *testing.T) {
	ctx := context.Background()
	eng, st := newRouteEngine(t, nil)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.FollowUp(ctx, run.ID, "again", "user_1"); !errors.Is(err, core.ErrUnavailable) {
		t.Errorf("follow-up: %v", err)
	}
	if err := eng.Interrupt(ctx, run.ID); !errors.Is(err, core.ErrUnavailable) {
		t.Errorf("interrupt: %v", err)
	}
	// The stop of a session is logged, so a cancel still succeeds.
	if err := eng.Cancel(ctx, run.ID, "user_1"); err != nil {
		t.Errorf("cancel: %v", err)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != core.RunCancelled {
		t.Errorf("state = %s", got.State)
	}
}
