package webhooks

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/coord"
)

// windowLimiter is a fixed one-minute window counter per key. With a shared
// counter (newSharedWindowLimiter) allow counts in the database, so the limit
// holds across replicas; the other methods always count in process memory.
type windowLimiter struct {
	limit  int
	window time.Duration

	mu sync.Mutex
	m  map[string]*window

	shared *sharedCounter
}

type window struct {
	start time.Time
	count int
}

func newWindowLimiter(limit int, w time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: w, m: map[string]*window{}}
}

func (l *windowLimiter) get(key string, now time.Time) *window {
	w, ok := l.m[key]
	if !ok || now.Sub(w.start) >= l.window || now.Before(w.start) {
		if len(l.m) > 10000 {
			l.prune(now)
		}
		w = &window{start: now}
		l.m[key] = w
	}
	return w
}

func (l *windowLimiter) prune(now time.Time) {
	for k, w := range l.m {
		if now.Sub(w.start) >= l.window {
			delete(l.m, k)
		}
	}
}

// allow consumes one unit; it reports false (and the time until the window
// resets) when the key is over its limit.
func (l *windowLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil || l.limit <= 0 {
		return true, 0
	}
	if l.shared != nil {
		ok, retry, err := l.shared.allow(key, now, l.limit, l.window)
		if err == nil {
			return ok, retry
		}
		// The database cannot count: limit this replica on its own.
		l.shared.log.Warn("shared webhook rate limit unavailable; counting on this replica", "webhook_id", key, "err", err)
	}
	return l.allowLocal(key, now)
}

func (l *windowLimiter) allowLocal(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.get(key, now)
	if w.count >= l.limit {
		return false, w.start.Add(l.window).Sub(now)
	}
	w.count++
	return true, 0
}

// exhausted reports whether the key is over its limit without consuming.
func (l *windowLimiter) exhausted(key string, now time.Time) (bool, time.Duration) {
	if l == nil || l.limit <= 0 {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.get(key, now)
	if w.count >= l.limit {
		return true, w.start.Add(l.window).Sub(now)
	}
	return false, 0
}

// add consumes one unit unconditionally.
func (l *windowLimiter) add(key string, now time.Time) {
	if l == nil || l.limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.get(key, now).count++
}

// sharedCounter counts fixed windows in the store's KV, one row per key
// ("<start unix ns> <count>"). A coordination lock per key makes the
// read-and-increment atomic across replicas, and a striped mutex makes it
// atomic between the goroutines of this replica (they share one lock holder).
type sharedCounter struct {
	store  core.Store
	coord  core.Coordinator
	holder string
	prefix string
	log    *slog.Logger
	// lockWait bounds how long allow waits for the coordination lock.
	lockWait time.Duration
	stripes  [64]sync.Mutex
}

// newSharedWindowLimiter returns a windowLimiter whose allow counts in the
// store, so every replica sees the same window. The store must also be the
// core.Coordinator (SQLite and PostgreSQL are); with any
// other store the limiter counts in process memory.
func newSharedWindowLimiter(store core.Store, prefix string, limit int, w time.Duration, log *slog.Logger) *windowLimiter {
	l := newWindowLimiter(limit, w)
	c, ok := store.(core.Coordinator)
	if !ok || limit <= 0 {
		return l
	}
	if log == nil {
		log = slog.Default()
	}
	l.shared = &sharedCounter{
		store: store, coord: c, holder: coord.NewHolder(""),
		prefix: prefix, log: log, lockWait: 2 * time.Second,
	}
	return l
}

func (c *sharedCounter) stripe(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &c.stripes[h.Sum32()%uint32(len(c.stripes))]
}

func (c *sharedCounter) allow(key string, now time.Time, limit int, window time.Duration) (bool, time.Duration, error) {
	mu := c.stripe(key)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), c.lockWait+5*time.Second)
	defer cancel()

	kvKey := c.prefix + key
	lockKey := kvKey + "/lock"
	deadline := time.Now().Add(c.lockWait)
	for wait := 2 * time.Millisecond; ; wait = min(wait*2, 50*time.Millisecond) {
		ok, err := c.coord.TryLock(ctx, lockKey, c.holder, now, now.Add(10*time.Second))
		if err != nil {
			return false, 0, fmt.Errorf("lock rate window: %w", err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return false, 0, errors.New("lock rate window: timed out")
		}
		time.Sleep(wait)
	}
	defer func() {
		if err := c.coord.Unlock(context.WithoutCancel(ctx), lockKey, c.holder); err != nil {
			c.log.Debug("unlock webhook rate window", "webhook_id", key, "err", err)
		}
	}()

	start, count := now, 0
	v, err := c.store.KVGet(ctx, kvKey)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return false, 0, fmt.Errorf("read rate window: %w", err)
	}
	if s, n, ok := parseWindow(v); ok && now.Sub(s) < window && !now.Before(s) {
		start, count = s, n
	}
	if count >= limit {
		return false, start.Add(window).Sub(now), nil
	}
	if err := c.store.KVSet(ctx, kvKey, formatWindow(start, count+1)); err != nil {
		return false, 0, fmt.Errorf("write rate window: %w", err)
	}
	return true, 0, nil
}

func formatWindow(start time.Time, count int) string {
	return strconv.FormatInt(start.UnixNano(), 10) + " " + strconv.Itoa(count)
}

func parseWindow(v string) (time.Time, int, bool) {
	a, b, ok := strings.Cut(v, " ")
	if !ok {
		return time.Time{}, 0, false
	}
	ns, err1 := strconv.ParseInt(a, 10, 64)
	n, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, false
	}
	return time.Unix(0, ns), n, true
}

// RateLimitKVPrefix prefixes the KV keys that hold the shared rate-limit
// window of each webhook.
const RateLimitKVPrefix = "webhooks/rate/"
