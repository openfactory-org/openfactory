package coord

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestReplicaIDAndHolder(t *testing.T) {
	t.Setenv(ReplicaIDEnv, "pod-7")
	if got := ReplicaID(""); got != "pod-7" {
		t.Fatalf("ReplicaID = %q", got)
	}
	if got := ReplicaID(" explicit "); got != "explicit" {
		t.Fatalf("ReplicaID = %q", got)
	}
	h1, h2 := NewHolder(""), NewHolder("")
	if h1 == h2 || HolderReplica(h1) != "pod-7" {
		t.Fatalf("holders %q %q", h1, h2)
	}
}

func TestFromSharesFallbackPerStore(t *testing.T) {
	type fake struct{ core.Store }
	s := &fake{}
	if From(s) != From(s) {
		t.Fatal("fallback coordinator must be shared per store")
	}
	if From(nil) != nil {
		t.Fatal("nil store")
	}
}

// Two campaigning replicas: exactly one leads at any time and the follower
// takes over after the leader stops.
func TestLeaderElectionAndFailover(t *testing.T) {
	c := NewMemory()
	opts := LeaderOptions{TTL: 150 * time.Millisecond, RenewInterval: 20 * time.Millisecond}
	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()
	oa, ob := opts, opts
	oa.Holder, ob.Holder = "a/1", "b/1"
	a, b := NewLeader(c, oa), NewLeader(c, ob)
	var changes atomic.Int32
	b.OnChange(func(bool) { changes.Add(1) })
	a.Start(ctxA)
	b.Start(ctxB)
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a.IsLeader() == b.IsLeader() {
			t.Fatal("split brain or no leader")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopA() // resigns: b takes over at its next campaign
	<-a.Done()
	waitTrue(t, "b leads", b.IsLeader)
	if a.IsLeader() {
		t.Fatal("a still leader after stop")
	}
	waitTrue(t, "b notified", func() bool { return changes.Load() == 1 })
}

// A leader that cannot renew (frozen, partitioned) stops reporting
// leadership once its lease would have expired.
func TestLeaderExpiresWithoutRenewal(t *testing.T) {
	c := NewMemory()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	l := NewLeader(c, LeaderOptions{Holder: "a/1", TTL: 10 * time.Second, Clock: clock})
	if !l.Campaign(context.Background()) || !l.IsLeader() {
		t.Fatal("not leader")
	}
	mu.Lock()
	now = now.Add(11 * time.Second)
	mu.Unlock()
	if l.IsLeader() {
		t.Fatal("leader past its lease")
	}
	other := NewLeader(c, LeaderOptions{Holder: "b/1", TTL: 10 * time.Second, Clock: clock})
	if !other.Campaign(context.Background()) {
		t.Fatal("expired lease not taken over")
	}
	if l.Campaign(context.Background()) {
		t.Fatal("old leader re-acquired a held lease")
	}
}

func TestLockerMutualExclusionAcrossHolders(t *testing.T) {
	c := NewMemory()
	lockers := []*Locker{NewLocker(c, LockerOptions{Holder: "a"}), NewLocker(c, LockerOptions{Holder: "b"}), NewLocker(c, LockerOptions{Holder: "c"})}
	var inside, max atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		l := lockers[i%len(lockers)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := l.Lock(context.Background(), "k")
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if max.Load() != 1 {
		t.Fatalf("max holders = %d", max.Load())
	}
}

func TestLockerHonoursContext(t *testing.T) {
	c := NewMemory()
	_, _ = c.TryLock(context.Background(), "k", "other", time.Now(), time.Now().Add(time.Hour))
	l := NewLocker(c, LockerOptions{Holder: "me"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "k"); err == nil {
		t.Fatal("lock held by another holder acquired")
	}
	// The local mutex was released: a later attempt with a free row works.
	_ = c.Unlock(context.Background(), "k", "other")
	unlock, err := l.Lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func waitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
