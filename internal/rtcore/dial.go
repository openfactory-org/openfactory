package rtcore

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Intervals of WaitDial.
const (
	waitDialTimeout = 200 * time.Millisecond // one connection attempt
	waitDialRetry   = 20 * time.Millisecond  // pause between attempts
)

// WaitDial dials network and address until a connection succeeds, then
// closes it. It is for a local listener that a process opens at start (a
// unix socket, or a TCP port on the host). It returns ctx.Err() when ctx
// ends, and the last dial error, wrapped, when timeout passes first.
func WaitDial(ctx context.Context, network, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	d := net.Dialer{Timeout: waitDialTimeout}
	for {
		c, err := d.DialContext(ctx, network, address)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s accepts no connection after %s: %w", network, address, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDialRetry):
		}
	}
}
