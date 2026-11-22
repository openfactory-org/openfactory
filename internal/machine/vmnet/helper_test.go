package vmnet

import (
	"context"
	"net"
	"net/netip"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// TestHelperStopDuringBackoff stops the helper while it waits to restart a
// process that exited. No process may start after Stop.
func TestHelperStopDuringBackoff(t *testing.T) {
	exe, err := exec.LookPath("true")
	if err != nil {
		t.Skip(err)
	}
	var launches atomic.Int32
	h := newHelper(HelperSpec{VM: VMNet{Tap: "oftest0"}}, HelperOptions{
		Backoff: 300 * time.Millisecond,
		Start: func([]string, int) (*exec.Cmd, error) {
			launches.Add(1)
			cmd := exec.Command(exe)
			return cmd, cmd.Start()
		},
	})
	if err := h.launch(); err != nil {
		t.Fatal(err)
	}
	go h.supervise()
	time.Sleep(100 * time.Millisecond) // the process exited; supervise waits
	h.Stop()
	time.Sleep(500 * time.Millisecond)
	if n := launches.Load(); n != 1 {
		t.Fatalf("helper started %d times; no start may follow Stop", n)
	}
	select {
	case <-h.done:
	default:
		t.Fatal("supervise still runs after Stop")
	}
}

// TestHelperLaunchAfterStop checks that a process started while Stop ran is
// ended at once.
func TestHelperLaunchAfterStop(t *testing.T) {
	exe, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip(err)
	}
	var cmd *exec.Cmd
	h := newHelper(HelperSpec{}, HelperOptions{Start: func([]string, int) (*exec.Cmd, error) {
		cmd = exec.Command(exe, "30")
		return cmd, cmd.Start()
	}})
	h.stopped = true
	if err := h.launch(); err == nil {
		t.Fatal("launch after Stop must fail")
	}
	if cmd.ProcessState == nil {
		t.Fatal("the process started after Stop still runs")
	}
}

// TestDNSRelayBoundsInflight sends more queries than MaxInflight to an
// upstream that never answers. The relay forwards only MaxInflight of them.
func TestDNSRelayBoundsInflight(t *testing.T) {
	up, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer up.Close()
	var got atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, err := up.ReadFrom(buf); err != nil {
				return
			}
			got.Add(1)
		}
	}()
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	listen := pc.LocalAddr().String()
	_ = pc.Close()
	r := &DNSRelay{
		Listen: listen, Upstreams: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		port: up.LocalAddr().(*net.UDPAddr).Port, Timeout: 3 * time.Second, MaxInflight: 2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	if err := rtcore.WaitDial(ctx, "tcp", listen, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp4", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Send bursts: the upstream never answers, so the first two exchanges
	// hold both slots for Timeout and the relay drops the other queries.
	for burst := 0; burst < 5; burst++ {
		for i := 0; i < 10; i++ {
			_, _ = c.Write([]byte("q"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := got.Load(); n != 2 {
		t.Fatalf("upstream got %d queries, want 2 (MaxInflight)", n)
	}
}
