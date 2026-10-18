package egress

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWaitContainer(t *testing.T) {
	c := Wait(WaitParams{Image: "example.test/runner:1", PullPolicy: "Never"})
	if c["name"] != WaitName || c["image"] != "example.test/runner:1" || c["imagePullPolicy"] != "Never" {
		t.Fatalf("container = %v", c)
	}
	want := []any{"openfactory", "netprobe", "wait-blocked", "--host", "$(OF_EGRESS_WAIT_HOST)", "--port", "10250", "--timeout", "30s"}
	if !reflect.DeepEqual(c["command"], want) {
		t.Errorf("command = %v", c["command"])
	}
	if c["restartPolicy"] != nil {
		t.Error("the wait must end before the next init container starts")
	}
	if len(c["env"].([]any)) != 1 {
		t.Errorf("env = %v", c["env"])
	}
}

func TestWaitContainerTimeoutAndAddr(t *testing.T) {
	c := Wait(WaitParams{Image: "i", Timeout: 90 * time.Second, Addr: "10.1.2.3:9999"})
	want := []any{"openfactory", "netprobe", "wait-blocked", "--addr", "10.1.2.3:9999", "--timeout", "1m30s"}
	if !reflect.DeepEqual(c["command"], want) {
		t.Errorf("command = %v", c["command"])
	}
	if _, ok := c["env"]; ok {
		t.Error("a fixed target needs no env")
	}
}

func TestWithWait(t *testing.T) {
	other := map[string]any{"name": "copy"}
	old := map[string]any{"name": WaitName, "image": "old"}
	out := WithWait([]any{other, old}, Wait(WaitParams{Image: "new"}))
	if len(out) != 2 || out[0].(map[string]any)["image"] != "new" || out[1].(map[string]any)["name"] != "copy" {
		t.Fatalf("out = %v", out)
	}
}

func TestWaitTargetIsNotAProxyOrLockedPort(t *testing.T) {
	// The egress lock refuses ports 80 and 443 from the first moment. A probe
	// on them would see the lock and not the NetworkPolicy.
	if WaitPort == 80 || WaitPort == 443 || WaitPort == ProxyPort || WaitPort == ReportPort || WaitPort == 53 {
		t.Fatalf("port %d is open or locked by the egress setup", WaitPort)
	}
}

func fakeDial(results ...error) (func(context.Context, string, time.Duration) error, *int) {
	n := 0
	return func(ctx context.Context, addr string, timeout time.Duration) error {
		i := n
		n++
		if i >= len(results) {
			i = len(results) - 1
		}
		return results[i]
	}, &n
}

func TestWaitBlockedEndsAfterConfirmedFailures(t *testing.T) {
	// Open, open, then blocked: the wait needs two failures in a row.
	dial, n := fakeDial(nil, nil, errors.New("refused"), errors.New("refused"))
	took, err := WaitBlocked(context.Background(), "10.0.0.1:10250", WaitOptions{
		Timeout: 5 * time.Second, Interval: time.Millisecond, Dial: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if *n != 4 {
		t.Errorf("dials = %d, want 4", *n)
	}
	if took <= 0 || took > time.Second {
		t.Errorf("took %s", took)
	}
}

func TestWaitBlockedOneFailureIsNotEnough(t *testing.T) {
	// A lost packet before the policy is active must not end the wait.
	dial, n := fakeDial(errors.New("timeout"), nil, errors.New("refused"), nil, errors.New("refused"), errors.New("refused"))
	if _, err := WaitBlocked(context.Background(), "x:1", WaitOptions{Timeout: 5 * time.Second, Interval: time.Millisecond, Dial: dial}); err != nil {
		t.Fatal(err)
	}
	if *n != 6 {
		t.Errorf("dials = %d, want 6", *n)
	}
}

func TestWaitBlockedAlreadyBlocked(t *testing.T) {
	dial, n := fakeDial(errors.New("refused"))
	if _, err := WaitBlocked(context.Background(), "x:1", WaitOptions{Interval: time.Millisecond, Dial: dial}); err != nil {
		t.Fatal(err)
	}
	if *n != WaitConfirm {
		t.Errorf("dials = %d, want %d", *n, WaitConfirm)
	}
}

func TestWaitBlockedTimesOut(t *testing.T) {
	dial, _ := fakeDial(nil)
	start := time.Now()
	_, err := WaitBlocked(context.Background(), "10.0.0.1:10250", WaitOptions{
		Timeout: 150 * time.Millisecond, Interval: 10 * time.Millisecond, Dial: dial,
	})
	if !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("err = %v, want ErrNotBlocked", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.1:10250") {
		t.Errorf("the error does not name the target: %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 3*time.Second {
		t.Errorf("took %s: the wait must be bounded by its timeout", d)
	}
}

// A dial that ends because the time is over says nothing: it must not count as
// "blocked".
func TestWaitBlockedFailureAtTheDeadlineIsNotBlocked(t *testing.T) {
	dial := func(ctx context.Context, addr string, timeout time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := WaitBlocked(context.Background(), "x:1", WaitOptions{Timeout: 50 * time.Millisecond, Interval: time.Millisecond, Dial: dial})
	if !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("err = %v, want ErrNotBlocked", err)
	}
}

func TestWaitBlockedStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dial, _ := fakeDial(nil)
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := WaitBlocked(ctx, "x:1", WaitOptions{Timeout: time.Minute, Interval: 5 * time.Millisecond, Dial: dial})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotBlocked) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// With the real dial: a listener is "open", a closed port is "blocked".
func TestWaitBlockedRealDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, err = WaitBlocked(context.Background(), addr, WaitOptions{Timeout: 200 * time.Millisecond, Interval: 10 * time.Millisecond})
	if !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("a listener must count as open: %v", err)
	}
	_ = ln.Close()
	if _, err := WaitBlocked(context.Background(), addr, WaitOptions{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}); err != nil {
		t.Fatalf("a closed port must count as blocked: %v", err)
	}
}
