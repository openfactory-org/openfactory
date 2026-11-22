//go:build unix

package tasklocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func newBackendOpts(t *testing.T, o Options) *Backend {
	t.Helper()
	if o.Root == "" {
		o.Root = t.TempDir()
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newBackend(t *testing.T, script string, max int) *Backend {
	t.Helper()
	return newBackendOpts(t, Options{Command: []string{"/bin/sh", "-c", script}, MaxTasks: max})
}

func task(runID string) rtcore.Task {
	return rtcore.Task{
		TaskID:   rtcore.TaskID(runID, 1),
		RunID:    runID,
		Session:  1,
		TeamID:   "team",
		Image:    "local",
		Platform: rtcore.TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			rtcore.TaskEnvControlURL:     "http://127.0.0.1:1",
			rtcore.TaskEnvRunID:          runID,
			rtcore.TaskEnvSession:        "1",
			rtcore.TaskEnvBootstrapToken: "token",
		},
		MaxSessionSeconds: 60,
	}
}

// start starts the task and registers the cleanup.
func start(t *testing.T, b *Backend, tk rtcore.Task) rtcore.TaskStatus {
	t.Helper()
	t.Cleanup(func() { _ = b.Destroy(context.Background(), tk.TaskID) })
	st, err := b.Start(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func waitExited(t *testing.T, b *Backend, taskID string) rtcore.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := b.Status(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == rtcore.TaskExited {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not exit: %+v", taskID, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func taskDir(b *Backend, taskID string) string {
	return filepath.Join(b.opts.Root, dirName(taskID))
}

func TestStartRunsAndCompletes(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	tk := task("r1")
	start(t, b, tk)
	st := waitExited(t, b, tk.TaskID)
	if st.Reason != rtcore.SandboxCompleted || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("unexpected status %+v", st)
	}
	if st.StartedAt == nil || st.EndedAt == nil {
		t.Fatalf("times not set: %+v", st)
	}
}

func TestStartNonZeroExit(t *testing.T) {
	b := newBackend(t, "exit 3", 0)
	tk := task("r1")
	start(t, b, tk)
	st := waitExited(t, b, tk.TaskID)
	if st.Reason != rtcore.SandboxProcessFailed || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestStartPassesEnvAndWorkdir(t *testing.T) {
	b := newBackendOpts(t, Options{
		Command:  []string{"/bin/sh", "-c", `echo "$OF_RUN_ID $OF_SESSION $EXTRA" > out.txt; pwd >> out.txt`},
		ExtraEnv: map[string]string{"EXTRA": "x"},
	})
	tk := task("r1")
	start(t, b, tk)
	waitExited(t, b, tk.TaskID)
	data, err := os.ReadFile(filepath.Join(taskDir(b, tk.TaskID), "workspace", "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "r1 1 x" || !strings.HasSuffix(lines[1], "/workspace") {
		t.Fatalf("unexpected output %q", data)
	}
}

func TestTaskEnvWinsOverExtraEnv(t *testing.T) {
	b := newBackendOpts(t, Options{
		Command:  []string{"/bin/sh", "-c", `echo "$OF_RUN_ID" > out.txt`},
		ExtraEnv: map[string]string{"OF_RUN_ID": "wrong"},
	})
	tk := task("r1")
	start(t, b, tk)
	waitExited(t, b, tk.TaskID)
	data, err := os.ReadFile(filepath.Join(taskDir(b, tk.TaskID), "workspace", "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "r1" {
		t.Fatalf("got %q", data)
	}
}

func TestStartIdempotent(t *testing.T) {
	b := newBackend(t, "sleep 30", 0)
	tk := task("r1")
	first := start(t, b, tk)
	second, err := b.Start(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if first.SandboxID == "" || first.SandboxID != second.SandboxID {
		t.Fatalf("sandbox ids differ: %q %q", first.SandboxID, second.SandboxID)
	}
	list, _ := b.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("list has %d entries", len(list))
	}
}

func TestStartInvalidSpec(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	tk := task("r1")
	tk.Image = ""
	if _, err := b.Start(context.Background(), tk); err == nil {
		t.Fatal("expected an error")
	}
	list, _ := b.List(context.Background())
	if len(list) != 0 {
		t.Fatalf("list has %d entries", len(list))
	}
}

func TestStartMissingProgram(t *testing.T) {
	b := newBackendOpts(t, Options{Command: []string{"/no/such/program"}})
	tk := task("r1")
	st := start(t, b, tk)
	if st.State != rtcore.TaskExited || st.Reason != rtcore.SandboxInvalidSpec || st.Message == "" {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestCapacity(t *testing.T) {
	b := newBackend(t, "sleep 30", 1)
	t1, t2 := task("r1"), task("r2")
	start(t, b, t1)
	t.Cleanup(func() { _ = b.Destroy(context.Background(), t2.TaskID) })
	if _, err := b.Start(context.Background(), t2); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("got %v, want ErrAtCapacity", err)
	}
	if err := b.Destroy(context.Background(), t1.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(context.Background(), t2); err != nil {
		t.Fatal(err)
	}
}

func TestStatusNotFound(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	ctx := context.Background()
	if _, err := b.Status(ctx, "nope.1"); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("status: %v", err)
	}
	if _, err := b.Watch(ctx, "nope.1"); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("watch: %v", err)
	}
	if err := b.Cancel(ctx, "nope.1", 1); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestCancelGraceful(t *testing.T) {
	b := newBackend(t, "trap 'exit 0' TERM; sleep 30 & wait", 0)
	tk := task("r1")
	start(t, b, tk)
	time.Sleep(200 * time.Millisecond) // let the shell install the trap
	begin := time.Now()
	if err := b.Cancel(context.Background(), tk.TaskID, 5); err != nil {
		t.Fatal(err)
	}
	st := waitExited(t, b, tk.TaskID)
	if time.Since(begin) > 2*time.Second || st.Reason != rtcore.SandboxCancelled {
		t.Fatalf("took %v, status %+v", time.Since(begin), st)
	}
}

func TestCancelHardStopAfterGrace(t *testing.T) {
	b := newBackend(t, "trap '' TERM; sleep 30 & wait", 0)
	tk := task("r1")
	start(t, b, tk)
	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	if err := b.Cancel(context.Background(), tk.TaskID, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Cancel(context.Background(), tk.TaskID, 1); err != nil {
		t.Fatal(err)
	}
	st := waitExited(t, b, tk.TaskID)
	if time.Since(begin) > 4*time.Second || st.Reason != rtcore.SandboxCancelled {
		t.Fatalf("took %v, status %+v", time.Since(begin), st)
	}
}

func TestCancelExitedIsNoop(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	tk := task("r1")
	start(t, b, tk)
	waitExited(t, b, tk.TaskID)
	if err := b.Cancel(context.Background(), tk.TaskID, 1); err != nil {
		t.Fatal(err)
	}
	st, _ := b.Status(context.Background(), tk.TaskID)
	if st.Reason != rtcore.SandboxCompleted {
		t.Fatalf("reason changed: %+v", st)
	}
}

func TestDestroyRunningTask(t *testing.T) {
	b := newBackend(t, "sleep 30", 0)
	tk := task("r1")
	start(t, b, tk)
	ctx := context.Background()
	if err := b.Destroy(ctx, tk.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Status(ctx, tk.TaskID); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(taskDir(b, tk.TaskID)); !os.IsNotExist(err) {
		t.Fatalf("task directory still exists: %v", err)
	}
	if err := b.Destroy(ctx, tk.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyKillsChildren(t *testing.T) {
	b := newBackend(t, "sleep 30 & echo $! > child.pid; wait", 0)
	tk := task("r1")
	start(t, b, tk)
	path := filepath.Join(taskDir(b, tk.TaskID), "workspace", "child.pid")
	pid := 0
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 {
		if data, err := os.ReadFile(path); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				pid = n
			}
		}
		if pid == 0 {
			if time.Now().After(deadline) {
				t.Fatal("child.pid not written")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := b.Destroy(context.Background(), tk.TaskID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d is still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatch(t *testing.T) {
	b := newBackend(t, "sleep 0.2; exit 0", 0)
	tk := task("r1")
	start(t, b, tk)
	ch, err := b.Watch(context.Background(), tk.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	next := func() (rtcore.TaskStatus, bool) {
		select {
		case st, ok := <-ch:
			return st, ok
		case <-timeout:
			t.Fatal("timeout")
			return rtcore.TaskStatus{}, false
		}
	}
	st, ok := next()
	if !ok || st.State != rtcore.TaskRunning {
		t.Fatalf("first: %+v %v", st, ok)
	}
	st, ok = next()
	if !ok || st.State != rtcore.TaskExited {
		t.Fatalf("second: %+v %v", st, ok)
	}
	if _, ok = next(); ok {
		t.Fatal("channel not closed")
	}
}

func TestWatchExitedTask(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	tk := task("r1")
	start(t, b, tk)
	waitExited(t, b, tk.TaskID)
	ch, err := b.Watch(context.Background(), tk.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := <-ch
	if !ok || st.State != rtcore.TaskExited {
		t.Fatalf("got %+v %v", st, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed")
	}
}

func TestWatchContextCancel(t *testing.T) {
	b := newBackend(t, "sleep 30", 0)
	tk := task("r1")
	start(t, b, tk)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Watch(ctx, tk.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	<-ch // current status
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected status")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
}

func TestDeadline(t *testing.T) {
	b := newBackendOpts(t, Options{Command: []string{"/bin/sh", "-c", "sleep 30"}, KillGrace: 100 * time.Millisecond})
	tk := task("r1")
	tk.MaxSessionSeconds = 1
	start(t, b, tk)
	begin := time.Now()
	st := waitExited(t, b, tk.TaskID)
	if time.Since(begin) > 4*time.Second || st.Reason != rtcore.SandboxDeadlineExceeded {
		t.Fatalf("took %v, status %+v", time.Since(begin), st)
	}
}

func TestListSorted(t *testing.T) {
	b := newBackend(t, "exit 0", 0)
	for _, id := range []string{"rc", "ra", "rb"} {
		start(t, b, task(id))
	}
	list, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ra.1", "rb.1", "rc.1"}
	if len(list) != 3 {
		t.Fatalf("got %d entries", len(list))
	}
	for i, st := range list {
		if st.TaskID != want[i] {
			t.Fatalf("entry %d is %s, want %s", i, st.TaskID, want[i])
		}
	}
}

func TestPreflight(t *testing.T) {
	ok := newBackendOpts(t, Options{Command: []string{"/bin/sh"}})
	if err := ok.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := newBackendOpts(t, Options{Command: []string{"/no/such/program"}})
	if err := bad.Preflight(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRunnerLog(t *testing.T) {
	b := newBackend(t, "echo hello; echo oops >&2", 0)
	tk := task("r1")
	start(t, b, tk)
	waitExited(t, b, tk.TaskID)
	data, err := os.ReadFile(filepath.Join(taskDir(b, tk.TaskID), "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "oops") {
		t.Fatalf("log is %q", data)
	}
}
