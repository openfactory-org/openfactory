package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// txCases run when the store implements core.Transactor and core.Coordinator.
var txCases = []struct {
	name string
	fn   func(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator)
}{
	{"Commit", testTxCommit},
	{"Rollback", testTxRollback},
	{"FailTx", testTxFailTx},
	{"NestedJoins", testTxNested},
	{"NestedErrorDoomsOuter", testTxNestedError},
	{"FailedStatementContinues", testTxFailedStatement},
	{"Panic", testTxPanic},
	{"ContextOutlivesTx", testTxContextOutlives},
	{"Concurrent", testTxConcurrent},
}

func runTx(t *testing.T, newStore func(t *testing.T) core.Store) {
	for _, tc := range txCases {
		t.Run("Tx/"+tc.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			tx, isTx := s.(core.Transactor)
			c, isCoord := s.(core.Coordinator)
			if !isTx || !isCoord {
				t.Skip("store does not implement core.Transactor and core.Coordinator")
			}
			tc.fn(t, s, tx, c)
		})
	}
}

// txWrites creates a run, a KV entry and an outbox message with ctx.
func txWrites(t *testing.T, ctx context.Context, s core.Store, c core.Coordinator, runID, key, msgID, topic string) {
	t.Helper()
	ok(t, s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: runID}))
	ok(t, s.KVSet(ctx, key, "v"))
	inserted, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: msgID, Topic: topic, Payload: []byte(`{}`), CreatedAt: base})
	ok(t, err)
	eq(t, inserted, true, "outbox insert in tx")
}

func claimIDs(t *testing.T, c core.Coordinator, topic string) []string {
	t.Helper()
	msgs, err := c.OutboxClaim(ctx, []string{topic}, uid("holder"), base.Add(time.Hour), time.Minute, 10)
	ok(t, err)
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func assertCommitted(t *testing.T, s core.Store, c core.Coordinator, runID, key, msgID, topic string) {
	t.Helper()
	_, err := s.GetRun(ctx, runID)
	ok(t, err)
	v, err := s.KVGet(ctx, key)
	ok(t, err)
	eq(t, v, "v", "kv value")
	ids := claimIDs(t, c, topic)
	eq(t, len(ids), 1, "committed outbox messages")
	eq(t, ids[0], msgID, "outbox message id")
}

func assertRolledBack(t *testing.T, s core.Store, c core.Coordinator, runID, key, msgID, topic string) {
	t.Helper()
	_, err := s.GetRun(ctx, runID)
	isErr(t, err, core.ErrNotFound)
	if v, err := s.KVGet(ctx, key); err == nil && v != "" {
		t.Fatalf("kv %s survived rollback: %q", key, v)
	}
	eq(t, len(claimIDs(t, c, topic)), 0, "outbox messages after rollback")
	// The ID is free again.
	inserted, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: msgID, Topic: topic, Payload: []byte(`{}`)})
	ok(t, err)
	eq(t, inserted, true, "outbox id reusable after rollback")
}

func testTxCommit(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	var hooks []string
	err := tx.InTx(ctx, func(ctx context.Context) error {
		if !core.InTransaction(ctx) {
			t.Error("InTransaction is false inside InTx")
		}
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		// Reads inside the transaction see its writes.
		_, err := s.GetRun(ctx, runID)
		ok(t, err)
		// Duplicate outbox IDs are deduplicated inside the transaction too.
		inserted, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: msgID, Topic: topic, Payload: []byte(`{}`)})
		ok(t, err)
		eq(t, inserted, false, "duplicate outbox id in tx")
		core.AfterCommit(ctx, func() { hooks = append(hooks, "a") })
		core.AfterCommit(ctx, func() { hooks = append(hooks, "b") })
		if len(hooks) != 0 {
			t.Error("after-commit hook ran before commit")
		}
		return nil
	})
	ok(t, err)
	eq(t, fmt.Sprint(hooks), "[a b]", "after-commit hooks run once, in order")
	assertCommitted(t, s, c, runID, key, msgID, topic)
	if core.InTransaction(ctx) {
		t.Fatal("InTransaction is true outside InTx")
	}
}

func testTxRollback(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	boom := errors.New("boom")
	hookRan := false
	err := tx.InTx(ctx, func(ctx context.Context) error {
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		core.AfterCommit(ctx, func() { hookRan = true })
		return boom
	})
	isErr(t, err, boom)
	eq(t, hookRan, false, "after-commit hook on rollback")
	assertRolledBack(t, s, c, runID, key, msgID, topic)
}

func testTxFailTx(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	boom := errors.New("outbox write failed")
	err := tx.InTx(ctx, func(ctx context.Context) error {
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		eq(t, core.FailTx(ctx, boom), true, "FailTx inside a transaction")
		return nil
	})
	isErr(t, err, boom)
	assertRolledBack(t, s, c, runID, key, msgID, topic)
	eq(t, core.FailTx(ctx, boom), false, "FailTx outside a transaction")
}

func testTxNested(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	inner := uid("run")
	hookRan := false
	err := tx.InTx(ctx, func(ctx context.Context) error {
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		ok(t, tx.InTx(ctx, func(ctx context.Context) error {
			ok(t, s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: inner}))
			core.AfterCommit(ctx, func() { hookRan = true })
			return nil
		}))
		eq(t, hookRan, false, "inner hook waits for the outer commit")
		return nil
	})
	ok(t, err)
	eq(t, hookRan, true, "inner hook after outer commit")
	assertCommitted(t, s, c, runID, key, msgID, topic)
	_, err = s.GetRun(ctx, inner)
	ok(t, err)

	// A joined call rolls back with the outer transaction.
	outerFail := errors.New("outer")
	inner2 := uid("run")
	err = tx.InTx(ctx, func(ctx context.Context) error {
		ok(t, tx.InTx(ctx, func(ctx context.Context) error {
			return s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: inner2})
		}))
		return outerFail
	})
	isErr(t, err, outerFail)
	_, err = s.GetRun(ctx, inner2)
	isErr(t, err, core.ErrNotFound)
}

func testTxNestedError(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	innerErr := errors.New("inner")
	err := tx.InTx(ctx, func(ctx context.Context) error {
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		isErr(t, tx.InTx(ctx, func(context.Context) error { return innerErr }), innerErr)
		return nil // the outer ignores the inner error
	})
	isErr(t, err, innerErr)
	assertRolledBack(t, s, c, runID, key, msgID, topic)
}

// A failed statement (here a unique violation the caller handles) does not
// abort the transaction on any store.
func testTxFailedStatement(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	existing := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, existing))
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	err := tx.InTx(ctx, func(ctx context.Context) error {
		isErr(t, s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: existing.ID}), core.ErrConflict)
		txWrites(t, ctx, s, c, runID, key, msgID, topic)
		return nil
	})
	ok(t, err)
	assertCommitted(t, s, c, runID, key, msgID, topic)
}

func testTxPanic(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	runID, key, msgID, topic := uid("run"), uid("k"), uid("evt"), uid("topic")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		_ = tx.InTx(ctx, func(ctx context.Context) error {
			txWrites(t, ctx, s, c, runID, key, msgID, topic)
			panic("boom")
		})
	}()
	assertRolledBack(t, s, c, runID, key, msgID, topic)
	// The store is usable afterwards.
	ok(t, tx.InTx(ctx, func(ctx context.Context) error { return s.KVSet(ctx, uid("k"), "v") }))
}

func testTxContextOutlives(t *testing.T, s core.Store, tx core.Transactor, _ core.Coordinator) {
	runID := uid("run")
	var kept context.Context
	ok(t, tx.InTx(ctx, func(ctx context.Context) error {
		kept = ctx
		return s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: runID})
	}))
	if core.InTransaction(kept) {
		t.Fatal("finished transaction still reported active")
	}
	// A context captured by an after-commit hook keeps working.
	_, err := s.GetRun(kept, runID)
	ok(t, err)
	ok(t, s.KVSet(kept, uid("k"), "v"))
}

func testTxConcurrent(t *testing.T, s core.Store, tx core.Transactor, c core.Coordinator) {
	const n = 8
	topic := uid("topic")
	ids := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		ids[i] = uid("run")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- tx.InTx(ctx, func(ctx context.Context) error {
				if err := s.CreateRun(ctx, &core.Run{FactoryID: anyFactory, ID: ids[i]}); err != nil {
					return err
				}
				_, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: ids[i], Topic: topic, Payload: []byte(`{}`), CreatedAt: base})
				return err
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		ok(t, err)
	}
	for _, id := range ids {
		_, err := s.GetRun(ctx, id)
		ok(t, err)
	}
	eq(t, len(claimIDs(t, c, topic)), n, "outbox messages of concurrent transactions")
}
