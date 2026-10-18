package runnerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func ev(t *testing.T, seq int64, typ runnerproto.EventType, turn string, data any) runnerproto.Event {
	t.Helper()
	e, err := runnerproto.NewEvent(seq, typ, turn, time.Now(), data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func send(t *testing.T, c *runnerclient.Client, events ...runnerproto.Event) int64 {
	t.Helper()
	ack, err := c.SendEvents(context.Background(), runnerproto.EventBatch{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func msgs(t *testing.T, seqs ...int64) []runnerproto.Event {
	var out []runnerproto.Event
	for _, s := range seqs {
		out = append(out, ev(t, s, runnerproto.EventMessage, "", runnerproto.MessageData{Role: "assistant", Content: "x"}))
	}
	return out
}

func TestEventsAckAndGap(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	if ack := send(t, c, msgs(t, 1, 2, 3)...); ack != 3 {
		t.Fatalf("ack %d", ack)
	}
	if ack := send(t, c, msgs(t, 2, 3, 4)...); ack != 4 {
		t.Fatalf("ack %d", ack)
	}
	// The hook gets only the new events.
	if len(e.hooks.events) != 4 || e.hooks.events[3].Seq != 4 || e.hooks.batches != 2 {
		t.Fatalf("hook events: %+v batches %d", e.hooks.events, e.hooks.batches)
	}
	ack, err := c.SendEvents(context.Background(), runnerproto.EventBatch{Events: msgs(t, 7)})
	if !errors.Is(err, runnerclient.ErrGap) || ack != 4 {
		t.Fatalf("want gap with ack 4, got %d %v", ack, err)
	}
	// A resend of old events only does not call the hook.
	send(t, c, msgs(t, 1, 2)...)
	if e.hooks.batches != 2 {
		t.Fatalf("hook called for old events: %d", e.hooks.batches)
	}
}

func TestEventsEmptyBatch(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	if ack := send(t, c); ack != 0 {
		t.Fatalf("ack %d", ack)
	}
	send(t, c, msgs(t, 1)...)
	if ack := send(t, c); ack != 1 {
		t.Fatalf("ack %d", ack)
	}
}

func TestEventsBadOrder(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	_, err := c.SendEvents(context.Background(), runnerproto.EventBatch{Events: msgs(t, 1, 3)})
	var se *runnerclient.StatusError
	if !errors.As(err, &se) || se.Status != 400 || se.Code != "bad_request" {
		t.Fatalf("want 400, got %v", err)
	}
}

func TestSessionReadyStoresCapabilities(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	send(t, c, ev(t, 1, runnerproto.EventSessionReady, "", runnerproto.SessionReadyData{
		Capabilities: []runnerproto.Capability{runnerproto.CapInterrupt},
	}))
	got := e.session(1).Capabilities
	if len(got) != 1 || got[0] != "interrupt" {
		t.Fatalf("capabilities %v", got)
	}
}

// deliver makes n inputs and marks them delivered for a turn.
func (e *env) deliver(n int, turn string) []string {
	e.t.Helper()
	ctx := context.Background()
	var ids []string
	for i := 0; i < n; i++ {
		m := &core.RunInput{RunID: e.run.ID, Kind: "user", Content: "hi"}
		if _, err := e.mem.AppendRunInput(ctx, m); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	if got, err := e.mem.MarkRunInputsDelivered(ctx, e.run.ID, ids, 1, turn); err != nil || got != n {
		e.t.Fatalf("mark delivered: %d %v", got, err)
	}
	return ids
}

func (e *env) input(id string) *core.RunInput {
	e.t.Helper()
	m, err := e.mem.GetRunInput(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestInputFlowTurnStartedAndEnded(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	ids := e.deliver(2, "t1")

	send(t, c, ev(t, 1, runnerproto.EventTurnStarted, "t1", nil))
	for _, id := range ids {
		if m := e.input(id); !m.Accepted || m.State != core.RunInputDelivered {
			t.Fatalf("input %s: %+v", id, m)
		}
	}
	if got := e.session(1).ActiveTurnID; got != "t1" {
		t.Fatalf("active turn %q", got)
	}

	send(t, c, ev(t, 2, runnerproto.EventTurnEnded, "t1", runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone}))
	for _, id := range ids {
		if m := e.input(id); m.State != core.RunInputConsumed {
			t.Fatalf("input %s: %+v", id, m)
		}
	}
	if got := e.session(1).ActiveTurnID; got != "" {
		t.Fatalf("active turn %q", got)
	}
}

func TestInputAcceptedThenTurnEnded(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	ids := e.deliver(2, "t1")

	send(t, c, ev(t, 1, runnerproto.EventInputAccepted, "t1", runnerproto.InputAcceptedData{MessageIDs: ids[:1]}))
	send(t, c, ev(t, 2, runnerproto.EventTurnEnded, "t1", runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone}))
	if m := e.input(ids[0]); m.State != core.RunInputConsumed {
		t.Fatalf("first: %+v", m)
	}
	if m := e.input(ids[1]); m.State != core.RunInputPending || m.Session != 0 || m.TurnID != "" {
		t.Fatalf("second: %+v", m)
	}
}

func TestEventsAfterEndAreRefused(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	send(t, c, msgs(t, 1)...)
	if err := c.End(context.Background(), runnerproto.EndRequest{Reason: runnerproto.EndIdle, LastEventSeq: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := c.SendEvents(context.Background(), runnerproto.EventBatch{Events: msgs(t, 2)})
	if !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}

// A runner that ends its session right after its last batch (idle timeout 0)
// closes the connection, which cancels the request context. The stored events
// must still take effect, or a lost turn.ended starts a new session.
func TestEventsProcessedAfterClientGoesAway(t *testing.T) {
	// The store calls that authenticate and store the batch ignore the
	// cancellation, so only the processing of stored events is tested.
	e := newEnv(t, func(c *Config) { c.Runner = storeIgnoresCancel{c.Runner} })
	h := e.svc.Handler()
	e.srv.Close()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == runnerproto.PathSessionEvents {
			ctx, cancel := context.WithCancel(r.Context())
			cancel() // the runner has gone away
			r = r.WithContext(ctx)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.srv.Close)
	c := e.started()
	send(t, c, msgs(t, 1)...)
	if len(e.hooks.ctxErrs) != 1 || e.hooks.ctxErrs[0] != nil {
		t.Fatalf("events were processed with a cancelled context: %v", e.hooks.ctxErrs)
	}
}

// storeIgnoresCancel runs the store calls made before the events are stored
// without the request cancellation, as if the runner went away just after.
type storeIgnoresCancel struct{ core.RunnerStore }

func (s storeIgnoresCancel) GetRunSession(ctx context.Context, runID string, session int) (*core.RunSession, error) {
	return s.RunnerStore.GetRunSession(context.WithoutCancel(ctx), runID, session)
}

func (s storeIgnoresCancel) LatestRunSession(ctx context.Context, runID string) (*core.RunSession, error) {
	return s.RunnerStore.LatestRunSession(context.WithoutCancel(ctx), runID)
}

func (s storeIgnoresCancel) TouchRunSession(ctx context.Context, runID string, session int, at time.Time) error {
	return s.RunnerStore.TouchRunSession(context.WithoutCancel(ctx), runID, session, at)
}

func (s storeIgnoresCancel) AppendRunnerEvents(ctx context.Context, runID string, session int, events []core.RunnerEvent) (int64, error) {
	return s.RunnerStore.AppendRunnerEvents(context.WithoutCancel(ctx), runID, session, events)
}
