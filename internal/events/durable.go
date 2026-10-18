package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/coord"
)

// DurableOptions configures a Durable bus.
type DurableOptions struct {
	// ReplicaID identifies this replica (default coord.ReplicaID("")).
	ReplicaID string
	// PollInterval is how often the outbox is polled when no wake-up arrives
	// (default 1s). PostgreSQL wakes dispatchers through NOTIFY; polling is
	// the safety net and SQLite's only trigger besides in-process wake-ups.
	PollInterval time.Duration
	// Visibility hides a claimed outbox message from other replicas while it
	// is being delivered (default 5m). A replica that dies mid-delivery
	// releases its messages after this delay.
	Visibility time.Duration
	// MaxAttempts is the number of deliveries before a message is dead-lettered
	// (default 8).
	MaxAttempts int
	// BaseBackoff and MaxBackoff bound the exponential retry delay (defaults
	// 1s and 5m); ±20% jitter is applied.
	BaseBackoff, MaxBackoff time.Duration
	// BatchSize is the number of messages claimed at once (default 16) and
	// Concurrency the number delivered in parallel (default 4).
	BatchSize, Concurrency int
	// SubscriberTimeout bounds a blocking hand-off to a channel subscriber of a
	// durable topic; a timeout counts as a failed delivery (default 30s).
	SubscriberTimeout time.Duration
	// Retention keeps delivered outbox rows (the deduplication window) before
	// they are purged (default 72h; negative disables purging).
	Retention time.Duration
	// BufferSize is the per-subscriber buffer of live topics (default 256).
	BufferSize int
	Clock      func() time.Time
	Logger     *slog.Logger
}

// Durable is the database-backed core.EventBus and core.Queue.
//
//   - Durable topics (core.DurableTopics) are written to the outbox and
//     delivered at least once, by one replica, with retries, exponential
//     backoff and dead-lettering. Publish never drops them: if the outbox
//     write fails, the payload is delivered in process (and the error logged).
//   - Live topics fan out in process immediately and to the other replicas
//     through core.Coordinator.Notify, carrying only IDs; receiving replicas
//     reload the record from the store.
//
// Wildcard subscribers see every live publish made on this replica, live
// publishes relayed from other replicas, and durable messages delivered on
// this replica.
type Durable struct {
	c      core.Coordinator
	store  core.Store
	opts   DurableOptions
	log    *slog.Logger
	holder string
	local  *Bus // live fan-out: local publishes and relays from other replicas
	origin *Bus // local publishes only (SubscribeLocal)

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	wake     chan struct{}
	unlisten func()

	mu       sync.RWMutex
	handlers map[string]map[*handlerReg]struct{}
	chans    map[string]map[*chanSub]struct{}
	closed   bool

	published  atomic.Uint64
	delivered  atomic.Uint64
	retried    atomic.Uint64
	deadLetter atomic.Uint64
	fallbacks  atomic.Uint64
}

type handlerReg struct{ h core.JobHandler }

type chanSub struct {
	ch     chan any
	mu     sync.Mutex
	closed bool
}

var (
	_ core.EventBus = (*Durable)(nil)
	_ core.Queue    = (*Durable)(nil)
)

// NewForStore returns the bus for a store: a Durable bus on the store's
// core.Coordinator (SQLite, PostgreSQL, memory). Stores without one get a
// process-local coordinator (coord.From), so durable topics still retry but
// live only in memory.
func NewForStore(store core.Store, opts DurableOptions) *Durable {
	return NewDurable(coord.From(store), store, opts)
}

// NewDurable returns a running Durable bus on c. store (optional) reloads live
// payloads announced by other replicas; without it those are not relayed.
// Call Close to stop it.
func NewDurable(c core.Coordinator, store core.Store, opts DurableOptions) *Durable {
	opts.ReplicaID = coord.ReplicaID(opts.ReplicaID)
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.Visibility <= 0 {
		opts.Visibility = 5 * time.Minute
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = time.Second
	}
	if opts.MaxBackoff < opts.BaseBackoff {
		opts.MaxBackoff = 5 * time.Minute
		if opts.MaxBackoff < opts.BaseBackoff {
			opts.MaxBackoff = opts.BaseBackoff
		}
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 16
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.SubscriberTimeout <= 0 {
		opts.SubscriberTimeout = 30 * time.Second
	}
	if opts.Retention == 0 {
		opts.Retention = 72 * time.Hour
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	busOpts := []Option{WithLogger(log)}
	if opts.BufferSize > 0 {
		busOpts = append(busOpts, WithBufferSize(opts.BufferSize))
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Durable{
		c: c, store: store, opts: opts,
		log:      log.With("component", "events", "replica", opts.ReplicaID),
		holder:   coord.NewHolder(opts.ReplicaID),
		local:    New(busOpts...),
		origin:   New(busOpts...),
		ctx:      ctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
		handlers: map[string]map[*handlerReg]struct{}{},
		chans:    map[string]map[*chanSub]struct{}{},
	}
	d.unlisten = c.Listen(d.onNotify)
	d.wg.Add(1)
	go d.dispatch()
	return d
}

// ---------- notifications ----------

// note is the NOTIFY payload. It carries IDs only (PostgreSQL limits payloads
// to 8000 bytes); receivers reload the record.
type note struct {
	Kind   string `json:"k,omitempty"` // "outbox" wakes dispatchers; empty = live update
	Origin string `json:"o"`
	Topic  string `json:"t,omitempty"`
	ID     string `json:"id,omitempty"`
	RunID  string `json:"r,omitempty"`
	Seq    int64  `json:"s,omitempty"`
	// End is the last sequence number of a batch of run messages
	// (PublishRunMessages); zero means the single message Seq.
	End int64 `json:"e,omitempty"`
}

func (d *Durable) notify(n note) {
	n.Origin = d.opts.ReplicaID
	b, _ := json.Marshal(n)
	if err := d.c.Notify(d.ctx, string(b)); err != nil && d.ctx.Err() == nil {
		d.log.Debug("notify failed", "topic", n.Topic, "err", err)
	}
}

func (d *Durable) onNotify(payload string) {
	var n note
	if json.Unmarshal([]byte(payload), &n) != nil {
		return
	}
	if n.Kind == "outbox" {
		d.signal()
		return
	}
	if n.Origin == d.opts.ReplicaID || n.Topic == "" || d.store == nil {
		return
	}
	if !d.local.hasSubscribers(n.Topic) {
		return
	}
	if n.Topic == core.TopicRunMessage {
		msgs, err := d.reloadMessages(n)
		if err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				d.log.Debug("reload relayed run messages", "run_id", n.RunID, "seq", n.Seq, "end", n.End, "err", err)
			}
			return
		}
		for i := range msgs {
			d.local.Publish(d.ctx, n.Topic, &msgs[i])
		}
		return
	}
	v, err := d.reload(n)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			d.log.Debug("reload relayed update", "topic", n.Topic, "id", n.ID, "err", err)
		}
		return
	}
	d.local.Publish(d.ctx, n.Topic, v)
}

// reloadMessages loads the run messages Seq..End (Seq alone when End is
// zero) announced by another replica, in order, with one store read.
func (d *Durable) reloadMessages(n note) ([]core.RunMessage, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	end := max(n.End, n.Seq)
	msgs, err := d.store.ListRunMessages(ctx, n.RunID, n.Seq-1)
	if err != nil {
		return nil, err
	}
	out := msgs[:0]
	for _, m := range msgs {
		if m.Seq >= n.Seq && m.Seq <= end {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, core.ErrNotFound
	}
	return out, nil
}

func (d *Durable) reload(n note) (any, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	switch n.Topic {
	case core.TopicRunUpdated:
		return d.store.GetRun(ctx, n.ID)
	case core.TopicWorkItem:
		return d.store.GetWorkItem(ctx, n.ID)
	case core.TopicInbox:
		return d.store.GetInboxItem(ctx, n.ID)
	case core.TopicDefinition:
		return d.store.GetFactory(ctx, n.ID)
	}
	return nil, core.ErrNotFound
}

// liveNote describes a live payload by ID, or reports false for payloads
// that cannot be relayed (unknown topics or types).
func liveNote(topic string, payload any) (note, bool) {
	n := note{Topic: topic}
	switch v := payload.(type) {
	case *core.Run:
		n.ID = v.ID
	case core.Run:
		n.ID = v.ID
	case *core.WorkItem:
		n.ID = v.ID
	case core.WorkItem:
		n.ID = v.ID
	case *core.InboxItem:
		n.ID = v.ID
	case core.InboxItem:
		n.ID = v.ID
	case *core.Factory:
		n.ID = v.ID
	case core.Factory:
		n.ID = v.ID
	case *core.RunMessage:
		n.RunID, n.Seq = v.RunID, v.Seq
	case core.RunMessage:
		n.RunID, n.Seq = v.RunID, v.Seq
	default:
		return n, false
	}
	return n, n.ID != "" || (n.RunID != "" && n.Seq > 0)
}

// ---------- core.EventBus ----------

// Publish implements core.EventBus. Durable topics go to the outbox; live
// topics fan out locally and to the other replicas.
//
// Inside a store transaction (core.Transactor; see core.RunInTx) the outbox
// row is written in that transaction, and every in-process effect (local
// fan-out, notifications to other replicas, dispatcher wake-up) happens after
// the commit, or never on rollback. A failed outbox write inside a
// transaction dooms it (core.FailTx) instead of falling back to in-process
// delivery, so the state change it announces is rolled back too.
func (d *Durable) Publish(ctx context.Context, topic string, payload any) {
	if d.isClosed() {
		return
	}
	d.published.Add(1)
	if core.IsDurableTopic(topic) {
		id := durableID(payload)
		if err := d.Enqueue(ctx, topic, id, payload); err != nil {
			if core.FailTx(ctx, fmt.Errorf("write %s to the outbox: %w", topic, err)) {
				d.log.ErrorContext(ctx, "outbox write failed; rolling back the transaction", "topic", topic, "id", id, "err", err)
				return
			}
			d.fallbacks.Add(1)
			d.log.ErrorContext(ctx, "outbox write failed; delivering in process only", "topic", topic, "id", id, "err", err)
			d.deliverValue(context.WithoutCancel(ctx), topic, payload)
		}
		return
	}
	core.AfterCommit(ctx, func() {
		pctx := context.WithoutCancel(ctx)
		d.local.publish(pctx, topic, payload)
		d.origin.publish(pctx, topic, payload)
		if n, ok := liveNote(topic, payload); ok {
			d.notify(n)
		}
	})
}

// PublishRunMessages publishes a batch of run messages written in one
// transaction. Local subscribers of core.TopicRunMessage receive
// one *core.RunMessage per message, in order, as with Publish. The other
// replicas receive one notification per run with the range of sequence
// numbers and load the range with one store read. Inside a store
// transaction everything happens after the commit.
func (d *Durable) PublishRunMessages(ctx context.Context, msgs []*core.RunMessage) {
	if d.isClosed() || len(msgs) == 0 {
		return
	}
	d.published.Add(uint64(len(msgs)))
	core.AfterCommit(ctx, func() {
		pctx := context.WithoutCancel(ctx)
		var ranges []note
		for _, m := range msgs {
			d.local.publish(pctx, core.TopicRunMessage, m)
			d.origin.publish(pctx, core.TopicRunMessage, m)
			if m.RunID == "" || m.Seq <= 0 {
				continue
			}
			if k := len(ranges) - 1; k >= 0 && ranges[k].RunID == m.RunID && max(ranges[k].End, ranges[k].Seq)+1 == m.Seq {
				ranges[k].End = m.Seq
				continue
			}
			ranges = append(ranges, note{Topic: core.TopicRunMessage, RunID: m.RunID, Seq: m.Seq})
		}
		for _, n := range ranges {
			d.notify(n)
		}
	})
}

// TxAware reports that Publish joins a store transaction carried by the
// context (see Publish).
func (d *Durable) TxAware() bool { return true }

// durableID returns the deduplication key of a durable payload, assigning one
// to events that have none.
func durableID(payload any) string {
	switch v := payload.(type) {
	case *core.Event:
		if v.ID == "" {
			v.ID = core.NewID("evt")
		}
		return v.ID
	case core.Event:
		return v.ID
	}
	return ""
}

// Subscribe implements core.EventBus. For durable topics the channel receives
// each message delivered on this replica (at least once; the hand-off blocks
// up to SubscriberTimeout instead of dropping). For live topics and Wildcard
// it behaves like Bus.Subscribe.
func (d *Durable) Subscribe(topic string) (<-chan any, func()) {
	if !core.IsDurableTopic(topic) {
		return d.local.Subscribe(topic)
	}
	bufSize := d.opts.BufferSize
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	s := &chanSub{ch: make(chan any, bufSize)}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	if d.chans[topic] == nil {
		d.chans[topic] = map[*chanSub]struct{}{}
	}
	d.chans[topic][s] = struct{}{}
	d.mu.Unlock()
	d.signal()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.chans[topic], s)
			if len(d.chans[topic]) == 0 {
				delete(d.chans, topic)
			}
			d.mu.Unlock()
			s.close()
		})
	}
}

func (s *chanSub) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

func (s *chanSub) send(ctx context.Context, v any, timeout time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil // unsubscribed: nothing to deliver to
	}
	select {
	case s.ch <- v:
		return nil
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s.ch <- v:
		return nil
	case <-t.C:
		return errors.New("subscriber did not accept the message in time")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubscribeLocal is Subscribe for live topics restricted to publishes made on
// this replica. Consumers with side effects (e.g. the engine reacting to turn
// ends) use it so that one replica, the publisher's, acts on each update.
func (d *Durable) SubscribeLocal(topic string) (<-chan any, func()) {
	if core.IsDurableTopic(topic) {
		return d.Subscribe(topic)
	}
	return d.origin.Subscribe(topic)
}

// ---------- core.Queue ----------

// Enqueue implements core.Queue. Inside a store transaction the outbox row
// is written in that transaction (when the bus runs on the store's own
// coordinator) and dispatchers are woken after the commit.
func (d *Durable) Enqueue(ctx context.Context, topic, id string, payload any) error {
	var data []byte
	switch v := payload.(type) {
	case []byte:
		data = v
	case json.RawMessage:
		data = v
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: encode %s payload: %w", core.ErrInvalid, topic, err)
		}
		data = b
	}
	if id == "" {
		id = core.NewID("msg")
	}
	now := d.opts.Clock().UTC()
	m := &core.OutboxMessage{ID: id, Topic: topic, Payload: data, CreatedAt: now, NextAttemptAt: now,
		PartitionKey: core.OutboxPartitionKey(topic, payload)}
	if _, err := d.c.OutboxEnqueue(ctx, m); err != nil {
		return err
	}
	// Inside a transaction the row becomes visible on commit: wake the
	// dispatchers then.
	core.AfterCommit(ctx, func() {
		d.signal()
		d.notify(note{Kind: "outbox"})
	})
	return nil
}

// Consume implements core.Queue.
func (d *Durable) Consume(topic string, h core.JobHandler) func() {
	r := &handlerReg{h: h}
	d.mu.Lock()
	if d.handlers[topic] == nil {
		d.handlers[topic] = map[*handlerReg]struct{}{}
	}
	d.handlers[topic][r] = struct{}{}
	d.mu.Unlock()
	d.signal()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.handlers[topic], r)
			if len(d.handlers[topic]) == 0 {
				delete(d.handlers, topic)
			}
			d.mu.Unlock()
		})
	}
}

// ---------- dispatcher ----------

func (d *Durable) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Durable) isClosed() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.closed
}

// topics returns the topics with at least one consumer on this replica.
func (d *Durable) topics() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	for t, m := range d.handlers {
		if len(m) > 0 && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for t, m := range d.chans {
		if len(m) > 0 && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (d *Durable) dispatch() {
	defer d.wg.Done()
	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	lastPurge := time.Time{}
	for {
		full := d.drain()
		if d.opts.Retention > 0 && time.Since(lastPurge) > time.Hour {
			lastPurge = time.Now()
			if n, err := d.c.OutboxPurge(d.ctx, d.opts.Clock().Add(-d.opts.Retention)); err != nil && d.ctx.Err() == nil {
				d.log.Warn("purge delivered outbox messages", "err", err)
			} else if n > 0 {
				d.log.Debug("purged delivered outbox messages", "count", n)
			}
		}
		if full {
			if d.ctx.Err() != nil {
				return
			}
			continue
		}
		select {
		case <-d.ctx.Done():
			return
		case <-d.wake:
		case <-t.C:
		}
	}
}

// drain claims and delivers one batch; it reports whether the batch was full.
func (d *Durable) drain() bool {
	topics := d.topics()
	if len(topics) == 0 || d.ctx.Err() != nil {
		return false
	}
	msgs, err := d.c.OutboxClaim(d.ctx, topics, d.holder, d.opts.Clock().UTC(), d.opts.Visibility, d.opts.BatchSize)
	if err != nil {
		if d.ctx.Err() == nil {
			d.log.Warn("claim outbox messages", "err", err)
		}
		return false
	}
	sem := make(chan struct{}, d.opts.Concurrency)
	var wg sync.WaitGroup
	for _, m := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func(m core.OutboxMessage) {
			defer wg.Done()
			defer func() { <-sem }()
			d.process(m)
		}(m)
	}
	wg.Wait()
	return len(msgs) == d.opts.BatchSize
}

func (d *Durable) process(m core.OutboxMessage) {
	// Deliveries finish even while the bus is closing so acks are recorded.
	ctx := context.WithoutCancel(d.ctx)
	err := d.deliverJob(ctx, core.Job{ID: m.ID, Topic: m.Topic, Payload: m.Payload, Attempt: m.Attempts, EnqueuedAt: m.CreatedAt})
	now := d.opts.Clock().UTC()
	switch {
	case err == nil:
		d.delivered.Add(1)
		if aerr := d.c.OutboxAck(ctx, m.ID, d.holder, now); aerr != nil {
			d.log.Warn("ack outbox message; it may be delivered again", "id", m.ID, "topic", m.Topic, "err", aerr)
		}
	case m.Attempts >= d.opts.MaxAttempts:
		d.deadLetter.Add(1)
		d.log.Error("outbox message dead-lettered", "id", m.ID, "topic", m.Topic, "attempts", m.Attempts, "err", err)
		if derr := d.c.OutboxDeadLetter(ctx, m.ID, d.holder, err.Error()); derr != nil {
			d.log.Warn("dead-letter outbox message", "id", m.ID, "err", derr)
		}
	default:
		d.retried.Add(1)
		next := now.Add(d.backoff(m.Attempts))
		d.log.Warn("outbox delivery failed; will retry", "id", m.ID, "topic", m.Topic, "attempt", m.Attempts, "next", next, "err", err)
		if rerr := d.c.OutboxRetry(ctx, m.ID, d.holder, next, err.Error()); rerr != nil {
			d.log.Warn("schedule outbox retry", "id", m.ID, "err", rerr)
		}
	}
}

// backoff returns the delay after the given attempt: base·2^(attempt-1),
// capped, with ±20% jitter.
func (d *Durable) backoff(attempt int) time.Duration {
	delay := d.opts.BaseBackoff
	for i := 1; i < attempt && delay < d.opts.MaxBackoff; i++ {
		delay *= 2
	}
	if delay > d.opts.MaxBackoff {
		delay = d.opts.MaxBackoff
	}
	jitter := 0.8 + 0.4*rand.Float64() //nolint:gosec // G404: retry jitter needs no cryptographic randomness
	return time.Duration(float64(delay) * jitter)
}

// decode turns a durable payload into the type publishers used.
func decode(topic string, data []byte) (any, error) {
	switch topic {
	case core.TopicInboundEvent:
		v := &core.Event{}
		return v, json.Unmarshal(data, v)
	case core.TopicOutboundNotice:
		v := &core.OutboundMessage{}
		return v, json.Unmarshal(data, v)
	}
	return json.RawMessage(append([]byte(nil), data...)), nil
}

// deliverJob hands a job to every handler and channel subscriber of its
// topic on this replica; it fails when any of them fails.
func (d *Durable) deliverJob(ctx context.Context, job core.Job) error {
	d.mu.RLock()
	hs := make([]core.JobHandler, 0, len(d.handlers[job.Topic]))
	for r := range d.handlers[job.Topic] {
		hs = append(hs, r.h)
	}
	cs := make([]*chanSub, 0, len(d.chans[job.Topic]))
	for s := range d.chans[job.Topic] {
		cs = append(cs, s)
	}
	d.mu.RUnlock()

	var errs []error
	for _, h := range hs {
		if err := safeHandle(ctx, h, job); err != nil {
			errs = append(errs, err)
		}
	}
	if len(cs) > 0 || d.local.hasSubscribers(Wildcard) {
		v, err := decode(job.Topic, job.Payload)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("decode %s payload: %w", job.Topic, err))...)
		}
		for _, s := range cs {
			if err := s.send(d.ctx, v, d.opts.SubscriberTimeout); err != nil {
				errs = append(errs, err)
			}
		}
		d.local.Publish(ctx, job.Topic, v) // wildcard subscribers only
	}
	return errors.Join(errs...)
}

func safeHandle(ctx context.Context, h core.JobHandler, job core.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, job)
}

// deliverValue is the in-process fallback when the outbox cannot be written.
func (d *Durable) deliverValue(ctx context.Context, topic string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := d.deliverJob(ctx, core.Job{ID: durableID(payload), Topic: topic, Payload: data, Attempt: 1, EnqueuedAt: d.opts.Clock()}); err != nil {
		d.log.ErrorContext(ctx, "in-process delivery failed", "topic", topic, "err", err)
	}
}

// ---------- lifecycle & stats ----------

// Close stops the dispatcher (waiting for in-flight deliveries), closes every
// subscriber channel and stops listening. It is idempotent.
func (d *Durable) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
	if d.unlisten != nil {
		d.unlisten()
	}
	d.mu.Lock()
	for _, m := range d.chans {
		for s := range m {
			s.close()
		}
	}
	d.chans = map[string]map[*chanSub]struct{}{}
	d.handlers = map[string]map[*handlerReg]struct{}{}
	d.mu.Unlock()
	_ = d.origin.Close()
	return d.local.Close()
}

// DurableStats extends Stats with outbox delivery counters.
type DurableStats struct {
	Stats
	Delivered    uint64
	Retried      uint64
	DeadLettered uint64
	// Fallbacks counts durable publishes delivered in process because the
	// outbox write failed.
	Fallbacks uint64
}

// Stats returns bus counters (live fan-out plus durable publishes).
func (d *Durable) Stats() Stats {
	st := d.local.Stats()
	st.Published = d.published.Load()
	d.mu.RLock()
	for _, m := range d.chans {
		st.Subscribers += len(m)
	}
	d.mu.RUnlock()
	return st
}

// DurableStats returns Stats plus outbox delivery counters.
func (d *Durable) DurableStats() DurableStats {
	return DurableStats{Stats: d.Stats(), Delivered: d.delivered.Load(), Retried: d.retried.Load(),
		DeadLettered: d.deadLetter.Load(), Fallbacks: d.fallbacks.Load()}
}

// Dropped returns the number of live deliveries dropped for slow subscribers.
func (d *Durable) Dropped() uint64 { return d.local.Dropped() }

// Coordinator returns the coordinator the bus runs on.
func (d *Durable) Coordinator() core.Coordinator { return d.c }
