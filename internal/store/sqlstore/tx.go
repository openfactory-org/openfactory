package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.Transactor = (*Store)(nil)

// sqlTx is the transaction carried by a context inside InTx.
type sqlTx struct {
	s     *Store
	tx    *sql.Tx
	scope *core.TxScope
	done  atomic.Bool

	mu sync.Mutex // serializes savepoints
	sp int
}

type txKey struct{}

// txFrom returns this store's active transaction carried by ctx, or nil. A
// context that outlives its transaction (e.g. captured by an after-commit
// hook) falls back to the connection pool.
func (s *Store) txFrom(ctx context.Context) *sqlTx {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(txKey{}).(*sqlTx)
	if t == nil || t.s != s || t.done.Load() {
		return nil
	}
	return t
}

// conn returns the querier for ctx: its transaction, or the pool.
func (s *Store) conn(ctx context.Context) querier {
	if t := s.txFrom(ctx); t != nil {
		if s.d.TxSavepoints {
			return txQuerier{t}
		}
		return t.tx
	}
	return s.db
}

// txQuerier runs each write statement of a transaction in its own savepoint
// (Dialect.TxSavepoints), so a failed statement (e.g. a unique violation the
// caller handles) returns its error without aborting the transaction, as on
// SQLite.
type txQuerier struct{ t *sqlTx }

func (q txQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := q.t.savepoint(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = tx.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

func (q txQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.t.tx.QueryContext(ctx, query, args...)
}

func (q txQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.t.tx.QueryRowContext(ctx, query, args...)
}

// InTx implements core.Transactor. Every Store and Coordinator method called
// with the context handed to fn runs in the transaction (outbox writes
// included). A call made while ctx already carries a transaction of this
// store joins it; its error dooms the outer transaction. After-commit hooks
// (core.AfterCommit) run once the outermost transaction commits.
//
// A failed statement returns its error and the transaction continues (on
// PostgreSQL each write runs in a savepoint, see Dialect.TxSavepoints).
// Notifications are sent after commit by the hooks, so other replicas never
// observe uncommitted rows. A transaction must not be used from several
// goroutines at once.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if t := s.txFrom(ctx); t != nil {
		if err := fn(ctx); err != nil {
			t.scope.Fail(err)
			return err
		}
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.mapErr(err)
	}
	tctx, scope := core.WithTxScope(ctx)
	t := &sqlTx{s: s, tx: tx, scope: scope}
	tctx = context.WithValue(tctx, txKey{}, t)
	committed := false
	defer func() {
		t.done.Store(true)
		if !committed {
			_ = tx.Rollback()
		}
		scope.Finish(committed)
	}()
	if err := fn(tctx); err != nil {
		return err
	}
	if err := scope.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return s.mapErr(err)
	}
	committed = true
	return nil
}

// tx runs fn in a transaction. Inside InTx it joins the caller's transaction
// through a savepoint, so a failing multi-statement method leaves no partial
// writes behind while the outer transaction continues.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if t := s.txFrom(ctx); t != nil {
		return t.savepoint(ctx, fn)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.mapErr(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.mapErr(tx.Commit())
}

func (t *sqlTx) savepoint(ctx context.Context, fn func(tx *sql.Tx) error) error {
	t.mu.Lock()
	t.sp++
	name := fmt.Sprintf("of_sp_%d", t.sp)
	t.mu.Unlock()
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return t.s.mapErr(err)
	}
	if err := fn(t.tx); err != nil {
		if _, rerr := t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); rerr != nil {
			t.scope.Fail(t.s.mapErr(rerr))
		}
		return err
	}
	if _, err := t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return t.s.mapErr(err)
	}
	return nil
}
