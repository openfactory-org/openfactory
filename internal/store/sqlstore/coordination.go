package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/coord"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

var _ core.Coordinator = (*Store)(nil)

// Notifier carries core.Coordinator notifications. PostgreSQL supplies one
// built on LISTEN/NOTIFY; the default is an in-process coord.Broadcaster
// (enough for SQLite, which runs a single replica).
type Notifier interface {
	Notify(ctx context.Context, payload string) error
	Listen(fn func(payload string)) (cancel func())
}

// SetNotifier replaces the notification transport. Call it before use.
func (s *Store) SetNotifier(n Notifier) { s.notifier = n }

func (s *Store) notify() Notifier {
	s.notifyOnce.Do(func() {
		if s.notifier == nil {
			s.notifier = coord.NewBroadcaster()
		}
	})
	return s.notifier
}

// Notify implements core.Coordinator.
func (s *Store) Notify(ctx context.Context, payload string) error {
	return s.notify().Notify(ctx, payload)
}

// Listen implements core.Coordinator.
func (s *Store) Listen(fn func(payload string)) func() { return s.notify().Listen(fn) }

func ns(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func (s *Store) rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	return n, s.mapErr(err)
}

// ---------- outbox ----------

const outboxCols = "id, topic, payload, state, attempts, next_ns, last_error, created_ns, delivered_ns, partition_key"

func scanOutbox(rows interface{ Scan(...any) error }) (core.OutboxMessage, error) {
	var m core.OutboxMessage
	var payload, state string
	var next, created, delivered int64
	if err := rows.Scan(&m.ID, &m.Topic, &payload, &state, &m.Attempts, &next, &m.LastError, &created, &delivered, &m.PartitionKey); err != nil {
		return m, err
	}
	m.Payload, m.State = []byte(payload), core.OutboxState(state)
	m.NextAttemptAt, m.CreatedAt = fromNS(next), fromNS(created)
	if delivered != 0 {
		t := fromNS(delivered)
		m.DeliveredAt = &t
	}
	return m, nil
}

func (s *Store) queryOutbox(ctx context.Context, q querier, query string, args ...any) ([]core.OutboxMessage, error) {
	rows, err := q.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.OutboxMessage{}
	for rows.Next() {
		m, err := scanOutbox(rows)
		if err != nil {
			return nil, s.mapErr(err)
		}
		out = append(out, m)
	}
	return out, s.mapErr(rows.Err())
}

// OutboxEnqueue implements core.Coordinator.
func (s *Store) OutboxEnqueue(ctx context.Context, m *core.OutboxMessage) (bool, error) {
	if m.ID == "" || m.Topic == "" {
		return false, fmt.Errorf("%w: outbox message needs id and topic", core.ErrInvalid)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now()
	}
	if m.NextAttemptAt.IsZero() {
		m.NextAttemptAt = m.CreatedAt
	}
	if m.State == "" {
		m.State = core.OutboxPending
	}
	res, err := s.exec(ctx, s.conn(ctx), `INSERT INTO outbox (id, topic, payload, state, attempts, next_ns, created_ns, partition_key)
VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (id) DO NOTHING`,
		m.ID, m.Topic, string(m.Payload), string(m.State), m.Attempts, ns(m.NextAttemptAt), ns(m.CreatedAt), m.PartitionKey)
	if err != nil {
		return false, err
	}
	n, err := s.rowsAffected(res)
	return n > 0, err
}

// OutboxClaim implements core.Coordinator. A row with a partition key is a
// candidate only when no row with the same key and a lower insert order
// (Dialect.OutboxSeq) is pending, claimed or not. A row that waits for a retry
// is still pending, so it stays at the head of its key. Dead and delivered
// rows do not block.
func (s *Store) OutboxClaim(ctx context.Context, topics []string, holder string, at time.Time, visibility time.Duration, limit int) ([]core.OutboxMessage, error) {
	if len(topics) == 0 {
		return []core.OutboxMessage{}, nil
	}
	if limit <= 0 {
		limit = 1
	}
	seq := s.d.OutboxSeq
	if seq == "" {
		seq = "created_ns"
	}
	w := &where{}
	w.add("o.state = 'pending'")
	w.in("o.topic", topics)
	w.add("o.next_ns <= ?", ns(at))
	w.add("o.claimed_until_ns <= ?", ns(at))
	w.add("(o.partition_key = '' OR NOT EXISTS (SELECT 1 FROM outbox p WHERE p.partition_key = o.partition_key" +
		" AND p.state = 'pending' AND p." + seq + " < o." + seq + "))")
	args := append([]any{holder, ns(at.Add(visibility))}, w.args...)
	query := "UPDATE outbox SET claimed_by = ?, claimed_until_ns = ?, attempts = attempts + 1 WHERE id IN (" +
		"SELECT o.id FROM outbox o" + w.sql() + " ORDER BY o.created_ns, o.id LIMIT " + strconv.Itoa(limit) + s.d.SkipLocked +
		") RETURNING " + outboxCols
	var out []core.OutboxMessage
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.queryOutbox(ctx, tx, query, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// finishOutbox applies a state change to a message claimed by holder.
func (s *Store) finishOutbox(ctx context.Context, id, holder, set string, args ...any) error {
	args = append(args, id, holder)
	res, err := s.exec(ctx, s.conn(ctx), "UPDATE outbox SET "+set+", claimed_by = '', claimed_until_ns = 0 WHERE id = ? AND claimed_by = ? AND state = 'pending'", args...)
	if err != nil {
		return err
	}
	n, err := s.rowsAffected(res)
	if err != nil {
		return err
	}
	if n == 0 {
		var one int
		if err := s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT 1 FROM outbox WHERE id = ?"), id).Scan(&one); err != nil {
			return s.mapErr(err)
		}
		return fmt.Errorf("%w: outbox message %s is not claimed by %s", core.ErrConflict, id, holder)
	}
	return nil
}

// OutboxAck implements core.Coordinator.
func (s *Store) OutboxAck(ctx context.Context, id, holder string, at time.Time) error {
	return s.finishOutbox(ctx, id, holder, "state = 'delivered', delivered_ns = ?, last_error = ''", ns(at))
}

// OutboxRetry implements core.Coordinator.
func (s *Store) OutboxRetry(ctx context.Context, id, holder string, next time.Time, lastErr string) error {
	return s.finishOutbox(ctx, id, holder, "next_ns = ?, last_error = ?", ns(next), lastErr)
}

// OutboxDeadLetter implements core.Coordinator.
func (s *Store) OutboxDeadLetter(ctx context.Context, id, holder string, lastErr string) error {
	return s.finishOutbox(ctx, id, holder, "state = 'dead', last_error = ?", lastErr)
}

// OutboxList implements core.Coordinator.
func (s *Store) OutboxList(ctx context.Context, state core.OutboxState, limit int) ([]core.OutboxMessage, error) {
	return s.queryOutbox(ctx, s.conn(ctx), "SELECT "+outboxCols+" FROM outbox WHERE state = ? ORDER BY created_ns, id LIMIT "+
		strconv.Itoa(storeutil.Limit(limit)), string(state))
}

// OutboxRedrive implements core.Coordinator.
func (s *Store) OutboxRedrive(ctx context.Context, id string, at time.Time) error {
	return s.execOne(ctx, s.conn(ctx), "UPDATE outbox SET state = 'pending', attempts = 0, next_ns = ?, claimed_by = '', claimed_until_ns = 0 WHERE id = ? AND state = 'dead'", ns(at), id)
}

// OutboxPurge implements core.Coordinator.
func (s *Store) OutboxPurge(ctx context.Context, before time.Time) (int, error) {
	res, err := s.exec(ctx, s.conn(ctx), "DELETE FROM outbox WHERE state = 'delivered' AND delivered_ns < ?", ns(before))
	if err != nil {
		return 0, err
	}
	n, err := s.rowsAffected(res)
	return int(n), err
}

// ---------- leader lease & locks ----------

func (s *Store) acquireRow(ctx context.Context, table, keyCol, key, holder string, at, expires time.Time) (bool, error) {
	res, err := s.exec(ctx, s.conn(ctx), "INSERT INTO "+table+" ("+keyCol+", holder, expires_ns) VALUES (?,?,?) ON CONFLICT ("+keyCol+
		") DO UPDATE SET holder = excluded.holder, expires_ns = excluded.expires_ns WHERE "+table+".holder = excluded.holder OR "+table+".expires_ns <= ?",
		key, holder, ns(expires), ns(at))
	if err != nil {
		return false, err
	}
	n, err := s.rowsAffected(res)
	return n > 0, err
}

// AcquireLeadership implements core.Coordinator.
func (s *Store) AcquireLeadership(ctx context.Context, name, holder string, at, expires time.Time) (bool, error) {
	return s.acquireRow(ctx, "leader_leases", "name", name, holder, at, expires)
}

// ReleaseLeadership implements core.Coordinator.
func (s *Store) ReleaseLeadership(ctx context.Context, name, holder string) error {
	_, err := s.exec(ctx, s.conn(ctx), "DELETE FROM leader_leases WHERE name = ? AND holder = ?", name, holder)
	return err
}

// TryLock implements core.Coordinator.
func (s *Store) TryLock(ctx context.Context, key, holder string, at, expires time.Time) (bool, error) {
	return s.acquireRow(ctx, "coord_locks", "lock_key", key, holder, at, expires)
}

// Unlock implements core.Coordinator.
func (s *Store) Unlock(ctx context.Context, key, holder string) error {
	_, err := s.exec(ctx, s.conn(ctx), "DELETE FROM coord_locks WHERE lock_key = ? AND holder = ?", key, holder)
	return err
}
