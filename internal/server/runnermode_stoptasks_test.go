package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/dispatch"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// listBackend is a task backend that holds two tasks and records Destroy.
type listBackend struct {
	core.TaskBackend
	mu        sync.Mutex
	destroyed []string
}

func (b *listBackend) List(context.Context) ([]core.TaskStatus, error) {
	return []core.TaskStatus{{TaskID: "run_a.1"}, {TaskID: "run_b.1"}}, nil
}

func (b *listBackend) Destroy(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroyed = append(b.destroyed, id)
	return nil
}

// A Job of the kubernetes backend, or a task of a plugin, belongs to every
// replica of the control plane. Stopping a replica (a restart, a rolling
// update) must leave them running. Only the local backend, whose tasks are
// children of this process, stops its tasks.
func TestStopTasksOnlyForLocalBackend(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		backend string
		want    int
	}{
		{config.BackendLocal, 2},
		{config.BackendKubernetes, 0},
		{"acme-tasks", 0}, // a plugin backend
	} {
		cfg := &config.Config{SandboxBackend: tc.backend}
		b := &listBackend{}
		m := &runnerMode{backend: b, ownsTasks: backendOwnsTasks(cfg)}
		m.stopTasks(log)
		if len(b.destroyed) != tc.want {
			t.Errorf("backend %q: destroyed %v, want %d tasks", tc.backend, b.destroyed, tc.want)
		}
	}
}

// When the loop of the dispatcher ends (a clean stop), it gives the dispatch
// lease up. Else the next replica waits for the end of the lease before it
// claims a run.
func TestRunnerLoopReleasesDispatchLease(t *testing.T) {
	mem := testdb.New(t)
	d, err := dispatch.New(dispatch.Config{
		Store: mem, Runner: mem, Backend: &listBackend{}, ControlURL: "http://control.test", Image: "img", ReplicaID: "replica-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &runnerMode{dispatcher: d}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.loop(ctx, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	// Wait until the loop holds the lease.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ok, _ := mem.AcquireLeadership(context.Background(), "dispatch", "replica-b", time.Now(), time.Now().Add(time.Minute)); !ok {
			break
		}
		_ = mem.ReleaseLeadership(context.Background(), "dispatch", "replica-b")
		if time.Now().After(deadline) {
			t.Fatal("the loop never took the lease")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	now := time.Now()
	if ok, err := mem.AcquireLeadership(context.Background(), "dispatch", "replica-b", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("the lease is still held after the loop ended: ok=%v err=%v", ok, err)
	}
}
