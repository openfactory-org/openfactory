package backend

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct/directtest"
)

// cutProxy forwards TCP to a target and can cut every live connection.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
			go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The microVM backend behind the worker protocol client: a task keeps
// running while the connection is down, ends while disconnected, and the
// reconnect restores its final status in the hub (the fake hub of
// directtest).
func TestV2ReconnectRestoresStatuses(t *testing.T) {
	e := newEnv(t, nil)
	hub := directtest.New(t, directtest.Options{APIKey: "test-key", HeartbeatMS: 100})
	proxy := newCutProxy(t, hub.Addr())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = direct.RunWorkerV2(ctx, direct.WorkerV2Options{
			ServerURL: "http://" + proxy.ln.Addr().String(), APIKey: "test-key", WorkerID: "build-01",
			Capacity: 2, Slots: 2, Backend: e.b, MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		})
	}()
	eventually(t, "connect", hub.Connected)

	task := testTask(1)
	st, err := hub.Assign(context.Background(), task, nil)
	if err != nil || st.State != rtcore.TaskAccepted {
		t.Fatalf("assign = %+v, %v", st, err)
	}
	eventually(t, "running in the hub", func() bool {
		s, ok := hub.Status(task.TaskID)
		return ok && s.State == rtcore.TaskRunning
	})

	// Cut the connection; the VM ends while the worker is offline.
	proxy.cut()
	eventually(t, "disconnect", func() bool { return !hub.Connected() })
	if s, ok := hub.Status(task.TaskID); !ok || s.State != rtcore.TaskRunning {
		t.Fatalf("status while disconnected = %+v, %v", s, ok)
	}
	code := 3
	e.vmm.vm(vm.VMID(0, task.TaskID)).end(vm.Exit{Report: &ctlstream.Frame{Phase: ctlstream.ExitPhaseRunner, ExitCode: &code}})

	eventually(t, "restored status", func() bool {
		s, ok := hub.Status(task.TaskID)
		return ok && hub.Connections() >= 2 && s.State == rtcore.TaskExited && s.Reason == rtcore.SandboxProcessFailed
	})
	if err := hub.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "destroyed on the worker", func() bool {
		l, _ := e.b.List(context.Background())
		return len(l) == 0
	})
}
