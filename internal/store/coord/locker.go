package coord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// LockerOptions configures a Locker.
type LockerOptions struct {
	// Holder identifies this process (default NewHolder("")).
	Holder string
	// TTL bounds how long a crashed holder blocks a key (default 30s). Held
	// locks are renewed every TTL/3.
	TTL time.Duration
	// MinBackoff and MaxBackoff bound the retry delay while a key is held by
	// another replica (defaults 5ms and 250ms).
	MinBackoff, MaxBackoff time.Duration
	Clock                  func() time.Time
	Logger                 *slog.Logger
}

// Locker provides mutual exclusion per key across replicas. Within a process
// a key is serialized by a mutex first, so only one goroutine per process
// competes for the database row; across processes the row (key, holder,
// expires_at) decides. Lock rows carry an expiry instead of pinning a database
// connection, so held locks never exhaust the connection pool, and a crashed
// replica's locks free themselves after TTL.
type Locker struct {
	c    core.Coordinator
	opts LockerOptions
	log  *slog.Logger

	mu    sync.Mutex
	local map[string]*localLock
}

type localLock struct {
	mu   sync.Mutex
	refs int
}

// NewLocker returns a Locker on c. A nil c gives process-local locks only.
func NewLocker(c core.Coordinator, opts LockerOptions) *Locker {
	if opts.Holder == "" {
		opts.Holder = NewHolder("")
	}
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 5 * time.Millisecond
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = 250 * time.Millisecond
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Locker{c: c, opts: opts, log: log.With("component", "locker"), local: map[string]*localLock{}}
}

func (l *Locker) localFor(key string) *localLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	ll := l.local[key]
	if ll == nil {
		ll = &localLock{}
		l.local[key] = ll
	}
	ll.refs++
	return ll
}

func (l *Locker) dropLocal(key string, ll *localLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ll.refs--
	if ll.refs == 0 {
		delete(l.local, key)
	}
}

// Lock blocks until key is held or ctx ends. The returned func releases it
// and must be called exactly once.
func (l *Locker) Lock(ctx context.Context, key string) (func(), error) {
	ll := l.localFor(key)
	ll.mu.Lock()
	if l.c == nil {
		return func() { ll.mu.Unlock(); l.dropLocal(key, ll) }, nil
	}
	backoff := l.opts.MinBackoff
	for {
		now := l.opts.Clock()
		ok, err := l.c.TryLock(ctx, key, l.opts.Holder, now, now.Add(l.opts.TTL))
		if err == nil && ok {
			break
		}
		if err != nil && ctx.Err() == nil {
			l.log.Warn("lock attempt failed", "key", key, "err", err)
		}
		select {
		case <-ctx.Done():
			ll.mu.Unlock()
			l.dropLocal(key, ll)
			return nil, fmt.Errorf("lock %q: %w", key, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > l.opts.MaxBackoff {
			backoff = l.opts.MaxBackoff
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { //nolint:gosec // renews the held lock until release, after the acquire context ends
		defer wg.Done()
		t := time.NewTicker(l.opts.TTL / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := l.opts.Clock()
				if ok, err := l.c.TryLock(context.Background(), key, l.opts.Holder, now, now.Add(l.opts.TTL)); err != nil || !ok {
					l.log.Warn("lock renewal failed", "key", key, "ok", ok, "err", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
			uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := l.c.Unlock(uctx, key, l.opts.Holder); err != nil {
				l.log.Warn("unlock failed; the lock expires by itself", "key", key, "err", err)
			}
			cancel()
			ll.mu.Unlock()
			l.dropLocal(key, ll)
		})
	}, nil
}
