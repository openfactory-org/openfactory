package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// The two ends of the enforcement probe. The probe Pods run them with the
// hidden command `openfactory netprobe` (see Probe).

// ServeProbe accepts TCP connections on addr and closes each one at once. It
// returns when ctx ends.
func ServeProbe(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // the listener was closed because ctx ended: a normal stop
			}
			return err
		}
		_ = c.Close()
	}
}

// DialProbe waits for delay, then tries up to attempts times to open a TCP
// connection to addr. It returns true as soon as one attempt works. interval
// is the time between two attempts, and timeout is the time of one attempt.
func DialProbe(ctx context.Context, addr string, delay time.Duration, attempts int, interval, timeout time.Duration) bool {
	if !sleep(ctx, delay) {
		return false
	}
	if attempts < 1 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if i > 0 && !sleep(ctx, interval) {
			return false
		}
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return true
		}
		if errors.Is(err, context.Canceled) {
			return false
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ErrNotBlocked means that a direct connection still worked when the time of
// WaitBlocked was over.
var ErrNotBlocked = errors.New("a direct connection still works")

// WaitOptions are the settings of WaitBlocked. The zero value uses the
// defaults of this package.
type WaitOptions struct {
	// Timeout is the time for the whole wait. Default DefaultWaitTimeout.
	Timeout time.Duration
	// Interval is the time between two dials. Default WaitInterval.
	Interval time.Duration
	// AttemptTimeout is the time of one dial. Default WaitAttemptTimeout.
	AttemptTimeout time.Duration
	// Confirm is the number of failed dials in a row that ends the wait.
	// Default WaitConfirm.
	Confirm int
	// Dial opens a connection. Default: TCP.
	Dial func(ctx context.Context, addr string, timeout time.Duration) error
}

func (o *WaitOptions) defaults() {
	if o.Timeout <= 0 {
		o.Timeout = DefaultWaitTimeout
	}
	if o.Interval <= 0 {
		o.Interval = WaitInterval
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = WaitAttemptTimeout
	}
	if o.Confirm < 1 {
		o.Confirm = WaitConfirm
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, addr string, timeout time.Duration) error {
			d := net.Dialer{Timeout: timeout}
			c, err := d.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = c.Close()
			}
			return err
		}
	}
}

// WaitBlocked dials addr until the connection fails Confirm times in a row. It
// returns the time that this took. If a connection still works when Timeout is
// over, the error wraps ErrNotBlocked. If ctx ends first, the error is the
// error of ctx.
//
// A connection that works means: the network does not block addr (yet). Any
// error, including "connection refused", counts as blocked, because a network
// plugin can reject packets as well as drop them. addr must be a target that
// accepts connections when no policy applies (see wait.go).
func WaitBlocked(ctx context.Context, addr string, o WaitOptions) (time.Duration, error) {
	o.defaults()
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	fails := 0
	for {
		err := o.Dial(wctx, addr, o.AttemptTimeout)
		switch {
		case err == nil:
			fails = 0
		case wctx.Err() != nil:
			// The dial ended because the time is over: this says nothing.
		default:
			if fails++; fails >= o.Confirm {
				return time.Since(start), nil
			}
		}
		if wctx.Err() == nil {
			sleep(wctx, o.Interval)
		}
		if wctx.Err() != nil {
			if ctx.Err() != nil {
				return time.Since(start), ctx.Err()
			}
			return time.Since(start), fmt.Errorf("%w after %s: %s", ErrNotBlocked, o.Timeout, addr)
		}
	}
}
