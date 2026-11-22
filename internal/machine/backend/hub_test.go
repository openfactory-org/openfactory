package backend

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

type hubStore struct{ core.Store }

func (hubStore) UpsertWorker(context.Context, *core.Worker) error { return nil }

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
// reconnect restores its final status in the hub (the port of
// TestV2ReconnectRestoresStatuses).
func TestV2ReconnectRestoresStatuses(t *testing.T) {
	e := newEnv(t, nil)
	hub := direct.NewHub(hubStore{}, direct.HubOptions{
		HeartbeatInterval: 100 * time.Millisecond,
		Authenticate: func(_ context.Context, token string) (string, string, error) {
			if token == "test-key" {
				return "team_1", "usr_1", nil
			}
			return "", "", core.ErrUnauthorized
		},
	})
	mux := http.NewServeMux()
	hub.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	proxy := newCutProxy(t, srv.Listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = direct.RunWorkerV2(ctx, direct.WorkerV2Options{
			ServerURL: "http://" + proxy.ln.Addr().String(), APIKey: "test-key", WorkerID: "build-01",
			Capacity: 2, Slots: 2, Backend: e.b, MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		})
	}()
	hb := hub.WorkerBackend("build-01")
	eventually(t, "connect", func() bool { return hub.Connected("build-01") })

	task := testTask(1)
	st, err := hb.Start(context.Background(), task)
	if err != nil || st.State != rtcore.TaskAccepted {
		t.Fatalf("start = %+v, %v", st, err)
	}
	eventually(t, "running in the hub", func() bool {
		s, err := hb.Status(context.Background(), task.TaskID)
		return err == nil && s.State == rtcore.TaskRunning
	})

	// Cut the connection; the VM ends while the worker is offline.
	proxy.cut()
	eventually(t, "disconnect", func() bool { return !hub.Connected("build-01") })
	if s, err := hb.Status(context.Background(), task.TaskID); err != nil || s.State != rtcore.TaskRunning {
		t.Fatalf("status while disconnected = %+v, %v", s, err)
	}
	code := 3
	e.vmm.vm(vm.VMID(0, task.TaskID)).end(vm.Exit{Report: &ctlstream.Frame{Phase: ctlstream.ExitPhaseRunner, ExitCode: &code}})

	eventually(t, "restored status", func() bool {
		s, err := hb.Status(context.Background(), task.TaskID)
		return err == nil && s.State == rtcore.TaskExited && s.Reason == rtcore.SandboxProcessFailed
	})
	if err := hb.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "destroyed on the worker", func() bool {
		l, _ := e.b.List(context.Background())
		return len(l) == 0
	})
}
