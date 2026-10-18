package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

type recQueue struct {
	mu       sync.Mutex
	jobs     []core.Job
	handlers map[string]core.JobHandler
	stopped  bool
}

func (q *recQueue) Enqueue(_ context.Context, topic, id string, _ any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, core.Job{ID: id, Topic: topic})
	return nil
}

func (q *recQueue) Consume(topic string, h core.JobHandler) func() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.handlers == nil {
		q.handlers = map[string]core.JobHandler{}
	}
	q.handlers[topic] = h
	return func() { q.mu.Lock(); q.stopped = true; q.mu.Unlock() }
}

func (q *recQueue) handler(topic string) core.JobHandler {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.handlers[topic]
}

type recBus struct {
	mu     sync.Mutex
	topics []string
}

func (b *recBus) Publish(_ context.Context, topic string, _ any) {
	b.mu.Lock()
	b.topics = append(b.topics, topic)
	b.mu.Unlock()
}

func (b *recBus) Subscribe(string) (<-chan any, func()) { return make(chan any), func() {} }

func TestQueuedBusPublish(t *testing.T) {
	tests := []struct {
		name      string
		topic     string
		payload   any
		wantQueue bool
		wantID    string
	}{
		{"inbound event keeps its ID", core.TopicInboundEvent, &core.Event{ID: "evt_1"}, true, "evt_1"},
		{"inbound event value", core.TopicInboundEvent, core.Event{ID: "evt_2"}, true, "evt_2"},
		{"outbound notice", core.TopicOutboundNotice, &core.OutboundMessage{Text: "x"}, true, ""},
		{"live topic goes to the base bus", core.TopicRunUpdated, &core.Run{ID: "run_1"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, base := &recQueue{}, &recBus{}
			b := newQueuedBus(base, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
			b.Publish(t.Context(), tt.topic, tt.payload)
			if tt.wantQueue {
				if len(q.jobs) != 1 || q.jobs[0].Topic != tt.topic || q.jobs[0].ID != tt.wantID || len(base.topics) != 0 {
					t.Fatalf("queue jobs = %+v, base = %v", q.jobs, base.topics)
				}
				return
			}
			if len(q.jobs) != 0 || len(base.topics) != 1 || base.topics[0] != tt.topic {
				t.Fatalf("queue jobs = %+v, base = %v", q.jobs, base.topics)
			}
		})
	}
}

func TestQueuedBusAssignsEventID(t *testing.T) {
	q := &recQueue{}
	b := newQueuedBus(&recBus{}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ev := &core.Event{}
	b.Publish(t.Context(), core.TopicInboundEvent, ev)
	if ev.ID == "" || q.jobs[0].ID != ev.ID {
		t.Fatalf("event ID %q, job ID %q", ev.ID, q.jobs[0].ID)
	}
}

func TestQueuedBusSubscribeDurable(t *testing.T) {
	q := &recQueue{}
	b := newQueuedBus(&recBus{}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch, unsub := b.Subscribe(core.TopicOutboundNotice)
	h := q.handler(core.TopicOutboundNotice)
	if h == nil {
		t.Fatal("Subscribe did not consume the durable topic")
	}
	if err := h(t.Context(), core.Job{Topic: core.TopicOutboundNotice, Payload: []byte(`{"text":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-ch:
		m, ok := v.(*core.OutboundMessage)
		if !ok || m.Text != "hello" {
			t.Fatalf("got %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("no value delivered")
	}
	if err := h(t.Context(), core.Job{Topic: core.TopicOutboundNotice, Payload: []byte(`not json`)}); err == nil {
		t.Fatal("undecodable job acknowledged")
	}
	unsub()
	unsub() // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed after unsubscribe")
	}
	if !q.stopped {
		t.Fatal("unsubscribe did not stop the consumer")
	}
	if err := h(t.Context(), core.Job{Topic: core.TopicOutboundNotice, Payload: []byte(`{}`)}); err == nil {
		t.Fatal("job acknowledged after unsubscribe")
	}
}
