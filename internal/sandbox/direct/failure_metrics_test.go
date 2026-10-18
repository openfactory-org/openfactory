package direct

import (
	"context"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/worker"
)

func TestFailurePhase(t *testing.T) {
	for reason, want := range map[core.SandboxReason]string{
		core.SandboxImagePull: "setup", core.SandboxUnschedulable: "setup", core.SandboxInvalidSpec: "setup",
		core.SandboxContainerOOM: "run", core.SandboxEvicted: "run", core.SandboxNodeLost: "run",
		core.SandboxDeadlineExceeded: "run", core.SandboxProcessFailed: "run", core.SandboxUnknown: "run",
		core.SandboxCompleted: "", core.SandboxCancelled: "", "": "",
	} {
		phase, ok := failurePhase(reason)
		if phase != want || ok != (want != "") {
			t.Errorf("failurePhase(%q) = %q, %v", reason, phase, ok)
		}
	}
}

func TestTaskResult(t *testing.T) {
	for reason, want := range map[core.SandboxReason]string{
		core.SandboxCompleted: worker.ResultSucceeded, core.SandboxCancelled: worker.ResultCancelled,
		core.SandboxProcessFailed: worker.ResultFailed, core.SandboxImagePull: worker.ResultFailed,
		core.SandboxEvicted: worker.ResultFailed, core.SandboxUnknown: worker.ResultFailed, "": worker.ResultFailed,
	} {
		if got := taskResult(reason); got != want {
			t.Errorf("taskResult(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestV2TaskFailureMetricHasPhaseRun checks that a sandbox that ends after it
// started (eviction) counts in of_worker_sandbox_failures_total with phase "run".
func TestV2TaskFailureMetricHasPhaseRun(t *testing.T) {
	m := worker.NewMetrics("edge-1", "fake", "test", 0)
	e := startV2(t, v2Opts{metrics: m})
	for i, reason := range []core.SandboxReason{core.SandboxEvicted, core.SandboxImagePull, core.SandboxCompleted} {
		task := testTask(i + 1)
		if _, err := e.hb.Start(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		e.backend.set(core.TaskStatus{TaskID: task.TaskID, State: core.TaskRunning})
		e.backend.set(exited(task.TaskID, reason))
	}
	eventually(t, "failure metrics", func() bool {
		return m.Value(worker.MetricTaskFailures, "run", "evicted") == 1 && m.Value(worker.MetricTaskFailures, "setup", "image_pull") == 1
	})
	eventually(t, "three tasks done", func() bool {
		return m.Value(worker.MetricTasksDone, worker.ResultSucceeded) == 1 && m.Value(worker.MetricTasksDone, worker.ResultFailed) == 2
	})
	if v := m.Value(worker.MetricTasksDone, "completed"); v != 0 {
		t.Errorf("the end reason is a result label: %v", v)
	}
	if v := m.Value(worker.MetricTaskFailures, "run", "completed"); v != 0 {
		t.Errorf("a completed task counts as a failure: %v", v)
	}
}
