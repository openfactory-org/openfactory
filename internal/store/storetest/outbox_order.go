package storetest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func enqueueKeyed(t *testing.T, c core.Coordinator, topic, key string, at time.Time) string {
	t.Helper()
	id := uid("evt")
	inserted, err := c.OutboxEnqueue(ctx, &core.OutboxMessage{ID: id, Topic: topic, PartitionKey: key, Payload: []byte(`{}`), CreatedAt: at})
	ok(t, err)
	eq(t, inserted, true, "enqueued")
	return id
}

func claimKeyed(t *testing.T, c core.Coordinator, topics []string, holder string, at time.Time) map[string]core.OutboxMessage {
	t.Helper()
	got, err := c.OutboxClaim(ctx, topics, holder, at, time.Hour, 50)
	ok(t, err)
	out := map[string]core.OutboxMessage{}
	for _, m := range got {
		out[m.ID] = m
	}
	return out
}

func wantIDs(t *testing.T, got map[string]core.OutboxMessage, what string, ids ...string) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("%s: claimed %d messages, want %d (%v)", what, len(got), len(ids), ids)
	}
	for _, id := range ids {
		if _, found := got[id]; !found {
			t.Fatalf("%s: %s was not claimed", what, id)
		}
	}
}

// testOutboxPartitionOrder checks the claim rules of the outbox: only the first
// pending message of a key is claimed, a message that waits for a retry
// stays at the head of its key, and a dead message does not block.
func testOutboxPartitionOrder(t *testing.T, _ core.Store, c core.Coordinator) {
	topic, other := uid("topic"), uid("topic")
	k1, k2 := uid("key"), uid("key")
	m1 := enqueueKeyed(t, c, topic, k1, base)
	n1 := enqueueKeyed(t, c, topic, k2, base.Add(time.Microsecond))
	m2 := enqueueKeyed(t, c, topic, k1, base.Add(2*time.Microsecond))
	free1 := enqueueKeyed(t, c, topic, "", base.Add(3*time.Microsecond))
	free2 := enqueueKeyed(t, c, topic, "", base.Add(4*time.Microsecond))
	m3 := enqueueKeyed(t, c, other, k1, base.Add(5*time.Microsecond)) // same key, other topic
	n2 := enqueueKeyed(t, c, topic, k2, base.Add(6*time.Microsecond))

	both := []string{topic, other}
	got := claimKeyed(t, c, both, "h1", base.Add(time.Second))
	wantIDs(t, got, "first claim: the head of each key and every message without a key", m1, n1, free1, free2)
	eq(t, got[m1].PartitionKey, k1, "partition key is returned")
	wantIDs(t, claimKeyed(t, c, both, "h2", base.Add(time.Second)), "heads are in flight")

	// A failed head waits for its retry and still blocks its key.
	next := base.Add(time.Minute)
	ok(t, c.OutboxRetry(ctx, m1, "h1", next, "boom"))
	ok(t, c.OutboxAck(ctx, n1, "h1", base))
	wantIDs(t, claimKeyed(t, c, both, "h2", base.Add(2*time.Second)), "the retry of m1 blocks m2; n2 follows n1", n2)
	got = claimKeyed(t, c, both, "h2", next)
	wantIDs(t, got, "m1 is due again", m1)
	eq(t, got[m1].Attempts, 2, "attempts")

	// A dead head does not block later messages of its key.
	ok(t, c.OutboxDeadLetter(ctx, m1, "h2", "gave up"))
	wantIDs(t, claimKeyed(t, c, both, "h3", next), "m2 after the dead m1", m2)
	ok(t, c.OutboxAck(ctx, m2, "h3", next))

	// The order of a key holds across topics.
	wantIDs(t, claimKeyed(t, c, []string{other}, "h3", next), "m3 after m2", m3)
	ok(t, c.OutboxAck(ctx, m3, "h3", next))

	// A redriven message goes back to its place. Later messages wait for it.
	m4 := enqueueKeyed(t, c, topic, k1, base.Add(7*time.Microsecond))
	ok(t, c.OutboxRedrive(ctx, m1, next))
	wantIDs(t, claimKeyed(t, c, both, "h4", next), "redriven m1 is the head again", m1)
	ok(t, c.OutboxAck(ctx, m1, "h4", next))
	wantIDs(t, claimKeyed(t, c, both, "h4", next), "m4 after m1", m4)
	ok(t, c.OutboxAck(ctx, m4, "h4", next))
	ok(t, c.OutboxAck(ctx, n2, "h2", next))
	ok(t, c.OutboxAck(ctx, free1, "h1", next))
	ok(t, c.OutboxAck(ctx, free2, "h1", next))
}

// testOutboxPartitionReplicas checks ordered claims across replicas: two replicas
// claim 100 ordered events of one key while one event fails two times. The
// events are delivered in order, never two of the key at the same time, and
// other keys run in parallel.
func testOutboxPartitionReplicas(t *testing.T, _ core.Store, c core.Coordinator) {
	topic := uid("topic")
	const ordered, others, perOther = 100, 3, 20
	main := uid("key")
	keys := []string{main}
	for i := 0; i < others; i++ {
		keys = append(keys, uid("key"))
	}
	ids := map[string][]string{}
	keyOf := map[string]string{}
	seqOf := map[string]int{}
	at := base
	enqueue := func(key string) {
		at = at.Add(time.Microsecond)
		id := enqueueKeyed(t, c, topic, key, at)
		seqOf[id] = len(ids[key])
		ids[key] = append(ids[key], id)
		keyOf[id] = key
	}
	for i := 0; i < ordered; i++ {
		enqueue(main)
		if i < perOther {
			for _, k := range keys[1:] {
				enqueue(k)
			}
		}
	}
	total := len(keyOf)
	failing := ids[main][37]

	now := base.Add(time.Hour)
	var mu sync.Mutex
	delivered := map[string][]int{}
	inFlight := map[string]int{}
	failures := 0
	maxKeysInBatch := 0
	count := 0
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func(holder string) {
			defer wg.Done()
			deadline := time.Now().Add(60 * time.Second)
			for {
				mu.Lock()
				done := count == total
				mu.Unlock()
				if done {
					return
				}
				if time.Now().After(deadline) {
					errs <- fmt.Errorf("%s: timed out", holder)
					return
				}
				msgs, err := c.OutboxClaim(ctx, []string{topic}, holder, now, time.Hour, 5)
				if err != nil {
					errs <- err
					return
				}
				if len(msgs) == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				batchKeys := map[string]bool{}
				mu.Lock()
				for _, m := range msgs {
					k := keyOf[m.ID]
					batchKeys[k] = true
					inFlight[k]++
					if inFlight[k] > 1 {
						mu.Unlock()
						errs <- fmt.Errorf("two events of key %s in flight at the same time", k)
						return
					}
				}
				if len(batchKeys) > maxKeysInBatch {
					maxKeysInBatch = len(batchKeys)
				}
				mu.Unlock()
				for _, m := range msgs {
					k := keyOf[m.ID]
					mu.Lock()
					fail := m.ID == failing && failures < 2
					if fail {
						failures++
					}
					inFlight[k]--
					if !fail {
						delivered[k] = append(delivered[k], seqOf[m.ID])
						count++
					}
					mu.Unlock()
					if fail {
						err = c.OutboxRetry(ctx, m.ID, holder, now, "temporary failure")
					} else {
						err = c.OutboxAck(ctx, m.ID, holder, now)
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}
		}(fmt.Sprintf("replica-%d", r))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	eq(t, failures, 2, "the failing event failed two times")
	for _, k := range keys {
		seqs := delivered[k]
		eq(t, len(seqs), len(ids[k]), "events delivered for key "+k)
		for i, s := range seqs {
			if s != i {
				t.Fatalf("key %s: delivery %d is event %d, want %d (order %v)", k, i, s, i, seqs)
			}
		}
	}
	if maxKeysInBatch < 2 {
		t.Fatalf("events of different keys never ran in parallel (max %d keys in a batch)", maxKeysInBatch)
	}
}
