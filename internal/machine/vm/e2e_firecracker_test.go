//go:build e2e_firecracker

package vm

// Integration tests on a Linux host with KVM, root and the pinned
// Firecracker. They skip unless OF_E2E_FIRECRACKER=1. Variables:
//
//	OF_E2E_FIRECRACKER_DIR  directory with firecracker and jailer v1.18.0
//	OF_E2E_KERNEL           guest vmlinux
//	OF_E2E_TOOLS            tools disk (vm-init and the runtime)
//	OF_E2E_IMAGE            ext4 image disk of a sandbox image
//	OF_E2E_SCRATCH          empty ext4 scratch template
//
// Run: sudo -E go test -tags e2e_firecracker ./internal/machine/... -count=1

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

func e2eEnv(t *testing.T) (*Firecracker, Spec) {
	t.Helper()
	if os.Getenv("OF_E2E_FIRECRACKER") != "1" {
		t.Skip("set OF_E2E_FIRECRACKER=1 on a KVM host to run the Firecracker tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("the Firecracker tests need root")
	}
	need := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("%s is not set", k)
		}
		return v
	}
	fcDir := need("OF_E2E_FIRECRACKER_DIR")
	base, err := os.MkdirTemp("/var/tmp", "ofe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	data, _ := os.ReadFile("/proc/self/cgroup")
	self, err := SelfCgroup(string(data))
	if err != nil {
		t.Fatal(err)
	}
	cg := Cgroups{Root: "/sys/fs/cgroup"}
	parent := filepath.Join(self, "ofe2e")
	_ = os.MkdirAll(filepath.Join(cg.Root, parent), 0o755)
	f := &Firecracker{
		Jail: JailerConfig{
			Jailer: filepath.Join(fcDir, "jailer"), Firecracker: filepath.Join(fcDir, "firecracker"),
			ChrootBase: filepath.Join(base, "jail"), ParentCgroup: parent, CgroupRoot: cg.Root,
		},
		Cgroups: cg,
	}
	tap := "ofe2e0"
	_ = execRun("ip", "tuntap", "add", "dev", tap, "mode", "tap")
	_ = execRun("ip", "addr", "add", "10.254.0.1/30", "dev", tap)
	_ = execRun("ip", "link", "set", tap, "up")
	t.Cleanup(func() { _ = execRun("ip", "link", "del", tap) })
	return f, Spec{
		VMID: "vm-0-e2e00000", Arch: runtime.GOARCH, Slot: 0, UID: 912345, GID: 912345, VCPUs: 1, MemoryMiB: 512, ScratchGiB: 1,
		Kernel: need("OF_E2E_KERNEL"), Tools: need("OF_E2E_TOOLS"), Image: need("OF_E2E_IMAGE"), Scratch: need("OF_E2E_SCRATCH"),
		Tap: tap, GuestMAC: net.HardwareAddr{6, 0, 10, 254, 0, 2},
	}
}

func execRun(name string, args ...string) error {
	_, err := osExec(name, args...)
	return err
}

func e2eDoc(exec string) mmds.Document {
	return mmds.Document{OpenFactory: mmds.Payload{
		V: 1, TaskID: "run_e2e.1", Hostname: "of-e2e", ClockUnixNs: time.Now().UnixNano(), MachineID: strings.Repeat("a", 32),
		User: &mmds.User{UID: 1001, GID: 1001, Home: "/home/agent"},
		Env: map[string]string{
			"OF_CONTROL_URL": "https://control.invalid", "OF_RUN_ID": "run_e2e", "OF_SESSION": "1", "OF_BOOTSTRAP_TOKEN": "ofb_test",
			"OF_RUNNER_EXEC_COMMAND": exec,
		},
		Network:           &mmds.Network{Mode: "restricted", Address: "10.254.0.2/30", Gateway: "10.254.0.1", MTU: 1500, DNS: []string{}},
		Disks:             &mmds.Disks{Image: "image", Scratch: "scratch"},
		MaxSessionSeconds: 120,
	}}
}

// The VM boots, vm-init reads MMDS, starts the runner and reports its exit.
func TestE2EBootAndExitReport(t *testing.T) {
	f, s := e2eEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := f.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Cleanup(context.Background())
	if err := v.Go(ctx, e2eDoc("true")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.Running():
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not start")
	}
	select {
	case e := <-v.Wait():
		if e.Report == nil {
			t.Fatalf("no exit report: %+v", e)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the VM did not end")
	}
}

// Cancel with grace reaches the runner and the VM ends within grace + 5 s.
func TestE2ECancel(t *testing.T) {
	f, s := e2eEnv(t)
	s.VMID = "vm-0-e2e00001"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := f.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Cleanup(context.Background())
	if err := v.Go(ctx, e2eDoc("sleep 600")); err != nil {
		t.Fatal(err)
	}
	<-v.Running()
	start := time.Now()
	if err := v.Cancel(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.Wait():
		if time.Since(start) > 15*time.Second {
			t.Fatalf("cancel took %s", time.Since(start))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the VM did not end after cancel")
	}
}
