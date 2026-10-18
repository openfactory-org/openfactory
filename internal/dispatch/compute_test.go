package dispatch

import (
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// A session records the instance shape of the agent machine when it is
// created, and the compute seconds when it ends.
func TestSessionRecordsShapeAndCompute(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	run.Config.Machine = &core.MachineSpec{Name: "big", OS: "linux", Arch: "x86_64", VCPUs: 4, MemoryGB: 8}
	if err := e.mem.UpdateRun(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	e.tick()
	s := e.session(run.ID, 1)
	if s.InstanceShape == nil || s.InstanceShape.VCPUs != 4 || s.InstanceShape.MemoryGB != 8 || s.InstanceShape.Label() != "linux/x86_64 4 vCPU 8 GB" {
		t.Fatalf("shape = %+v", s.InstanceShape)
	}
	e.makeActive(run.ID, 1)
	e.advance(90 * time.Second)
	if _, wrote, err := e.d.endSession(e.ctx, run.ID, 1, "idle", ""); err != nil || !wrote {
		t.Fatalf("endSession wrote=%v err=%v", wrote, err)
	}
	if got := e.session(run.ID, 1).ComputeSeconds; got != 90 {
		t.Fatalf("compute seconds = %v, want 90", got)
	}
}

func TestComputeSecondsNeverStarted(t *testing.T) {
	assigned := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := &core.RunSession{AssignedAt: assigned}
	if got := computeSeconds(s, assigned.Add(30*time.Second)); got != 30 {
		t.Fatalf("got %v", got)
	}
	if got := computeSeconds(s, assigned.Add(-time.Second)); got != 0 {
		t.Fatalf("negative span gives %v", got)
	}
}

func TestInstanceShapeLabel(t *testing.T) {
	var nilShape *core.InstanceShape
	if nilShape.Label() != "default" || (&core.InstanceShape{Name: "x"}).Label() != "default" {
		t.Fatal("empty shape must be default")
	}
	if got := (&core.InstanceShape{OS: "macos", VCPUs: 6, MemoryGB: 14}).Label(); got != "macos 6 vCPU 14 GB" {
		t.Fatalf("label = %q", got)
	}
}
