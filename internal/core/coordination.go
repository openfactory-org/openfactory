package core

import (
	"context"
	"time"
)

// ---------- Delivery contracts ----------
//
// The database is the only source of truth. Two kinds of topics exist:
//
//   - Durable topics (TopicInboundEvent, TopicOutboundNotice) are written to an
//     outbox table and delivered AT LEAST ONCE. A delivery that fails is retried
//     with exponential backoff and moved to the dead-letter state after a
//     bounded number of attempts. Each message is processed by one replica.
//     Consumers MUST be idempotent, keyed by the message ID (core.Event.ID for
//     inbound events). Publishing the same ID twice delivers it once.
//   - Live topics (run.updated, run.message, workitem.updated, inbox.updated,
//     definition.applied) are lossy fan-out notifications that reach every
//     replica (Postgres LISTEN/NOTIFY; in-process for SQLite and memory). A
//     consumer that must not miss a change re-reads the store; payloads received
//     from another replica are reloaded from the store, so they show the latest
//     state, not the exact snapshot that was published.

// DurableTopics lists the topics delivered through the outbox.
var DurableTopics = []string{TopicInboundEvent, TopicOutboundNotice}

// PartitionKeyer is implemented by durable payloads that name their own
// outbox partition key (see OutboxMessage.PartitionKey).
type PartitionKeyer interface {
	OutboxPartitionKey() string
}

// OutboxPartitionKey returns the partition key of a durable payload. The
// outbox delivers the messages of one key in order, one at a time.
//
//   - An inbound event uses its team and the key of its source (the issue,
//     pull request or thread), so a review comment and the merge of the same
//     pull request are handled in the order they arrived.
//   - An outbound notice uses its work item, or its source when it has no
//     work item.
//   - Other payloads use PartitionKeyer, or have no key and no order.
//
// The key starts with the topic, so a slow outbound notice never holds back
// inbound events of the same entity.
func OutboxPartitionKey(topic string, payload any) string {
	var key string
	switch v := payload.(type) {
	case PartitionKeyer:
		key = v.OutboxPartitionKey()
	case *Event:
		key = eventPartitionKey(v)
	case Event:
		key = eventPartitionKey(&v)
	case *OutboundMessage:
		key = noticePartitionKey(v)
	case OutboundMessage:
		key = noticePartitionKey(&v)
	}
	if key == "" {
		return ""
	}
	return topic + "|" + key
}

func eventPartitionKey(e *Event) string {
	if e == nil {
		return ""
	}
	if k := e.Source.Key(); k != "" {
		return "team:" + e.TeamID + "|" + k
	}
	return ""
}

func noticePartitionKey(m *OutboundMessage) string {
	switch {
	case m == nil:
		return ""
	case m.WorkItemID != "":
		return "wi:" + m.WorkItemID
	case m.Source.Key() != "":
		return "source:" + m.Source.Key()
	}
	return ""
}

// IsDurableTopic reports whether topic is delivered through the outbox.
func IsDurableTopic(topic string) bool {
	for _, t := range DurableTopics {
		if t == topic {
			return true
		}
	}
	return false
}

// Job is one durable message handed to a JobHandler.
type Job struct {
	ID         string
	Topic      string
	Payload    []byte // JSON
	Attempt    int    // 1 for the first delivery
	EnqueuedAt time.Time
}

// JobHandler processes a job. Returning nil acknowledges it; an error schedules
// a retry (or dead-letters the job after the last attempt). Handlers must be
// idempotent: the same job can be delivered more than once.
type JobHandler func(ctx context.Context, job Job) error

// Queue is a durable, at-least-once work queue backed by the database.
type Queue interface {
	// Enqueue records a job. An empty id gets a generated one; a job whose id
	// already exists is ignored (deduplication). payload is JSON-encoded unless
	// it is already a []byte or json.RawMessage.
	Enqueue(ctx context.Context, topic, id string, payload any) error
	// Consume registers h for topic on this replica. Every job is handed to one
	// replica; on that replica every handler of the topic must succeed for the
	// job to be acknowledged. stop unregisters the handler.
	Consume(topic string, h JobHandler) (stop func())
}

// Leadership reports whether this replica currently holds the leader lease.
type Leadership interface {
	IsLeader() bool
}

// ---------- Coordinator ----------

// OutboxState is the delivery state of an outbox message.
type OutboxState string

const (
	OutboxPending   OutboxState = "pending"
	OutboxDelivered OutboxState = "delivered"
	OutboxDead      OutboxState = "dead"
)

// OutboxMessage is one row of the outbox.
type OutboxMessage struct {
	ID            string      `json:"id"`
	Topic         string      `json:"topic"`
	Payload       []byte      `json:"payload"`
	State         OutboxState `json:"state"`
	Attempts      int         `json:"attempts"`
	NextAttemptAt time.Time   `json:"next_attempt_at"`
	LastError     string      `json:"last_error,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	DeliveredAt   *time.Time  `json:"delivered_at,omitempty"`
	// PartitionKey orders the messages of one entity. OutboxClaim returns a
	// message only when no earlier message (in insert order) with the same
	// key is pending or claimed. A dead message does not block later
	// messages. An empty key has no order.
	PartitionKey string `json:"partition_key,omitempty"`
}

// Coordinator holds the database primitives that make several replicas safe:
// the outbox, live notifications, the leader lease and distributed locks.
// Stores implement it next to core.Store;
// callers get it with a type assertion (see internal/store/coord.From).
//
// Times are passed in by callers (one clock per replica); keep replica clocks
// in sync (NTP) to well within the lease TTLs.
type Coordinator interface {
	// OutboxEnqueue inserts m (ID, Topic, Payload required; NextAttemptAt and
	// CreatedAt default to now). It reports false when the ID already exists.
	OutboxEnqueue(ctx context.Context, m *OutboxMessage) (bool, error)
	// OutboxClaim locks up to limit pending messages of the given topics that
	// are due at now and not claimed by anyone else, for visibility. Attempts is
	// incremented on every claim. A message with a PartitionKey is claimed
	// only when it is the first pending message of its key, so at most one
	// message of each key is in flight, and a message that waits for a retry
	// stays at the head of its key.
	OutboxClaim(ctx context.Context, topics []string, holder string, now time.Time, visibility time.Duration, limit int) ([]OutboxMessage, error)
	// OutboxAck marks a claimed message delivered.
	OutboxAck(ctx context.Context, id, holder string, now time.Time) error
	// OutboxRetry releases a claimed message for another attempt at next.
	OutboxRetry(ctx context.Context, id, holder string, next time.Time, lastErr string) error
	// OutboxDeadLetter moves a claimed message to the dead state.
	OutboxDeadLetter(ctx context.Context, id, holder string, lastErr string) error
	// OutboxList lists messages in a state, oldest first.
	OutboxList(ctx context.Context, state OutboxState, limit int) ([]OutboxMessage, error)
	// OutboxRedrive moves a dead message back to pending with zero attempts.
	OutboxRedrive(ctx context.Context, id string, now time.Time) error
	// OutboxPurge deletes delivered messages delivered before t.
	OutboxPurge(ctx context.Context, before time.Time) (int, error)

	// Notify sends a small (≤ 7900 bytes) payload to every Listen callback on
	// every replica, including this one. Delivery is best effort.
	Notify(ctx context.Context, payload string) error
	// Listen registers fn for notifications; cancel unregisters it.
	Listen(fn func(payload string)) (cancel func())

	// AcquireLeadership takes or renews the named leader lease for holder until
	// expires; it succeeds when the lease is free, expired at now, or already
	// held by holder.
	AcquireLeadership(ctx context.Context, name, holder string, now, expires time.Time) (bool, error)
	// ReleaseLeadership gives the lease up when holder holds it.
	ReleaseLeadership(ctx context.Context, name, holder string) error

	// TryLock takes or renews the named lock like AcquireLeadership.
	TryLock(ctx context.Context, key, holder string, now, expires time.Time) (bool, error)
	// Unlock releases the lock when holder holds it.
	Unlock(ctx context.Context, key, holder string) error
}
