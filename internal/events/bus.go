// Package events provides the event buses: Bus, the in-process fan-out bus,
// and Durable (durable.go), the database-backed core.EventBus and core.Queue
// used in production.
//
// The bus fans each published payload out to every subscriber of the topic.
// Each subscriber has its own buffered channel; Publish never blocks — when a
// subscriber's buffer is full the message is dropped for that subscriber, a
// warning is logged and the drop counters are incremented.
package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/openfactory-org/openfactory/internal/core"
)

// DefaultBufferSize is the per-subscriber channel capacity.
const DefaultBufferSize = 256

// Wildcard subscribes to every topic.
const Wildcard = "*"

// Envelope is delivered to Wildcard subscribers so they can see the topic.
type Envelope struct {
	Topic   string
	Payload any
}

type subscriber struct {
	topic   string
	ch      chan any
	dropped atomic.Uint64
	closed  bool // guarded by Bus.mu
}

// Bus is an in-process, fan-out publish/subscribe bus. It is safe for
// concurrent use. The zero value is not usable; call NewBus.
type Bus struct {
	mu      sync.RWMutex
	subs    map[string]map[*subscriber]struct{}
	closed  bool
	bufSize int
	logger  *slog.Logger

	published atomic.Uint64
	dropped   atomic.Uint64
}

var _ core.EventBus = (*Bus)(nil)

// Option configures a Bus.
type Option func(*Bus)

// WithBufferSize sets the per-subscriber buffer size (default 256).
func WithBufferSize(n int) Option {
	return func(b *Bus) {
		if n > 0 {
			b.bufSize = n
		}
	}
}

// WithLogger sets the logger used for drop warnings (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(b *Bus) {
		if l != nil {
			b.logger = l
		}
	}
}

// NewBus returns a ready bus as a core.EventBus. Use New for the concrete type.
func NewBus() core.EventBus { return New() }

// New returns a ready bus.
func New(opts ...Option) *Bus {
	b := &Bus{
		subs:    map[string]map[*subscriber]struct{}{},
		bufSize: DefaultBufferSize,
		logger:  slog.Default(),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Publish delivers payload to every current subscriber of topic (and of
// Wildcard, wrapped in an Envelope). It never blocks. Publishing on a closed
// bus is a no-op. Inside a store transaction (core.Transactor) delivery
// happens after the commit, and not at all on rollback.
func (b *Bus) Publish(ctx context.Context, topic string, payload any) {
	if core.InTransaction(ctx) {
		core.AfterCommit(ctx, func() { b.publish(context.WithoutCancel(ctx), topic, payload) })
		return
	}
	b.publish(ctx, topic, payload)
}

// TxAware reports that Publish defers delivery until a store transaction
// carried by the context commits.
func (b *Bus) TxAware() bool { return true }

func (b *Bus) publish(ctx context.Context, topic string, payload any) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	b.published.Add(1)
	for s := range b.subs[topic] {
		b.deliver(ctx, s, topic, payload)
	}
	if topic != Wildcard {
		if wild := b.subs[Wildcard]; len(wild) > 0 {
			env := Envelope{Topic: topic, Payload: payload}
			for s := range wild {
				b.deliver(ctx, s, topic, env)
			}
		}
	}
}

// deliver must be called with b.mu held (read lock suffices: channels are
// only closed under the write lock).
func (b *Bus) deliver(ctx context.Context, s *subscriber, topic string, v any) {
	select {
	case s.ch <- v:
	default:
		n := s.dropped.Add(1)
		b.dropped.Add(1)
		// Log the first drop and then every 100th to avoid log floods.
		if n == 1 || n%100 == 0 {
			b.logger.WarnContext(ctx, "events: subscriber buffer full, dropping message",
				"topic", topic, "subscription", s.topic, "dropped", n, "buffer", cap(s.ch))
		}
	}
}

// Subscribe returns a channel receiving payloads published on topic (use
// Wildcard for all topics; payloads are then Envelopes) and an idempotent
// unsubscribe function that closes the channel. Subscribing to a closed bus
// returns an already-closed channel.
func (b *Bus) Subscribe(topic string) (<-chan any, func()) {
	s := &subscriber{topic: topic, ch: make(chan any, b.bufSize)}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	m := b.subs[topic]
	if m == nil {
		m = map[*subscriber]struct{}{}
		b.subs[topic] = m
	}
	m[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if s.closed {
				return
			}
			s.closed = true
			if m := b.subs[topic]; m != nil {
				delete(m, s)
				if len(m) == 0 {
					delete(b.subs, topic)
				}
			}
			close(s.ch)
		})
	}
}

// Close closes every subscriber channel; later Publish calls are no-ops and
// later Subscribe calls return closed channels. Close is idempotent.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, m := range b.subs {
		for s := range m {
			if !s.closed {
				s.closed = true
				close(s.ch)
			}
		}
	}
	b.subs = map[string]map[*subscriber]struct{}{}
	return nil
}

// Stats is a snapshot of bus counters.
type Stats struct {
	Published   uint64
	Dropped     uint64
	Subscribers int
}

// Stats returns the current counters.
func (b *Bus) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, m := range b.subs {
		n += len(m)
	}
	return Stats{Published: b.published.Load(), Dropped: b.dropped.Load(), Subscribers: n}
}

// Dropped returns the total number of dropped deliveries.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }

// ErrClosed is returned by Wait when the bus closes before a match arrives.
var ErrClosed = errors.New("events: bus closed")

// Wait subscribes to topic and returns the first payload for which pred
// returns true (pred may be nil to accept any payload). It returns ctx.Err()
// when the context ends and ErrClosed if the subscription channel closes.
//
// To avoid missing an event that is published concurrently with the call,
// check current state after subscribing — or use WaitFunc.
func Wait(ctx context.Context, bus core.EventBus, topic string, pred func(any) bool) (any, error) {
	return WaitFunc(ctx, bus, topic, pred, nil)
}

// WaitFunc is like Wait but, once subscribed, first calls check (if non-nil).
// If check reports done, its value is returned immediately. This closes the
// race between reading current state and subscribing, which long-polling
// handlers need ("return the run if it is already finished, otherwise wait
// for the update").
func WaitFunc(ctx context.Context, bus core.EventBus, topic string, pred func(any) bool, check func() (any, bool, error)) (any, error) {
	ch, unsub := bus.Subscribe(topic)
	defer unsub()
	if check != nil {
		v, done, err := check()
		if err != nil {
			return nil, err
		}
		if done {
			return v, nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case v, ok := <-ch:
			if !ok {
				return nil, ErrClosed
			}
			if pred == nil || pred(v) {
				return v, nil
			}
		}
	}
}

// hasSubscribers reports whether topic (or, for any topic but Wildcard
// itself, the Wildcard) has a subscriber.
func (b *Bus) hasSubscribers(topic string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.subs[topic]) > 0 {
		return true
	}
	return topic != Wildcard && len(b.subs[Wildcard]) > 0
}
