package sandbox

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestSelectImage(t *testing.T) {
	spec := core.SandboxSpec{Machine: core.MachineSpec{Image: "machine:1"}, Environment: &core.Environment{Image: "env:1"}}
	if got := SelectImage(spec, "def"); got != "env:1" {
		t.Errorf("environment image: got %q", got)
	}
	spec.Environment = nil
	if got := SelectImage(spec, "def"); got != "machine:1" {
		t.Errorf("machine image: got %q", got)
	}
	spec.Machine.Image = " "
	if got := SelectImage(spec, "def"); got != "def" {
		t.Errorf("default image: got %q", got)
	}
}
