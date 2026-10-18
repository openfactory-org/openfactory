package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/openfactory-org/openfactory/internal/core"
)

// WithEventBus replaces the built-in durable event bus (outbox plus live
// notifications) for every component. A bus that also implements core.Queue
// gets at-least-once inbound event handling (the engine acknowledges a job
// only after handling it); otherwise the engine subscribes to
// core.TopicInboundEvent. A bus that implements io.Closer is closed on
// Close. Without it the built-in bus is used.
func WithEventBus(b core.EventBus) Option { return func(o *options) { o.bus = b } }

// WithQueue routes the durable topics (core.DurableTopics: inbound events and
// outbound notices) through q instead of the built-in outbox. Live topics
// still go through the event bus (built-in or WithEventBus). Subscribers of a
// durable topic receive each job q hands to this replica, decoded like the
// built-in bus does (*core.Event, *core.OutboundMessage). The server does
// not close q.
func WithQueue(q core.Queue) Option { return func(o *options) { o.queue = q } }

// WithLeadership replaces the built-in leader lease (OF_LEADER_LEASE_TTL)
// that gates the scheduler, git definition polling, lease sweeping and
// run-data retention to one replica. When l has a Start(context.Context)
// method it is called from Start; when it has a Done() <-chan struct{}
// method Close waits for it (release the lease there).
func WithLeadership(l core.Leadership) Option { return func(o *options) { o.leadership = l } }

// queuedBus is an event bus whose durable topics go through a core.Queue
// and whose live topics go to base.
type queuedBus struct {
	base core.EventBus
	q    core.Queue
	log  *slog.Logger
}

var (
	_ core.EventBus = (*queuedBus)(nil)
	_ core.Queue    = (*queuedBus)(nil)
)

func newQueuedBus(base core.EventBus, q core.Queue, log *slog.Logger) *queuedBus {
	return &queuedBus{base: base, q: q, log: log}
}

// Publish enqueues durable topics on the queue (deduplicated by event ID)
// and forwards live topics to the base bus.
func (b *queuedBus) Publish(ctx context.Context, topic string, payload any) {
	if !core.IsDurableTopic(topic) {
		b.base.Publish(ctx, topic, payload)
		return
	}
	id := ""
	switch v := payload.(type) {
	case *core.Event:
		if v.ID == "" {
			v.ID = core.NewID("evt")
		}
		id = v.ID
	case core.Event:
		id = v.ID
	}
	if err := b.q.Enqueue(ctx, topic, id, payload); err != nil {
		b.log.ErrorContext(ctx, "queue enqueue failed; message dropped", "topic", topic, "id", id, "err", err)
	}
}

// Subscribe delivers the jobs of a durable topic as decoded values; a job is
// acknowledged once the subscriber received it. Live topics come from the
// base bus.
func (b *queuedBus) Subscribe(topic string) (<-chan any, func()) {
	if !core.IsDurableTopic(topic) {
		return b.base.Subscribe(topic)
	}
	ch := make(chan any, 64)
	done := make(chan struct{})
	var mu sync.RWMutex
	closed := false
	stop := b.q.Consume(topic, func(ctx context.Context, job core.Job) error {
		v, err := decodeJob(job)
		if err != nil {
			return err
		}
		mu.RLock()
		defer mu.RUnlock()
		if closed {
			return context.Canceled
		}
		select {
		case ch <- v:
			return nil
		case <-done:
			return context.Canceled
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			close(done)
			stop()
			mu.Lock()
			closed = true
			close(ch)
			mu.Unlock()
		})
	}
}

// SubscribeLocal forwards to the base bus (live topics only reach this replica).
func (b *queuedBus) SubscribeLocal(topic string) (<-chan any, func()) {
	if ls, ok := b.base.(interface {
		SubscribeLocal(topic string) (<-chan any, func())
	}); ok {
		return ls.SubscribeLocal(topic)
	}
	return b.base.Subscribe(topic)
}

// Enqueue implements core.Queue.
func (b *queuedBus) Enqueue(ctx context.Context, topic, id string, payload any) error {
	return b.q.Enqueue(ctx, topic, id, payload)
}

// Consume implements core.Queue.
func (b *queuedBus) Consume(topic string, h core.JobHandler) func() { return b.q.Consume(topic, h) }

// decodeJob decodes a durable job payload into the value the built-in bus
// hands to subscribers.
func decodeJob(job core.Job) (any, error) {
	switch job.Topic {
	case core.TopicInboundEvent:
		v := &core.Event{}
		return v, json.Unmarshal(job.Payload, v)
	case core.TopicOutboundNotice:
		v := &core.OutboundMessage{}
		return v, json.Unmarshal(job.Payload, v)
	}
	return json.RawMessage(append([]byte(nil), job.Payload...)), nil
}
