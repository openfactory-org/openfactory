// Package config loads the configuration of a microVM worker host from
// /etc/openfactory/worker.yaml, fills defaults and checks it.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// DefaultPath is the configuration file of a worker host.
const DefaultPath = "/etc/openfactory/worker.yaml"

// FirecrackerVersion is the pinned Firecracker and jailer release.
const FirecrackerVersion = "v1.18.0"

// Config is the configuration of a worker host.
type Config struct {
	// ControlURL is the base URL of the control plane. enroll writes it.
	ControlURL string `yaml:"control_url"`
	// Name is the display name of the worker. enroll writes it.
	Name string `yaml:"name"`
	// Labels select the worker with workerHost: pool:<label>.
	Labels map[string]string `yaml:"labels,omitempty"`
	// Slots is the number of sessions that run at once.
	Slots int `yaml:"slots"`
	// Slot is the fixed size of one slot (one microVM).
	Slot SlotSize `yaml:"slot"`
	// DataDir holds images, kernels, jails and the identity.
	DataDir     string      `yaml:"data_dir"`
	Disk        Disk        `yaml:"disk"`
	Network     Network     `yaml:"network"`
	Jail        Jail        `yaml:"jail"`
	Firecracker Firecracker `yaml:"firecracker"`
	// Kernel is the guest kernel (vmlinux). Empty means the newest file
	// under <data_dir>/kernels/*/vmlinux.
	Kernel string `yaml:"kernel,omitempty"`
	Guest  Guest  `yaml:"guest"`
	Debug  Debug  `yaml:"debug"`
	// Snapshots turns on snapshot restore (one template for each image
	// digest, slot size and Firecracker version).
	Snapshots bool `yaml:"snapshots"`
}

// SlotSize is the size of one slot.
type SlotSize struct {
	VCPUs      int `yaml:"vcpus"`
	MemoryMiB  int `yaml:"memory_mib"`
	ScratchGiB int `yaml:"scratch_gib"`
}

// Disk holds the disk limits.
type Disk struct {
	// ReserveGiB is disk that the worker leaves free.
	ReserveGiB int `yaml:"reserve_gib"`
	// ImagesMaxGiB is the LRU limit of images and tools disks.
	ImagesMaxGiB int `yaml:"images_max_gib"`
	// GitcacheMaxGiB is the LRU limit of the git mirror cache.
	GitcacheMaxGiB int `yaml:"gitcache_max_gib"`
}

// Network holds the VM network settings.
type Network struct {
	// Subnet holds one /30 per slot.
	Subnet string `yaml:"subnet"`
	// BlockSMTP rejects tcp/25 in unrestricted mode. Nil means true.
	BlockSMTP *bool `yaml:"block_smtp,omitempty"`
	// InternalAllow lists ip:port exceptions to the private-range drop,
	// for example a self-managed forge.
	InternalAllow []string `yaml:"internal_allow,omitempty"`
	Rate          Rate     `yaml:"rate"`
}

// Rate holds per-VM network limits.
type Rate struct {
	TxMbit   int `yaml:"tx_mbit"`
	RxMbit   int `yaml:"rx_mbit"`
	MaxConns int `yaml:"max_conns"`
}

// Jail holds the uids of the jails.
type Jail struct {
	// UIDBase: the VM in slot n runs as uid and gid UIDBase+n.
	UIDBase int `yaml:"uid_base"`
	// OfnetUID runs netguard and the DNS relay.
	OfnetUID int `yaml:"ofnet_uid"`
}

// Firecracker names the pinned VMM.
type Firecracker struct {
	Version string `yaml:"version"`
	// Dir holds firecracker and jailer. Empty means
	// <data_dir>/bin/firecracker-<version>.
	Dir string `yaml:"dir,omitempty"`
}

// Guest holds guest settings.
type Guest struct {
	// AllowRoot permits platform.user "0".
	AllowRoot bool `yaml:"allow_root"`
}

// Debug holds settings for development only.
type Debug struct {
	SerialConsole bool `yaml:"serial_console"`
}

// Default returns the configuration with every default set.
func Default() Config {
	return Config{
		Slots:   2,
		Slot:    SlotSize{VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 40},
		DataDir: "/var/lib/openfactory",
		Disk:    Disk{ReserveGiB: 20, ImagesMaxGiB: 60, GitcacheMaxGiB: 50},
		Network: Network{
			Subnet: "10.213.0.0/16",
			Rate:   Rate{TxMbit: 200, RxMbit: 400, MaxConns: 2048},
		},
		Jail:        Jail{UIDBase: 900000, OfnetUID: 899999},
		Firecracker: Firecracker{Version: FirecrackerVersion},
	}
}

// Load reads path over the defaults and checks the result. A missing file
// gives the defaults.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path) //nolint:gosec // G304: an operator-given config path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, c.Validate()
		}
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%w: parse %s: %w", rtcore.ErrInvalid, path, err)
	}
	return c, c.Validate()
}

// Save writes the configuration to path with mode 0644.
func Save(path string, c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: /etc/openfactory is world-readable
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306: the config holds no secret
		return err
	}
	return os.Rename(tmp, path)
}

// Validate returns the first problem of the configuration.
func (c Config) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: "+f, append([]any{rtcore.ErrInvalid}, a...)...) }
	if c.Slots < 0 || c.Slots > 64 {
		return bad("slots must be 0 to 64, got %d", c.Slots)
	}
	if c.Slot.VCPUs < 1 || c.Slot.VCPUs > 32 {
		return bad("slot.vcpus must be 1 to 32, got %d", c.Slot.VCPUs)
	}
	if c.Slot.MemoryMiB < 256 {
		return bad("slot.memory_mib must be 256 or more, got %d", c.Slot.MemoryMiB)
	}
	if c.Slot.ScratchGiB < 1 {
		return bad("slot.scratch_gib must be 1 or more, got %d", c.Slot.ScratchGiB)
	}
	if !filepath.IsAbs(c.DataDir) {
		return bad("data_dir must be an absolute path, got %q", c.DataDir)
	}
	p, err := netip.ParsePrefix(c.Network.Subnet)
	if err != nil || !p.Addr().Is4() {
		return bad("network.subnet %q is not an IPv4 prefix", c.Network.Subnet)
	}
	if p.Masked() != p {
		return bad("network.subnet %q has host bits set", c.Network.Subnet)
	}
	if free := 32 - p.Bits(); free < 2 || (1<<(free-2)) < c.Slots {
		return bad("network.subnet %q has room for fewer than %d /30 subnets", c.Network.Subnet, c.Slots)
	}
	for _, a := range c.Network.InternalAllow {
		if _, err := netip.ParseAddrPort(a); err != nil {
			return bad("network.internal_allow %q is not ip:port", a)
		}
	}
	if c.Jail.UIDBase < 1000 || c.Jail.OfnetUID < 1000 {
		return bad("jail.uid_base and jail.ofnet_uid must be 1000 or more")
	}
	if c.Jail.OfnetUID >= c.Jail.UIDBase && c.Jail.OfnetUID <= c.Jail.UIDBase+255 {
		return bad("jail.ofnet_uid %d is inside the jail uid range", c.Jail.OfnetUID)
	}
	if c.ControlURL != "" && !strings.HasPrefix(c.ControlURL, "https://") && !strings.HasPrefix(c.ControlURL, "http://") {
		return bad("control_url %q must start with https://", c.ControlURL)
	}
	return nil
}

// Subnet returns the parsed VM subnet. Call it after Validate.
func (c Config) Subnet() netip.Prefix {
	p, _ := netip.ParsePrefix(c.Network.Subnet)
	return p
}

// SMTPBlocked reports whether tcp/25 is rejected in unrestricted mode.
func (c Config) SMTPBlocked() bool { return c.Network.BlockSMTP == nil || *c.Network.BlockSMTP }

// FirecrackerDir returns the directory of firecracker and jailer.
func (c Config) FirecrackerDir() string {
	if c.Firecracker.Dir != "" {
		return c.Firecracker.Dir
	}
	return filepath.Join(c.DataDir, "bin", "firecracker-"+c.Firecracker.Version)
}

// WorkerDir returns <data_dir>/worker: identity, task records, state.
func (c Config) WorkerDir() string { return filepath.Join(c.DataDir, "worker") }

// IdentityDir returns the directory of the key pair and worker.json.
func (c Config) IdentityDir() string { return filepath.Join(c.WorkerDir(), "identity") }

// ControlSocket returns the unix socket of the running agent, used by
// worker status and worker drain.
func (c Config) ControlSocket() string { return filepath.Join(c.WorkerDir(), "agent.sock") }

// KernelPath returns the guest kernel. It picks the newest
// <data_dir>/kernels/*/vmlinux when Kernel is empty.
func (c Config) KernelPath() (string, error) {
	if c.Kernel != "" {
		return c.Kernel, nil
	}
	matches, _ := filepath.Glob(filepath.Join(c.DataDir, "kernels", "*", "vmlinux"))
	var best string
	var bestT int64
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().UnixNano() > bestT {
			best, bestT = m, fi.ModTime().UnixNano()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no guest kernel under %s; run install.sh or set kernel", filepath.Join(c.DataDir, "kernels"))
	}
	return best, nil
}
