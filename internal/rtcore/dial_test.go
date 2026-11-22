package rtcore

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitDial(t *testing.T) {
	// A listener that opens after a delay.
	sock := filepath.Join(t.TempDir(), "s.sock")
	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			ready <- nil
			return
		}
		ready <- ln
	}()
	ctx := context.Background()
	if err := WaitDial(ctx, "unix", sock, 2*time.Second); err != nil {
		t.Fatalf("late listener: %v", err)
	}
	if ln := <-ready; ln != nil {
		_ = ln.Close()
	}

	// TCP listener that is already open.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := WaitDial(ctx, "tcp", addr, time.Second); err != nil {
		t.Fatalf("open listener: %v", err)
	}
	_ = ln.Close()

	// No listener: the timeout wraps the dial error.
	start := time.Now()
	err = WaitDial(ctx, "unix", filepath.Join(t.TempDir(), "none.sock"), 100*time.Millisecond)
	if err == nil {
		t.Fatal("no listener: want an error")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("timeout returned a context error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}

	// A cancelled context ends the wait with ctx.Err().
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err = WaitDial(cctx, "unix", filepath.Join(t.TempDir(), "none.sock"), 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled", err)
	}
}
