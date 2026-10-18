package storetest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// coordinatorCases run when the store implements core.Coordinator.
var coordinatorCases = []struct {
	name string
	fn   func(t *testing.T, s core.Store, c core.Coordinator)
}{
	{"OutboxLifecycle", testOutboxLifecycle},
	{"OutboxVisibility", testOutboxVisibility},
	{"OutboxConcurrentClaim", testOutboxConcurrent},
	{"OutboxPartitionOrder", testOutboxPartitionOrder},
	{"OutboxPartitionReplicas", testOutboxPartitionReplicas},
	{"LateLockHolderWrite", testLateLockHolder},
	{"LeaderLease", testLeaderLease},
	{"LeaderLeaseConcurrent", testLeaderConcurrent},
	{"Locks", testLocks},
	{"Notify", testNotify},
}

func runCoordinator(t *testing.T, newStore func(t *testing.T) core.Store) {
	for _, cc := range coordinatorCases {
		t.Run("Coordinator/"+cc.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			c, isCoord := s.(core.Coordinator)
			if !isCoord {
				t.Skip("store does not implement core.Coordinator")
			}
			cc.fn(t, s, c)
		})
	}
}

func testOutboxLifecycle(t *testing.T, _ core.Store, c core.Coordinator) {
	topic := uid("topic")
	id := uid("evt")
	t0 := base
	m := &core.OutboxMessage{ID: id, Topic: topic, Payload: []byte(`{"a":1}`), CreatedAt: t0}
	inserted, err := c.OutboxEnqueue(ctx, m)
	ok(t, err)
	eq(t, inserted, true, "first enqueue inserts")
	inserted, err = c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: id, Topic: topic, Payload: []byte(`{"a":2}`), CreatedAt: t0})
	ok(t, err)
	eq(t, inserted, false, "duplicate id is ignored")
	_, err = c.OutboxEnqueue(ctx, &core.OutboxMessage{Topic: topic})
	isErr(t, err, core.ErrInvalid)

	got, err := c.OutboxClaim(ctx, []string{topic}, "h1", t0, time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 1, "claimed")
	eq(t, string(got[0].Payload), `{"a":1}`, "payload of first enqueue")
	eq(t, got[0].Attempts, 1, "attempts")
	eq(t, got[0].State, core.OutboxPending, "state")
	timeEq(t, got[0].CreatedAt, t0, "created")

	again, err := c.OutboxClaim(ctx, []string{topic}, "h2", t0, time.Minute, 10)
	ok(t, err)
	eq(t, len(again), 0, "claimed message is invisible")

	// Retry later: not due before next.
	next := t0.Add(10 * time.Second)
	isErr(t, c.OutboxRetry(ctx, id, "h2", next, "boom"), core.ErrConflict)
	ok(t, c.OutboxRetry(ctx, id, "h1", next, "boom"))
	got, err = c.OutboxClaim(ctx, []string{topic}, "h1", t0.Add(time.Second), time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 0, "not due yet")
	got, err = c.OutboxClaim(ctx, []string{topic}, "h1", next, time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 1, "due again")
	eq(t, got[0].Attempts, 2, "attempts after retry")
	eq(t, got[0].LastError, "boom", "last error")

	ok(t, c.OutboxAck(ctx, id, "h1", next))
	isErr(t, c.OutboxAck(ctx, id, "h1", next), core.ErrConflict)
	isErr(t, c.OutboxAck(ctx, uid("missing"), "h1", next), core.ErrNotFound)
	delivered, err := c.OutboxList(ctx, core.OutboxDelivered, 500)
	ok(t, err)
	found := false
	for _, d := range delivered {
		if d.ID == id {
			found = true
			if d.DeliveredAt == nil || !d.DeliveredAt.Equal(next) {
				t.Fatalf("delivered at = %v, want %v", d.DeliveredAt, next)
			}
		}
	}
	eq(t, found, true, "listed as delivered")

	// Dead letter + redrive.
	dead := uid("evt")
	_, err = c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: dead, Topic: topic, Payload: []byte(`{}`), CreatedAt: t0})
	ok(t, err)
	got, err = c.OutboxClaim(ctx, []string{topic}, "h1", t0, time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 1, "claim dead candidate")
	ok(t, c.OutboxDeadLetter(ctx, dead, "h1", "gave up"))
	got, err = c.OutboxClaim(ctx, []string{topic}, "h1", t0.Add(time.Hour), time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 0, "dead messages are not claimed")
	deads, err := c.OutboxList(ctx, core.OutboxDead, 500)
	ok(t, err)
	found = false
	for _, d := range deads {
		if d.ID == dead {
			found = true
			eq(t, d.LastError, "gave up", "dead error")
		}
	}
	eq(t, found, true, "listed as dead")
	ok(t, c.OutboxRedrive(ctx, dead, t0))
	isErr(t, c.OutboxRedrive(ctx, dead, t0), core.ErrNotFound)
	got, err = c.OutboxClaim(ctx, []string{topic}, "h1", t0, time.Minute, 10)
	ok(t, err)
	eq(t, len(got), 1, "redriven")
	eq(t, got[0].Attempts, 1, "attempts reset by redrive")
	ok(t, c.OutboxAck(ctx, dead, "h1", t0))

	// Purge removes delivered messages older than the cutoff.
	n, err := c.OutboxPurge(ctx, next.Add(time.Nanosecond))
	ok(t, err)
	if n < 2 {
		t.Fatalf("purged %d, want >= 2", n)
	}
	inserted, err = c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: id, Topic: topic, Payload: []byte(`{}`)})
	ok(t, err)
	eq(t, inserted, true, "purged id can be enqueued again")
}

func testOutboxVisibility(t *testing.T, _ core.Store, c core.Coordinator) {
	topic, other := uid("topic"), uid("topic")
	id := uid("evt")
	_, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: id, Topic: topic, Payload: []byte(`1`), CreatedAt: base})
	ok(t, err)
	_, err = c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: uid("evt"), Topic: other, Payload: []byte(`2`), CreatedAt: base})
	ok(t, err)
	got, err := c.OutboxClaim(ctx, []string{topic}, "a", base, 30*time.Second, 10)
	ok(t, err)
	eq(t, len(got), 1, "only the requested topic")
	eq(t, got[0].ID, id, "claimed id")
	// A crashed claimer's message becomes visible after the visibility timeout.
	got, err = c.OutboxClaim(ctx, []string{topic}, "b", base.Add(30*time.Second), 30*time.Second, 10)
	ok(t, err)
	eq(t, len(got), 1, "visible after timeout")
	eq(t, got[0].Attempts, 2, "attempts count every claim")
	isErr(t, c.OutboxAck(ctx, id, "a", base), core.ErrConflict)
	ok(t, c.OutboxAck(ctx, id, "b", base))
}

func testOutboxConcurrent(t *testing.T, _ core.Store, c core.Coordinator) {
	topic := uid("topic")
	const msgs, workers = 40, 6
	for i := 0; i < msgs; i++ {
		_, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: uid("evt"), Topic: topic, Payload: []byte(`{}`),
			CreatedAt: base.Add(time.Duration(i) * time.Microsecond)})
		ok(t, err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			holder := fmt.Sprintf("replica-%d", w)
			for {
				got, err := c.OutboxClaim(ctx, []string{topic}, holder, base.Add(time.Second), time.Hour, 3)
				if err != nil {
					errs <- err
					return
				}
				if len(got) == 0 {
					return
				}
				for _, m := range got {
					mu.Lock()
					seen[m.ID]++
					mu.Unlock()
					if err := c.OutboxAck(ctx, m.ID, holder, base); err != nil {
						errs <- err
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("claim: %v", err)
	}
	eq(t, len(seen), msgs, "every message delivered")
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("message %s delivered %d times", id, n)
		}
	}
}

func testLeaderLease(t *testing.T, _ core.Store, c core.Coordinator) {
	name := uid("leader")
	ttl := 15 * time.Second
	got, err := c.AcquireLeadership(ctx, name, "a", base, base.Add(ttl))
	ok(t, err)
	eq(t, got, true, "a acquires")
	got, err = c.AcquireLeadership(ctx, name, "b", base.Add(time.Second), base.Add(time.Second+ttl))
	ok(t, err)
	eq(t, got, false, "b blocked while a holds")
	got, err = c.AcquireLeadership(ctx, name, "a", base.Add(5*time.Second), base.Add(5*time.Second+ttl))
	ok(t, err)
	eq(t, got, true, "a renews")
	got, err = c.AcquireLeadership(ctx, name, "b", base.Add(16*time.Second), base.Add(31*time.Second))
	ok(t, err)
	eq(t, got, false, "renewal extended the lease")
	got, err = c.AcquireLeadership(ctx, name, "b", base.Add(20*time.Second), base.Add(35*time.Second))
	ok(t, err)
	eq(t, got, true, "b takes over an expired lease")
	got, err = c.AcquireLeadership(ctx, name, "a", base.Add(21*time.Second), base.Add(36*time.Second))
	ok(t, err)
	eq(t, got, false, "a lost the lease")
	ok(t, c.ReleaseLeadership(ctx, name, "a")) // not holder: no-op
	got, err = c.AcquireLeadership(ctx, name, "a", base.Add(22*time.Second), base.Add(37*time.Second))
	ok(t, err)
	eq(t, got, false, "release by non-holder is ignored")
	ok(t, c.ReleaseLeadership(ctx, name, "b"))
	got, err = c.AcquireLeadership(ctx, name, "a", base.Add(22*time.Second), base.Add(37*time.Second))
	ok(t, err)
	eq(t, got, true, "a acquires after release")
}

func testLeaderConcurrent(t *testing.T, _ core.Store, c core.Coordinator) {
	for round := 0; round < 5; round++ {
		name := uid("leader")
		const n = 8
		var wins sync.Map
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				holder := fmt.Sprintf("replica-%d", i)
				got, err := c.AcquireLeadership(ctx, name, holder, base, base.Add(time.Minute))
				if err != nil {
					errs <- err
					return
				}
				if got {
					wins.Store(holder, true)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("acquire: %v", err)
		}
		count := 0
		wins.Range(func(_, _ any) bool { count++; return true })
		eq(t, count, 1, "exactly one leader")
	}
}

func testLocks(t *testing.T, _ core.Store, c core.Coordinator) {
	key := uid("wi")
	got, err := c.TryLock(ctx, key, "a", base, base.Add(time.Minute))
	ok(t, err)
	eq(t, got, true, "a locks")
	got, err = c.TryLock(ctx, key, "b", base, base.Add(time.Minute))
	ok(t, err)
	eq(t, got, false, "b blocked")
	got, err = c.TryLock(ctx, uid("wi"), "b", base, base.Add(time.Minute))
	ok(t, err)
	eq(t, got, true, "other keys are independent")
	ok(t, c.Unlock(ctx, key, "b"))
	got, err = c.TryLock(ctx, key, "b", base, base.Add(time.Minute))
	ok(t, err)
	eq(t, got, false, "unlock by non-holder is ignored")
	ok(t, c.Unlock(ctx, key, "a"))
	got, err = c.TryLock(ctx, key, "b", base, base.Add(time.Minute))
	ok(t, err)
	eq(t, got, true, "b locks after unlock")
	got, err = c.TryLock(ctx, key, "a", base.Add(2*time.Minute), base.Add(3*time.Minute))
	ok(t, err)
	eq(t, got, true, "expired lock is taken over")
}

func testNotify(t *testing.T, _ core.Store, c core.Coordinator) {
	want := uid("ping")
	got := make(chan string, 16)
	cancel := c.Listen(func(p string) {
		if p == want {
			got <- p
		}
	})
	defer cancel()
	deadline := time.After(10 * time.Second)
	// The PostgreSQL listener connects asynchronously: notify until heard.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		ok(t, c.Notify(ctx, want))
		select {
		case <-got:
			return
		case <-tick.C:
		case <-deadline:
			t.Fatal("notification not received")
		}
	}
}
