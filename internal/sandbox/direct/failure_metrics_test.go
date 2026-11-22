package direct

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/worker"
)

func TestFailurePhase(t *testing.T) {
	for reason, want := range map[rtcore.SandboxReason]string{
		rtcore.SandboxImagePull: "setup", rtcore.SandboxUnschedulable: "setup", rtcore.SandboxInvalidSpec: "setup",
		rtcore.SandboxContainerOOM: "run", rtcore.SandboxEvicted: "run", rtcore.SandboxNodeLost: "run",
		rtcore.SandboxDeadlineExceeded: "run", rtcore.SandboxProcessFailed: "run", rtcore.SandboxUnknown: "run",
		rtcore.SandboxCompleted: "", rtcore.SandboxCancelled: "", "": "",
	} {
		phase, ok := failurePhase(reason)
		if phase != want || ok != (want != "") {
			t.Errorf("failurePhase(%q) = %q, %v", reason, phase, ok)
		}
	}
}

func TestTaskResult(t *testing.T) {
	for reason, want := range map[rtcore.SandboxReason]string{
		rtcore.SandboxCompleted: worker.ResultSucceeded, rtcore.SandboxCancelled: worker.ResultCancelled,
		rtcore.SandboxProcessFailed: worker.ResultFailed, rtcore.SandboxImagePull: worker.ResultFailed,
		rtcore.SandboxEvicted: worker.ResultFailed, rtcore.SandboxUnknown: worker.ResultFailed, "": worker.ResultFailed,
	} {
		if got := taskResult(reason); got != want {
			t.Errorf("taskResult(%q) = %q, want %q", reason, got, want)
		}
	}
}
