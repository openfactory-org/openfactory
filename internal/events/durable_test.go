package events

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func fastOpts(replica string) DurableOptions {
	return DurableOptions{ReplicaID: replica, PollInterval: 10 * time.Millisecond, BaseBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond, SubscriberTimeout: time.Second}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDurableDeliversAndDedupes(t *testing.T) {
	st := testdb.New(t)
	d := NewForStore(st, fastOpts("a"))
	defer d.Close()
	ch, unsub := d.Subscribe(core.TopicInboundEvent)
	defer unsub()
	wild, unsubW := d.Subscribe(Wildcard)
	defer unsubW()

	ctx := context.Background()
	ev := &core.Event{ID: "evt_1", Provider: "slack", Text: "hi"}
	d.Publish(ctx, core.TopicInboundEvent, ev)
	d.Publish(ctx, core.TopicInboundEvent, &core.Event{ID: "evt_1", Provider: "slack", Text: "dup"})
	got, ok := recv(t, ch).(*core.Event)
	if !ok || got.ID != "evt_1" || got.Text != "hi" {
		t.Fatalf("got %#v", got)
	}
	env := recv(t, wild).(Envelope)
	if env.Topic != core.TopicInboundEvent {
		t.Fatalf("wildcard envelope topic %q", env.Topic)
	}
	select {
	case v := <-ch:
		t.Fatalf("duplicate delivered: %#v", v)
	case <-time.After(100 * time.Millisecond):
	}
	waitFor(t, "delivered state", func() bool {
		msgs, _ := st.OutboxList(ctx, core.OutboxDelivered, 10)
		return len(msgs) == 1 && msgs[0].ID == "evt_1"
	})

	// Events without an ID get one, so they can be deduplicated downstream.
	anon := &core.Event{Provider: "x"}
	d.Publish(ctx, core.TopicInboundEvent, anon)
	if anon.ID == "" {
		t.Fatal("event id not assigned")
	}
	if got := recv(t, ch).(*core.Event); got.ID != anon.ID {
		t.Fatalf("got %s want %s", got.ID, anon.ID)
	}
}

func TestDurableRetriesThenDeadLetters(t *testing.T) {
	st := testdb.New(t)
	opts := fastOpts("a")
	opts.MaxAttempts = 3
	d := NewForStore(st, opts)
	defer d.Close()

	var flaky, broken atomic.Int32
	stop := d.Consume("jobs", func(_ context.Context, j core.Job) error {
		switch j.ID {
		case "flaky":
			if flaky.Add(1) == 1 {
				return errors.New("transient")
			}
			if j.Attempt != 2 {
				return fmt.Errorf("attempt = %d, want 2", j.Attempt)
			}
			return nil
		case "broken":
			broken.Add(1)
			return errors.New("permanent")
		}
		return nil
	})
	defer stop()
	ctx := context.Background()
	if err := d.Enqueue(ctx, "jobs", "flaky", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, "jobs", "broken", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "dead letter", func() bool {
		msgs, _ := st.OutboxList(ctx, core.OutboxDead, 10)
		return len(msgs) == 1 && msgs[0].ID == "broken"
	})
	waitFor(t, "flaky delivered", func() bool {
		msgs, _ := st.OutboxList(ctx, core.OutboxDelivered, 10)
		return len(msgs) == 1 && msgs[0].ID == "flaky"
	})
	if n := broken.Load(); n != 3 {
		t.Fatalf("broken attempts = %d, want 3", n)
	}
	dead, _ := st.OutboxList(ctx, core.OutboxDead, 10)
	if dead[0].LastError != "permanent" || dead[0].Attempts != 3 {
		t.Fatalf("dead = %+v", dead[0])
	}
	if s := d.DurableStats(); s.DeadLettered != 1 || s.Retried < 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestDurableBackoffGrowsAndCaps(t *testing.T) {
	d := &Durable{opts: DurableOptions{BaseBackoff: time.Second, MaxBackoff: 10 * time.Second}}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 10: 10 * time.Second} {
		got := d.backoff(attempt)
		if got < want*8/10 || got > want*12/10 {
			t.Fatalf("backoff(%d) = %v, want ~%v", attempt, got, want)
		}
	}
}

// Two replicas sharing one database: every durable message is handled by
// exactly one of them.
func TestDurableTwoReplicasProcessOnce(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) core.Store{
		"sqlite": func(t *testing.T) core.Store {
			s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "of.db"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			defer st.Close()
			a := NewForStore(st, fastOpts("a"))
			b := NewForStore(st, fastOpts("b"))
			defer a.Close()
			defer b.Close()
			var mu sync.Mutex
			seen := map[string][]string{}
			handler := func(replica string) core.JobHandler {
				return func(_ context.Context, j core.Job) error {
					mu.Lock()
					seen[j.ID] = append(seen[j.ID], replica)
					mu.Unlock()
					return nil
				}
			}
			defer a.Consume(core.TopicInboundEvent, handler("a"))()
			defer b.Consume(core.TopicInboundEvent, handler("b"))()
			const n = 60
			ctx := context.Background()
			for i := 0; i < n; i++ {
				pub := a
				if i%2 == 1 {
					pub = b
				}
				pub.Publish(ctx, core.TopicInboundEvent, &core.Event{ID: fmt.Sprintf("evt_%d", i)})
			}
			waitFor(t, "all handled", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(seen) == n
			})
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			for id, by := range seen {
				if len(by) != 1 {
					t.Fatalf("%s handled %d times by %v", id, len(by), by)
				}
			}
		})
	}
}

// A message published while nothing consumes it survives a restart.
func TestDurableSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "of.db")
	ctx := context.Background()
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d := NewForStore(st, fastOpts("a"))
	d.Publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{WorkItemID: "wi_1", Text: "hello"})
	_ = d.Close()
	_ = st.Close()

	st2, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	d2 := NewForStore(st2, fastOpts("a"))
	defer d2.Close()
	ch, unsub := d2.Subscribe(core.TopicOutboundNotice)
	defer unsub()
	got, ok := recv(t, ch).(*core.OutboundMessage)
	if !ok || got.WorkItemID != "wi_1" || got.Text != "hello" {
		t.Fatalf("got %#v", got)
	}
}

// Live topics reach other replicas by ID; the receiver reloads the record.
func TestDurableLiveFanOutAcrossReplicas(t *testing.T) {
	st := testdb.New(t)
	a := NewForStore(st, fastOpts("a"))
	b := NewForStore(st, fastOpts("b"))
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	run := &core.Run{FactoryID: "fac_test", TeamID: "t", State: core.RunRunning}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	msg := &core.RunMessage{RunID: run.ID, Role: "assistant", Content: "hello"}
	if err := st.AppendRunMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	aRuns, unsubA := a.Subscribe(core.TopicRunUpdated)
	defer unsubA()
	bRuns, unsubB := b.Subscribe(core.TopicRunUpdated)
	defer unsubB()
	bMsgs, unsubM := b.Subscribe(core.TopicRunMessage)
	defer unsubM()
	bLocal, unsubL := b.SubscribeLocal(core.TopicRunUpdated)
	defer unsubL()
	aLocal, unsubAL := a.SubscribeLocal(core.TopicRunUpdated)
	defer unsubAL()

	a.Publish(ctx, core.TopicRunUpdated, run)
	a.Publish(ctx, core.TopicRunMessage, msg)
	if got := recv(t, aRuns); got != run {
		t.Fatalf("local subscriber must get the published payload, got %#v", got)
	}
	if got := recv(t, bRuns).(*core.Run); got.ID != run.ID || got.State != core.RunRunning {
		t.Fatalf("remote run = %#v", got)
	}
	if got := recv(t, bMsgs).(*core.RunMessage); got.Seq != msg.Seq || got.Content != "hello" {
		t.Fatalf("remote message = %#v", got)
	}
	if got := recv(t, aLocal); got != run {
		t.Fatalf("local-only subscriber of the publisher = %#v", got)
	}
	select {
	case v := <-aRuns:
		t.Fatalf("publisher received its own relay: %#v", v)
	case v := <-bLocal:
		t.Fatalf("local-only subscriber received a relay: %#v", v)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDurableCloseIsIdempotent(t *testing.T) {
	d := NewForStore(testdb.New(t), fastOpts("a"))
	ch, _ := d.Subscribe(core.TopicInboundEvent)
	live, _ := d.Subscribe(core.TopicRunUpdated)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("durable channel not closed")
	}
	if _, ok := <-live; ok {
		t.Fatal("live channel not closed")
	}
	d.Publish(context.Background(), core.TopicInboundEvent, &core.Event{ID: "x"}) // no panic
}
