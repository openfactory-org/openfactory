package events

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func recv(t *testing.T, ch <-chan any) any {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message")
	}
	return nil
}

func TestFanOut(t *testing.T) {
	b := New()
	defer b.Close()
	ctx := context.Background()
	a, unsubA := b.Subscribe(core.TopicRunUpdated)
	c, unsubC := b.Subscribe(core.TopicRunUpdated)
	other, unsubO := b.Subscribe(core.TopicInbox)
	defer unsubA()
	defer unsubC()
	defer unsubO()

	run := &core.Run{FactoryID: "fac_test", ID: "run_1"}
	b.Publish(ctx, core.TopicRunUpdated, run)
	if recv(t, a) != run || recv(t, c) != run {
		t.Fatal("subscribers must receive the same payload")
	}
	select {
	case v := <-other:
		t.Fatalf("other topic received %v", v)
	default:
	}
	if st := b.Stats(); st.Published != 1 || st.Subscribers != 3 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestOrderingPerSubscriber(t *testing.T) {
	b := New()
	defer b.Close()
	ch, unsub := b.Subscribe("t")
	defer unsub()
	for i := 0; i < 100; i++ {
		b.Publish(context.Background(), "t", i)
	}
	for i := 0; i < 100; i++ {
		if v := recv(t, ch); v != i {
			t.Fatalf("got %v, want %d", v, i)
		}
	}
}

func TestWildcard(t *testing.T) {
	b := New()
	defer b.Close()
	ch, unsub := b.Subscribe(Wildcard)
	defer unsub()
	b.Publish(context.Background(), "x", 1)
	env, ok := recv(t, ch).(Envelope)
	if !ok || env.Topic != "x" || env.Payload != 1 {
		t.Fatalf("got %#v", env)
	}
}

func TestDropOnFullBuffer(t *testing.T) {
	var logs bytes.Buffer
	b := New(WithBufferSize(2), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	defer b.Close()
	slow, unsubSlow := b.Subscribe("t")
	fast, unsubFast := b.Subscribe("t")
	defer unsubSlow()
	defer unsubFast()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			b.Publish(context.Background(), "t", i)
			if v := recv(t, fast); v != i {
				t.Errorf("fast got %v want %d", v, i)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if got := b.Dropped(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
	if recv(t, slow) != 0 || recv(t, slow) != 1 {
		t.Fatal("slow subscriber should keep the first buffered messages")
	}
	if !strings.Contains(logs.String(), "dropping message") {
		t.Fatalf("expected drop warning, got %q", logs.String())
	}
}

func TestUnsubscribe(t *testing.T) {
	b := New()
	defer b.Close()
	ch, unsub := b.Subscribe("t")
	unsub()
	unsub() // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
	b.Publish(context.Background(), "t", 1) // must not panic
	if st := b.Stats(); st.Subscribers != 0 {
		t.Fatalf("subscribers = %d", st.Subscribers)
	}
}

func TestClose(t *testing.T) {
	b := New()
	ch, unsub := b.Subscribe("t")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed")
	}
	unsub() // after Close: no panic
	b.Publish(context.Background(), "t", 1)
	late, lateUnsub := b.Subscribe("t")
	defer lateUnsub()
	if _, ok := <-late; ok {
		t.Fatal("subscribe after close must return a closed channel")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	b := New()
	defer b.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(ctx, "t", 1)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		ch, unsub := b.Subscribe("t")
		go func() {
			for range ch {
			}
		}()
		if i%2 == 0 {
			unsub()
		} else {
			defer unsub()
		}
	}
	close(stop)
	wg.Wait()
}

func TestWait(t *testing.T) {
	b := New()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type res struct {
		v   any
		err error
	}
	out := make(chan res, 1)
	go func() {
		v, err := Wait(ctx, b, core.TopicRunUpdated, func(v any) bool {
			r, ok := v.(*core.Run)
			return ok && r.State.Terminal()
		})
		out <- res{v, err}
	}()
	// Wait until the waiter is subscribed.
	for b.Stats().Subscribers == 0 {
		time.Sleep(time.Millisecond)
	}
	b.Publish(ctx, core.TopicRunUpdated, &core.Run{FactoryID: "fac_test", ID: "r", State: core.RunRunning})
	b.Publish(ctx, core.TopicRunUpdated, &core.Run{FactoryID: "fac_test", ID: "r", State: core.RunSucceeded})
	r := <-out
	if r.err != nil || r.v.(*core.Run).State != core.RunSucceeded {
		t.Fatalf("Wait = %+v", r)
	}
	if b.Stats().Subscribers != 0 {
		t.Fatal("Wait must unsubscribe")
	}
}

func TestWaitTimeoutAndClose(t *testing.T) {
	b := New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Wait(ctx, b, "t", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	go func() {
		for b.Stats().Subscribers == 0 {
			time.Sleep(time.Millisecond)
		}
		b.Close()
	}()
	if _, err := Wait(context.Background(), b, "t", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitFuncCheck(t *testing.T) {
	b := New()
	defer b.Close()
	v, err := WaitFunc(context.Background(), b, "t", nil, func() (any, bool, error) { return "already", true, nil })
	if err != nil || v != "already" {
		t.Fatalf("WaitFunc = %v, %v", v, err)
	}
	boom := errors.New("boom")
	if _, err := WaitFunc(context.Background(), b, "t", nil, func() (any, bool, error) { return nil, false, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewBusInterface(t *testing.T) {
	var bus core.EventBus = NewBus() //nolint:staticcheck // ST1023: the declared type checks that the bus implements core.EventBus
	ch, unsub := bus.Subscribe("t")
	defer unsub()
	bus.Publish(context.Background(), "t", "x")
	if recv(t, ch) != "x" {
		t.Fatal("NewBus must work as core.EventBus")
	}
}
