package guest

import "time"

// cancelQueue carries cancel requests to the supervisor. A push never
// blocks: requests that wait are merged and the shortest grace wins.
type cancelQueue struct{ ch chan int }

func newCancelQueue() cancelQueue { return cancelQueue{ch: make(chan int, 1)} }

func (q cancelQueue) push(grace int) {
	for {
		select {
		case q.ch <- grace:
			return
		default:
		}
		select {
		case old := <-q.ch:
			grace = min(grace, old)
		default:
		}
	}
}

// killClock is the SIGKILL deadline after a cancel. A later request can
// bring the deadline closer but never moves it later.
type killClock struct {
	t  *time.Timer
	at time.Time
}

func (k *killClock) arm(d time.Duration) {
	at := time.Now().Add(d)
	if k.t != nil && !at.Before(k.at) {
		return
	}
	k.stop()
	k.t, k.at = time.NewTimer(d), at
}

// C returns the channel of the deadline, or nil when none is armed.
func (k *killClock) C() <-chan time.Time {
	if k.t == nil {
		return nil
	}
	return k.t.C
}

// stop disarms the clock. Call it also after the deadline fired.
func (k *killClock) stop() {
	if k.t != nil {
		k.t.Stop()
		k.t = nil
	}
}
