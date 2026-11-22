package hostcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/machine/config"
)

func write(t *testing.T, root, p, data string) {
	t.Helper()
	full := filepath.Join(root, p)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeEnv(t *testing.T, cmds map[string]string) Env {
	root := t.TempDir()
	write(t, root, "/sys/fs/cgroup/cgroup.controllers", "cpu memory pids io")
	write(t, root, "/proc/meminfo", "MemTotal:       65536000 kB\n")
	write(t, root, "/proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nens3\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\nens3\t0000000A\t00000000\t0001\t0\t0\t0\t0000FFFF\n")
	return Env{
		Root:  root,
		KVM:   func() error { return nil },
		Uname: func() (string, string) { return "6.18.2-generic", "x86_64" },
		LookPath: func(n string) (string, error) {
			if _, ok := cmds["path:"+n]; ok {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("missing")
		},
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			key := filepath.Base(name) + " " + strings.Join(args, " ")
			if out, ok := cmds[key]; ok {
				return out, nil
			}
			return "", errors.New("no command " + key)
		},
	}
}

func status(r Result, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestDoctorHealthy(t *testing.T) {
	cmds := map[string]string{
		"path:nft": "", "path:ip": "", "path:mkfs.ext4": "", "path:git": "",
		"firecracker --version": "Firecracker v1.18.0\n", "jailer --version": "Jailer v1.18.0\n",
	}
	e := fakeEnv(t, cmds)
	c := config.Default()
	c.Slots = 1
	c.Slot.VCPUs = 1
	c.DataDir = t.TempDir()
	c.Kernel = filepath.Join(c.DataDir, "vmlinux")
	_ = os.WriteFile(c.Kernel, []byte("k"), 0o444)
	r := Doctor(context.Background(), e, c)
	if r.Failed() {
		t.Fatalf("checks failed: %+v", r.Checks)
	}
	// The 10.0.0.0/16 route does not overlap 10.213.0.0/16.
	if status(r, "subnet").Status != OK {
		t.Fatalf("subnet %+v", status(r, "subnet"))
	}
}

func TestDoctorProblems(t *testing.T) {
	cmds := map[string]string{
		"path:iptables": "", "iptables -S FORWARD": "-P FORWARD DROP\n-N DOCKER-USER\n",
		"path:ufw": "", "ufw status": "Status: active\n",
	}
	e := fakeEnv(t, cmds)
	e.KVM = func() error { return errors.New("no /dev/kvm") }
	e.Uname = func() (string, string) { return "5.10.0", "x86_64" }
	write(t, e.Root, "/sys/kernel/mm/ksm/run", "1\n")
	write(t, e.Root, "/proc/swaps", "Filename Type\n/swap file\n")
	write(t, e.Root, "/etc/passwd", "root:x:0:0::/root:/bin/sh\nbad:x:900003:900003::/:/bin/sh\n")
	c := config.Default()
	c.Network.Subnet = "10.0.0.0/16"
	r := Doctor(context.Background(), e, c)
	for name, want := range map[string]Status{
		"kvm": Fail, "kernel": Fail, "tool nft": Fail, "firecracker": Fail, "subnet": Fail, "jail uids": Fail,
		"iptables forward": Warn, "ufw": Warn, "ksm": Warn, "swap": Warn,
	} {
		if got := status(r, name); got.Status != want {
			t.Errorf("%s: %+v, want %s", name, got, want)
		}
	}
	if !r.ForwardDropped || !strings.Contains(status(r, "iptables forward").Fix, "DOCKER-USER") {
		t.Fatalf("forward %+v", status(r, "iptables forward"))
	}
	if !strings.Contains(status(r, "tool nft").Fix, "nftables") {
		t.Fatal("fix names the package")
	}
}

func TestFactsAndHelpers(t *testing.T) {
	e := fakeEnv(t, map[string]string{"firecracker --version": "Firecracker v1.18.0\n"})
	c := config.Default()
	f := Facts(context.Background(), e, c, "1.0.0")
	if f.Arch != "amd64" || !f.KVM || f.MemoryMiB != 64000 || f.FirecrackerVersion != "Firecracker v1.18.0" || f.RuntimeVersion != "1.0.0" || len(f.Warnings) != 0 {
		t.Fatalf("facts %+v", f)
	}
	if maj, min := KernelVersion("6.1.0-27-amd64"); maj != 6 || min != 1 {
		t.Fatalf("kernel %d.%d", maj, min)
	}
	if GoArch("aarch64") != "arm64" {
		t.Fatal("arch")
	}
}
