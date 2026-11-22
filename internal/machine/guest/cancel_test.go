package guest

import (
	"testing"
	"time"
)

func TestCancelQueueNeverBlocks(t *testing.T) {
	q := newCancelQueue()
	done := make(chan struct{})
	go func() {
		for _, g := range []int{30, 10, 20, 15, 40, 25} {
			q.push(g)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked without a reader")
	}
	if g := <-q.ch; g != 10 {
		t.Fatalf("merged grace = %d, want the shortest (10)", g)
	}
	select {
	case g := <-q.ch:
		t.Fatalf("a second request %d waits; requests must merge", g)
	default:
	}
}

func TestKillClockOnlyMovesCloser(t *testing.T) {
	var k killClock
	if k.C() != nil {
		t.Fatal("a new clock is armed")
	}
	k.arm(time.Hour)
	first := k.at
	k.arm(2 * time.Hour)
	if !k.at.Equal(first) {
		t.Fatal("a later cancel moved the kill later")
	}
	k.arm(20 * time.Millisecond)
	if !k.at.Before(first) {
		t.Fatal("a shorter grace must bring the kill closer")
	}
	select {
	case <-k.C():
	case <-time.After(2 * time.Second):
		t.Fatal("the closer deadline did not fire")
	}
	k.stop()
	if k.C() != nil {
		t.Fatal("stop must disarm the clock")
	}
	k.arm(time.Hour) // a cancel after the kill arms a new deadline
	if k.C() == nil {
		t.Fatal("arm after stop must arm the clock")
	}
	k.stop()
}
