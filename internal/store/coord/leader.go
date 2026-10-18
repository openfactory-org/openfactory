package coord

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// DefaultLeaderName is the lease shared by the scheduler, the idle sweeper,
// the run-lease sweeper and git definition sync.
const DefaultLeaderName = "control-plane"

// LeaderOptions configures a Leader.
type LeaderOptions struct {
	// Name of the lease row (default DefaultLeaderName).
	Name string
	// Holder identifies this process (default NewHolder(ReplicaID(""))).
	Holder string
	// TTL is how long a lease lasts without renewal (default 15s).
	TTL time.Duration
	// RenewInterval is how often the lease is renewed or retried (default TTL/3).
	RenewInterval time.Duration
	Clock         func() time.Time
	Logger        *slog.Logger
}

// Leader keeps a leader lease (one database row: holder, expires_at) for this
// replica. Exactly one live replica is leader at a time; a dead leader's lease
// is taken over after TTL. It implements core.Leadership.
type Leader struct {
	c    core.Coordinator
	opts LeaderOptions
	log  *slog.Logger

	until atomic.Int64 // unix nanos the lease is known valid until (0 = not leader)

	mu        sync.Mutex
	listeners []func(bool)
	leader    bool
	started   bool
	done      chan struct{}
}

var _ core.Leadership = (*Leader)(nil)

// NewLeader returns a Leader; call Start to begin campaigning.
func NewLeader(c core.Coordinator, opts LeaderOptions) *Leader {
	if opts.Name == "" {
		opts.Name = DefaultLeaderName
	}
	if opts.Holder == "" {
		opts.Holder = NewHolder("")
	}
	if opts.TTL <= 0 {
		opts.TTL = 15 * time.Second
	}
	if opts.RenewInterval <= 0 || opts.RenewInterval >= opts.TTL {
		opts.RenewInterval = opts.TTL / 3
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Leader{c: c, opts: opts, log: log.With("component", "leader", "lease", opts.Name, "holder", opts.Holder),
		done: make(chan struct{})}
}

// Holder returns this process's holder id.
func (l *Leader) Holder() string { return l.opts.Holder }

// IsLeader reports whether this replica holds the lease. It turns false as
// soon as the lease would have expired without a successful renewal, even if
// the renew loop is stuck.
func (l *Leader) IsLeader() bool {
	u := l.until.Load()
	return u != 0 && l.opts.Clock().UnixNano() < u
}

// OnChange registers fn, called (from the campaign goroutine) whenever
// leadership is gained (true) or lost (false).
func (l *Leader) OnChange(fn func(leader bool)) {
	l.mu.Lock()
	l.listeners = append(l.listeners, fn)
	l.mu.Unlock()
}

// Start makes one acquisition attempt synchronously, then keeps renewing in
// the background until ctx ends; the lease is then released. Start is a no-op
// after the first call.
func (l *Leader) Start(ctx context.Context) {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()
	l.Campaign(ctx)
	go func() {
		defer close(l.done)
		t := time.NewTicker(l.opts.RenewInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				l.resign()
				return
			case <-t.C:
				l.Campaign(ctx)
			}
		}
	}()
}

// Done is closed when the campaign goroutine started by Start has exited.
func (l *Leader) Done() <-chan struct{} { return l.done }

// Campaign makes one acquire-or-renew attempt and returns the result.
func (l *Leader) Campaign(ctx context.Context) bool {
	now := l.opts.Clock()
	until := now.Add(l.opts.TTL)
	ok, err := l.c.AcquireLeadership(ctx, l.opts.Name, l.opts.Holder, now, until)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("leader lease renewal failed", "err", err)
		}
		// Keep the current (time-bounded) state: IsLeader expires by itself.
		l.set(l.IsLeader())
		return l.IsLeader()
	}
	if ok {
		l.until.Store(until.UnixNano())
	} else {
		l.until.Store(0)
	}
	l.set(ok)
	return ok
}

func (l *Leader) resign() {
	was := l.IsLeader()
	l.until.Store(0)
	if was {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := l.c.ReleaseLeadership(ctx, l.opts.Name, l.opts.Holder); err != nil {
			l.log.Warn("release leader lease", "err", err)
		}
	}
	l.set(false)
}

func (l *Leader) set(leader bool) {
	l.mu.Lock()
	if l.leader == leader {
		l.mu.Unlock()
		return
	}
	l.leader = leader
	fns := append([]func(bool){}, l.listeners...)
	l.mu.Unlock()
	if leader {
		l.log.Info("became leader")
	} else {
		l.log.Info("lost leadership")
	}
	for _, fn := range fns {
		fn(leader)
	}
}

// Always is a core.Leadership that is always leader (single-process setups
// and tests).
type Always struct{}

// IsLeader implements core.Leadership.
func (Always) IsLeader() bool { return true }
