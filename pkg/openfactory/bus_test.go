package openfactory_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
	"github.com/openfactory-org/openfactory/pkg/openfactory"
)

// fakeQueue is an in-memory Queue: Enqueue records jobs, deliver hands a job
// to every handler of its topic synchronously.
type fakeQueue struct {
	mu       sync.Mutex
	handlers map[string][]openfactory.JobHandler
	enqueued []openfactory.Job
}

func (q *fakeQueue) Enqueue(_ context.Context, topic, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	q.mu.Lock()
	q.enqueued = append(q.enqueued, openfactory.Job{ID: id, Topic: topic, Payload: data, Attempt: 1})
	q.mu.Unlock()
	return nil
}

func (q *fakeQueue) Consume(topic string, h openfactory.JobHandler) func() {
	q.mu.Lock()
	if q.handlers == nil {
		q.handlers = map[string][]openfactory.JobHandler{}
	}
	q.handlers[topic] = append(q.handlers[topic], h)
	q.mu.Unlock()
	return func() {}
}

func (q *fakeQueue) consumers(topic string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.handlers[topic])
}

func (q *fakeQueue) deliver(ctx context.Context, job openfactory.Job) error {
	q.mu.Lock()
	hs := append([]openfactory.JobHandler(nil), q.handlers[job.Topic]...)
	q.mu.Unlock()
	for _, h := range hs {
		if err := h(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// fakeBus is an in-memory EventBus (no Queue) that records publishes.
type fakeBus struct {
	mu        sync.Mutex
	subs      map[string][]chan any
	published map[string]int
	closed    atomic.Bool
}

func (b *fakeBus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.published == nil {
		b.published = map[string]int{}
	}
	b.published[topic]++
	for _, ch := range b.subs[topic] {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (b *fakeBus) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, 16)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[string][]chan any{}
	}
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch, func() {}
}

func (b *fakeBus) subscribers(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[topic])
}

func (b *fakeBus) Close() error { b.closed.Store(true); return nil }

// fakeLeader is a Leadership with the optional Start and Done methods.
type fakeLeader struct {
	started atomic.Bool
	asked   atomic.Int64
	done    chan struct{}
}

func (l *fakeLeader) IsLeader() bool { l.asked.Add(1); return true }

func (l *fakeLeader) Start(ctx context.Context) {
	l.started.Store(true)
	go func() { <-ctx.Done(); close(l.done) }()
}

func (l *fakeLeader) Done() <-chan struct{} { return l.done }

func newTestServer(t *testing.T, opts ...openfactory.Option) *openfactory.Server {
	t.Helper()
	cfg := openfactory.DefaultConfig()
	cfg.Dev = true // generated secret key; the bootstrap key goes to io.Discard
	cfg.DatabaseURL = "sqlite://:memory:"
	cfg.DataDir = t.TempDir()
	cfg.SandboxRoot = t.TempDir()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	opts = append([]openfactory.Option{
		openfactory.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		openfactory.WithStdout(io.Discard),
	}, opts...)
	srv, err := openfactory.New(t.Context(), cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv
}

// seenStore wraps a store and records each team whose factories the engine
// lists. The engine lists the factories of the event's team as its first
// store read for an inbound event, so a recorded team shows that the engine
// handled the event. (The engine does not write a delivery marker.)
type seenStore struct {
	*sqlstore.Store
	mu    sync.Mutex
	teams map[string]int
}

func (s *seenStore) ListFactories(ctx context.Context, teamID, cursor string) ([]core.Factory, error) {
	s.mu.Lock()
	if s.teams == nil {
		s.teams = map[string]int{}
	}
	s.teams[teamID]++
	s.mu.Unlock()
	return s.Store.ListFactories(ctx, teamID, cursor)
}

func (s *seenStore) listed(teamID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.teams[teamID] > 0
}

func newSeenStore(t *testing.T) *seenStore {
	t.Helper()
	return &seenStore{Store: testdb.New(t)}
}

func inboundEvent(id, team, delivery string) *openfactory.Event {
	return &openfactory.Event{
		ID: id, TeamID: team, Provider: "custom", Type: "received", DeliveryID: delivery,
		ReceivedAt: time.Now().UTC(),
	}
}

func TestWithQueueRoutesDurableTopics(t *testing.T) {
	ctx := t.Context()
	q := &fakeQueue{}
	st := newSeenStore(t)
	srv := newTestServer(t, openfactory.WithQueue(q), openfactory.WithStore(st))
	defer srv.Close()

	// The engine consumes inbound events and the notification router
	// outbound notices from the replacement queue.
	if n := q.consumers(openfactory.TopicInboundEvent); n == 0 {
		t.Fatal("engine does not consume inbound events from the replacement queue")
	}
	if n := q.consumers(openfactory.TopicOutboundNotice); n == 0 {
		t.Fatal("notification router does not consume outbound notices from the replacement queue")
	}

	payload, _ := json.Marshal(inboundEvent("evt_q1", "team_q1", "dq1"))
	if err := q.deliver(ctx, openfactory.Job{ID: "evt_q1", Topic: openfactory.TopicInboundEvent, Payload: payload, Attempt: 1}); err != nil {
		t.Fatalf("deliver inbound event: %v", err)
	}
	if !st.listed("team_q1") {
		t.Fatal("engine did not handle the event delivered by the replacement queue")
	}

	notice, _ := json.Marshal(openfactory.OutboundMessage{Text: "hi", Kind: "update"})
	if err := q.deliver(ctx, openfactory.Job{ID: "n1", Topic: openfactory.TopicOutboundNotice, Payload: notice, Attempt: 1}); err != nil {
		t.Fatalf("deliver outbound notice: %v", err)
	}
}

func TestWithEventBusReplacesBus(t *testing.T) {
	ctx := t.Context()
	bus := &fakeBus{}
	leader := &fakeLeader{done: make(chan struct{})}
	st := newSeenStore(t)
	srv := newTestServer(t, openfactory.WithEventBus(bus), openfactory.WithLeadership(leader), openfactory.WithStore(st))

	// Without Queue support the engine subscribes to inbound events.
	if bus.subscribers(openfactory.TopicInboundEvent) == 0 {
		t.Fatal("engine did not subscribe to inbound events on the replacement bus")
	}
	bus.Publish(ctx, openfactory.TopicInboundEvent, inboundEvent("evt_b1", "team_b1", "db1"))
	deadline := time.Now().Add(5 * time.Second)
	for !st.listed("team_b1") {
		if time.Now().After(deadline) {
			t.Fatal("engine did not handle the event published on the replacement bus")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !leader.started.Load() || leader.asked.Load() == 0 {
		t.Fatalf("replacement leadership not used: started=%v asked=%d", leader.started.Load(), leader.asked.Load())
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if !bus.closed.Load() {
		t.Fatal("Close did not close the replacement bus")
	}
}

type fakeKeys struct{ wrapped atomic.Int32 }

func (f *fakeKeys) CurrentVersion() string { return "ext-1" }
func (f *fakeKeys) Wrap(_ context.Context, dek []byte) ([]byte, string, error) {
	f.wrapped.Add(1)
	return append([]byte("X"), dek...), "ext-1", nil
}
func (f *fakeKeys) Unwrap(_ context.Context, w []byte, _ string) ([]byte, error) { return w[1:], nil }

func TestWithKeyProviderOption(t *testing.T) {
	var p openfactory.KeyProvider = &fakeKeys{}
	srv := newTestServer(t, openfactory.WithKeyProvider(p))
	defer srv.Close()
}
