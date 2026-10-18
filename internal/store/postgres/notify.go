package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openfactory-org/openfactory/internal/store/coord"
)

// NotifyChannel is the LISTEN/NOTIFY channel shared by every replica.
const NotifyChannel = "openfactory_events"

// maxNotifyPayload is PostgreSQL's NOTIFY payload limit (8000 bytes) minus a margin.
const maxNotifyPayload = 7900

// notifier implements sqlstore.Notifier with LISTEN/NOTIFY. One pooled
// connection per process LISTENs; it reconnects with backoff after errors
// (notifications sent while reconnecting are lost: they are best effort).
type notifier struct {
	db   *sql.DB
	pool *pgxpool.Pool
	b    *coord.Broadcaster

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
}

func newNotifier(db *sql.DB, pool *pgxpool.Pool) *notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &notifier{db: db, pool: pool, b: coord.NewBroadcaster(), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (n *notifier) Notify(ctx context.Context, payload string) error {
	if len(payload) > maxNotifyPayload {
		return fmt.Errorf("postgres: notification payload of %d bytes exceeds %d", len(payload), maxNotifyPayload)
	}
	_, err := n.db.ExecContext(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, payload)
	return err
}

func (n *notifier) Listen(fn func(payload string)) func() {
	n.once.Do(func() { go n.loop() })
	return n.b.Listen(fn)
}

func (n *notifier) close() {
	n.cancel()
	n.once.Do(func() { close(n.done) })
	<-n.done
}

func (n *notifier) loop() {
	defer close(n.done)
	backoff := 100 * time.Millisecond
	for n.ctx.Err() == nil {
		err := n.listen()
		if n.ctx.Err() != nil {
			return
		}
		slog.Warn("postgres: LISTEN connection lost; reconnecting", "err", err, "in", backoff)
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}

func (n *notifier) listen() error {
	pc, err := n.pool.Acquire(n.ctx)
	if err != nil {
		return err
	}
	// The connection's session state (LISTEN) must not return to the pool.
	conn := pc.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(n.ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	for {
		msg, err := conn.WaitForNotification(n.ctx)
		if err != nil {
			return err
		}
		_ = n.b.Notify(n.ctx, msg.Payload)
	}
}
