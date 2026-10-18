package core

import (
	"context"
	"sync"
)

// ---------- Transactions (transactional outbox) ----------
//
// A store that implements Transactor runs several writes atomically. Every
// store and Coordinator method called with the context handed to fn joins the
// transaction, including Coordinator.OutboxEnqueue, so a state change and the
// outbox message that announces it commit (or roll back) together.
//
// Effects that must not be visible before the commit (in-process delivery,
// live notifications, dispatcher wake-ups) are registered with AfterCommit;
// they run once the outermost transaction commits and are dropped on
// rollback.
//
// Rules for code running inside fn:
//   - Use only the context passed to fn (or contexts derived from it) for
//     store calls. A call made with an unrelated context does not join the
//     transaction and, on single-connection databases (SQLite :memory:), waits for the transaction to finish: a deadlock.
//   - Do not take distributed locks (they write through their own context);
//     take them before starting the transaction.
//   - Keep transactions short: SQLite serializes writers.

// Transactor is implemented by stores that can run several writes in one
// database transaction. It is an optional capability: callers type-assert
// the store and fall back to plain calls when it is missing (see RunInTx).
type Transactor interface {
	// InTx runs fn in a transaction. The transaction commits when fn returns
	// nil and nothing called FailTx; otherwise it rolls back and InTx returns
	// the error. A call made while ctx already carries a transaction of the
	// same store joins it (no nesting); an error from a joined call dooms the
	// outer transaction.
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// TxScope is the store-independent state of one transaction: the effects to
// run after commit and a failure recorded by participants that cannot return
// an error (e.g. core.EventBus.Publish). Transactor implementations create one
// per outermost transaction with WithTxScope.
type TxScope struct {
	mu    sync.Mutex
	hooks []func()
	err   error
	done  bool
}

type txScopeKey struct{}

// WithTxScope returns ctx carrying a new TxScope.
func WithTxScope(ctx context.Context) (context.Context, *TxScope) {
	s := &TxScope{}
	return context.WithValue(ctx, txScopeKey{}, s), s
}

// TxScopeFrom returns the active (not yet finished) TxScope carried by ctx,
// or nil.
func TxScopeFrom(ctx context.Context) *TxScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(txScopeKey{}).(*TxScope)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	return s
}

// InTransaction reports whether ctx carries an active transaction.
func InTransaction(ctx context.Context) bool { return TxScopeFrom(ctx) != nil }

// Fail dooms the transaction: it will roll back and InTx returns err. The
// first error wins.
func (s *TxScope) Fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// Err returns the error recorded by Fail.
func (s *TxScope) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// OnCommit registers fn to run after the transaction commits. When the scope
// already finished, fn runs immediately.
func (s *TxScope) OnCommit(fn func()) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		fn()
		return
	}
	s.hooks = append(s.hooks, fn)
	s.mu.Unlock()
}

// Finish ends the scope. When committed is true the after-commit hooks run in
// registration order (a panicking hook does not stop the others); otherwise
// they are dropped. Transactor implementations call it exactly once, after
// Commit or Rollback.
func (s *TxScope) Finish(committed bool) {
	s.mu.Lock()
	hooks := s.hooks
	s.hooks, s.done = nil, true
	s.mu.Unlock()
	if !committed {
		return
	}
	for _, h := range hooks {
		runHook(h)
	}
}

func runHook(h func()) {
	defer func() { _ = recover() }()
	h()
}

// AfterCommit runs fn after the transaction carried by ctx commits, or now
// when ctx carries none.
func AfterCommit(ctx context.Context, fn func()) {
	if s := TxScopeFrom(ctx); s != nil {
		s.OnCommit(fn)
		return
	}
	fn()
}

// FailTx dooms the transaction carried by ctx (see TxScope.Fail). It reports
// false when ctx carries none.
func FailTx(ctx context.Context, err error) bool {
	s := TxScopeFrom(ctx)
	if s == nil {
		return false
	}
	s.Fail(err)
	return true
}

// RunInTx runs fn in a transaction of store when it implements Transactor,
// and calls fn(ctx) directly otherwise (each write then commits on its own,
// and AfterCommit effects run immediately).
func RunInTx(ctx context.Context, store any, fn func(ctx context.Context) error) error {
	if t, ok := store.(Transactor); ok {
		return t.InTx(ctx, fn)
	}
	return fn(ctx)
}
