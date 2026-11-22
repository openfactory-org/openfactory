package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
)

func TestKeyChangesWithEveryPart(t *testing.T) {
	base := KeyParts{FirecrackerVersion: "v1.18.0", KernelSHA256: "k", ToolsSHA256: "t", ImageDigest: "sha256:a", VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 40, HostCPUModel: "cpu", HostKernel: "6.18.1"}
	k := Key(base)
	muts := []func(*KeyParts){
		func(p *KeyParts) { p.FirecrackerVersion = "v1.18.1" },
		func(p *KeyParts) { p.ImageDigest = "sha256:b" },
		func(p *KeyParts) { p.MemoryMiB = 8192 },
		func(p *KeyParts) { p.HostCPUModel = "other" },
		func(p *KeyParts) { p.HostKernel = "6.1.0" },
	}
	for i, m := range muts {
		p := base
		m(&p)
		if Key(p) == k {
			t.Errorf("mutation %d keeps the key", i)
		}
	}
	if Key(base) != k || len(k) != 64 {
		t.Fatal("key is not stable")
	}
}

func TestCPUModel(t *testing.T) {
	if got := CPUModel("processor\t: 0\nmodel name\t: AMD EPYC 9654\nprocessor\t: 1\nmodel name\t: AMD EPYC 9654\n"); got != "AMD EPYC 9654" {
		t.Fatalf("x86 %q", got)
	}
	if got := CPUModel("processor\t: 0\nCPU implementer\t: 0x41\nCPU part\t: 0xd0c\n"); got != "0x41/0xd0c" {
		t.Fatalf("arm %q", got)
	}
}

type fakeTemplateVM struct {
	dir     string
	cleaned atomic.Bool
}

func (f *fakeTemplateVM) ID() string                                  { return "vm-255-template" }
func (f *fakeTemplateVM) StreamKey() string                           { return "" }
func (f *fakeTemplateVM) Go(context.Context, mmds.Document) error     { return nil }
func (f *fakeTemplateVM) Running() <-chan struct{}                    { return nil }
func (f *fakeTemplateVM) Cancel(context.Context, time.Duration) error { return nil }
func (f *fakeTemplateVM) Kill(context.Context) error                  { return nil }
func (f *fakeTemplateVM) Wait() <-chan vm.Exit                        { return nil }
func (f *fakeTemplateVM) Cleanup(context.Context) error               { f.cleaned.Store(true); return nil }
func (f *fakeTemplateVM) Snapshot(context.Context) (vm.SnapshotFiles, error) {
	out := vm.SnapshotFiles{State: filepath.Join(f.dir, "vm.state"), Mem: filepath.Join(f.dir, "mem"), Scratch: filepath.Join(f.dir, "scratch.ext4")}
	for _, p := range []string{out.State, out.Mem, out.Scratch} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o600); err != nil {
			return out, err
		}
	}
	return out, nil
}

type fakeVMM struct {
	spec vm.Spec
	vm   *fakeTemplateVM
}

func (f *fakeVMM) Start(_ context.Context, s vm.Spec) (vm.VM, error)        { f.spec = s; return f.vm, nil }
func (f *fakeVMM) Adopt(context.Context, []vm.AdoptRecord) ([]vm.VM, error) { return nil, nil }

func TestBuildAndGet(t *testing.T) {
	jail := t.TempDir()
	fv := &fakeTemplateVM{dir: jail}
	vmm := &fakeVMM{vm: fv}
	released := false
	m := &Manager{Dir: t.TempDir(), VMM: vmm, Settle: time.Millisecond, Net: func(context.Context) (string, func(), error) {
		return "oftpl0", func() { released = true }, nil
	}}
	if m.Get("k1") != nil {
		t.Fatal("template before build")
	}
	if err := m.Build(context.Background(), "k1", vm.Spec{VMID: "vm-255-template", Cache: []string{"x"}, Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if !vmm.spec.Template || vmm.spec.Cache != nil || vmm.spec.Tap != "oftpl0" || vmm.spec.GuestMAC.String() != vm.TemplateMAC.String() {
		t.Fatalf("template spec %+v", vmm.spec)
	}
	f := m.Get("k1")
	if f == nil || !fv.cleaned.Load() || !released {
		t.Fatal("template not stored, VM not cleaned or tap not released")
	}
	if fi, _ := os.Stat(f.Mem); fi.Mode().Perm() != 0o400 {
		t.Fatalf("mem mode %v", fi.Mode())
	}
	m.Prune(func(k string) bool { return k != "k1" })
	if m.Get("k1") != nil {
		t.Fatal("prune")
	}
}
