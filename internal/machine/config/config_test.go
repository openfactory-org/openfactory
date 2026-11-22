package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func TestLoadDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(filepath.Join(dir, "missing.yaml"))
	if err != nil || c.Slots != 2 || c.Slot.MemoryMiB != 4096 || !c.SMTPBlocked() {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	p := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(p, []byte("slots: 4\nslot: {vcpus: 4, memory_mib: 8192, scratch_gib: 20}\nnetwork: {block_smtp: false}\nlabels: {pool: big}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Slots != 4 || c.Slot.VCPUs != 4 || c.SMTPBlocked() || c.Labels["pool"] != "big" || c.Network.Subnet != "10.213.0.0/16" {
		t.Fatalf("loaded = %+v", c)
	}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	if c2, err := Load(p); err != nil || c2.Slots != 4 {
		t.Fatalf("round trip = %+v, %v", c2, err)
	}
	if c.FirecrackerDir() != "/var/lib/openfactory/bin/firecracker-v1.18.0" {
		t.Fatalf("firecracker dir %q", c.FirecrackerDir())
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
	}{
		{"slots", func(c *Config) { c.Slots = 100 }},
		{"vcpus", func(c *Config) { c.Slot.VCPUs = 0 }},
		{"memory", func(c *Config) { c.Slot.MemoryMiB = 64 }},
		{"scratch", func(c *Config) { c.Slot.ScratchGiB = 0 }},
		{"data dir", func(c *Config) { c.DataDir = "rel" }},
		{"subnet", func(c *Config) { c.Network.Subnet = "fd00::/64" }},
		{"host bits", func(c *Config) { c.Network.Subnet = "10.213.0.1/16" }},
		{"subnet room", func(c *Config) { c.Network.Subnet = "10.0.0.0/29"; c.Slots = 4 }},
		{"internal allow", func(c *Config) { c.Network.InternalAllow = []string{"10.1.2.3"} }},
		{"ofnet in range", func(c *Config) { c.Jail.OfnetUID = c.Jail.UIDBase + 3 }},
		{"url", func(c *Config) { c.ControlURL = "ftp://x" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mut(&c)
			if err := c.Validate(); !errors.Is(err, rtcore.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestKernelPath(t *testing.T) {
	c := Default()
	c.DataDir = t.TempDir()
	if _, err := c.KernelPath(); err == nil {
		t.Fatal("want an error without a kernel")
	}
	k := filepath.Join(c.DataDir, "kernels", "abc", "vmlinux")
	_ = os.MkdirAll(filepath.Dir(k), 0o755)
	_ = os.WriteFile(k, []byte("x"), 0o444)
	if got, err := c.KernelPath(); err != nil || got != k {
		t.Fatalf("kernel = %q, %v", got, err)
	}
}
