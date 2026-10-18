package coord

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// Memory is a process-local core.Coordinator. It is correct for exactly one
// replica: From wraps stores that do not implement core.Coordinator with it.
type Memory struct {
	mu      sync.Mutex
	outbox  map[string]*core.OutboxMessage
	claims  map[string]outboxClaim
	ord     map[string]int64
	seq     int64
	leaders map[string]holderRec
	locks   map[string]holderRec

	*Broadcaster
}

type outboxClaim struct {
	holder string
	until  time.Time
}

type holderRec struct {
	holder  string
	expires time.Time
}

var _ core.Coordinator = (*Memory)(nil)

// NewMemory returns an empty in-memory coordinator.
func NewMemory() *Memory {
	return &Memory{
		outbox:      map[string]*core.OutboxMessage{},
		claims:      map[string]outboxClaim{},
		ord:         map[string]int64{},
		leaders:     map[string]holderRec{},
		locks:       map[string]holderRec{},
		Broadcaster: NewBroadcaster(),
	}
}

func cloneMsg(m *core.OutboxMessage) core.OutboxMessage {
	c := *m
	c.Payload = append([]byte(nil), m.Payload...)
	if m.DeliveredAt != nil {
		t := *m.DeliveredAt
		c.DeliveredAt = &t
	}
	return c
}

// ---------- outbox ----------

func (c *Memory) OutboxEnqueue(_ context.Context, m *core.OutboxMessage) (bool, error) {
	if m.ID == "" || m.Topic == "" {
		return false, fmt.Errorf("%w: outbox message needs id and topic", core.ErrInvalid)
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.NextAttemptAt.IsZero() {
		m.NextAttemptAt = m.CreatedAt
	}
	if m.State == "" {
		m.State = core.OutboxPending
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.outbox[m.ID]; ok {
		return false, nil
	}
	cp := cloneMsg(m)
	c.outbox[m.ID] = &cp
	c.seq++
	c.ord[m.ID] = c.seq
	return true, nil
}

func (c *Memory) sorted(filter func(*core.OutboxMessage) bool) []*core.OutboxMessage {
	var out []*core.OutboxMessage
	for _, m := range c.outbox {
		if filter(m) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return c.ord[out[i].ID] < c.ord[out[j].ID]
	})
	return out
}

func (c *Memory) OutboxClaim(_ context.Context, topics []string, holder string, now time.Time, visibility time.Duration, limit int) ([]core.OutboxMessage, error) {
	if limit <= 0 {
		limit = 1
	}
	want := map[string]bool{}
	for _, t := range topics {
		want[t] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The first pending message of each partition key, in insert order, is
	// the head of its key. Only heads can be claimed.
	head := map[string]string{}
	for id, m := range c.outbox {
		if m.State != core.OutboxPending || m.PartitionKey == "" {
			continue
		}
		if h, ok := head[m.PartitionKey]; !ok || c.ord[id] < c.ord[h] {
			head[m.PartitionKey] = id
		}
	}
	cands := c.sorted(func(m *core.OutboxMessage) bool {
		if m.State != core.OutboxPending || !want[m.Topic] || m.NextAttemptAt.After(now) {
			return false
		}
		if m.PartitionKey != "" && head[m.PartitionKey] != m.ID {
			return false
		}
		cl, ok := c.claims[m.ID]
		return !ok || !cl.until.After(now)
	})
	var out []core.OutboxMessage
	for _, m := range cands {
		if len(out) == limit {
			break
		}
		m.Attempts++
		c.claims[m.ID] = outboxClaim{holder: holder, until: now.Add(visibility)}
		out = append(out, cloneMsg(m))
	}
	return out, nil
}

func (c *Memory) claimed(id, holder string) (*core.OutboxMessage, error) {
	m, ok := c.outbox[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	if cl, ok := c.claims[id]; !ok || cl.holder != holder || m.State != core.OutboxPending {
		return nil, fmt.Errorf("%w: outbox message %s is not claimed by %s", core.ErrConflict, id, holder)
	}
	return m, nil
}

func (c *Memory) OutboxAck(_ context.Context, id, holder string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.claimed(id, holder)
	if err != nil {
		return err
	}
	t := now.UTC()
	m.State, m.DeliveredAt, m.LastError = core.OutboxDelivered, &t, ""
	delete(c.claims, id)
	return nil
}

func (c *Memory) OutboxRetry(_ context.Context, id, holder string, next time.Time, lastErr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.claimed(id, holder)
	if err != nil {
		return err
	}
	m.NextAttemptAt, m.LastError = next.UTC(), lastErr
	delete(c.claims, id)
	return nil
}

func (c *Memory) OutboxDeadLetter(_ context.Context, id, holder string, lastErr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.claimed(id, holder)
	if err != nil {
		return err
	}
	m.State, m.LastError = core.OutboxDead, lastErr
	delete(c.claims, id)
	return nil
}

func (c *Memory) OutboxList(_ context.Context, state core.OutboxState, limit int) ([]core.OutboxMessage, error) {
	limit = storeutil.Limit(limit)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []core.OutboxMessage{}
	for _, m := range c.sorted(func(m *core.OutboxMessage) bool { return m.State == state }) {
		if len(out) == limit {
			break
		}
		out = append(out, cloneMsg(m))
	}
	return out, nil
}

func (c *Memory) OutboxRedrive(_ context.Context, id string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.outbox[id]
	if !ok || m.State != core.OutboxDead {
		return core.ErrNotFound
	}
	m.State, m.Attempts, m.NextAttemptAt = core.OutboxPending, 0, now.UTC()
	delete(c.claims, id)
	return nil
}

func (c *Memory) OutboxPurge(_ context.Context, before time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, m := range c.outbox {
		if m.State == core.OutboxDelivered && m.DeliveredAt != nil && m.DeliveredAt.Before(before) {
			delete(c.outbox, id)
			delete(c.ord, id)
			n++
		}
	}
	return n, nil
}

// ---------- leader & locks ----------

func acquire(m map[string]holderRec, name, holder string, now, expires time.Time) bool {
	cur, ok := m[name]
	if ok && cur.holder != holder && cur.expires.After(now) {
		return false
	}
	m[name] = holderRec{holder: holder, expires: expires}
	return true
}

func (c *Memory) AcquireLeadership(_ context.Context, name, holder string, now, expires time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return acquire(c.leaders, name, holder, now, expires), nil
}

func (c *Memory) ReleaseLeadership(_ context.Context, name, holder string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.leaders[name]; ok && cur.holder == holder {
		delete(c.leaders, name)
	}
	return nil
}

func (c *Memory) TryLock(_ context.Context, key, holder string, now, expires time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return acquire(c.locks, key, holder, now, expires), nil
}

func (c *Memory) Unlock(_ context.Context, key, holder string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.locks[key]; ok && cur.holder == holder {
		delete(c.locks, key)
	}
	return nil
}
