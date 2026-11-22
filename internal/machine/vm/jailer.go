package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VMID returns the jailer ID of the VM of a task: vm-<slot>-<8 hex digits of
// sha256(task ID)>. The jailer allows only letters, digits and hyphens, and
// task IDs contain "_" and ".".
func VMID(slot int, taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return "vm-" + strconv.Itoa(slot) + "-" + hex.EncodeToString(sum[:4])
}

// ValidVMID reports whether id is a valid jailer ID.
func ValidVMID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// RateLimit is a rate in bytes per second. 0 means no limit.
type RateLimit int64

func (r RateLimit) limiter(ops int64) *RateLimiter {
	if r <= 0 {
		return nil
	}
	l := &RateLimiter{Bandwidth: &TokenBucket{Size: int64(r), RefillTime: 1000}}
	if ops > 0 {
		l.Ops = &TokenBucket{Size: ops, RefillTime: 1000}
	}
	return l
}

// SnapshotFiles are the files of a snapshot template for a restore.
type SnapshotFiles struct {
	State, Mem, Scratch string
}

// Spec describes one VM.
type Spec struct {
	VMID string
	// Arch is amd64 or arm64.
	Arch          string
	Slot          int
	UID, GID      int
	VCPUs         int
	MemoryMiB     int
	ScratchGiB    int
	Kernel, Tools string
	Image         string
	// Scratch is the template of the scratch disk; the VM gets a copy.
	Scratch string
	// Cache are the read-only git mirror disks, attached as cache-0, cache-1...
	Cache    []string
	Tap      string
	GuestMAC net.HardwareAddr
	DiskRate RateLimit
	NetTx    RateLimit
	NetRx    RateLimit
	Debug    bool
	// Snapshot restores the VM from a template instead of a cold boot.
	Snapshot *SnapshotFiles
	// Template boots a template VM: it stops after hello.
	Template bool
}

// Validate checks the spec.
func (s Spec) Validate() error {
	if !ValidVMID(s.VMID) {
		return fmt.Errorf("invalid VM id %q", s.VMID)
	}
	if s.Arch != "amd64" && s.Arch != "arm64" {
		return fmt.Errorf("arch %q", s.Arch)
	}
	if s.UID <= 0 || s.GID <= 0 {
		return fmt.Errorf("jail uid and gid must be set")
	}
	if s.VCPUs < 1 || s.MemoryMiB < 128 {
		return fmt.Errorf("vcpus %d, memory %d MiB", s.VCPUs, s.MemoryMiB)
	}
	if s.Kernel == "" || s.Tools == "" || s.Tap == "" || len(s.GuestMAC) != 6 {
		return fmt.Errorf("kernel, tools disk, tap and MAC are required")
	}
	if !s.Template && s.Image == "" {
		return fmt.Errorf("image disk is required")
	}
	if len(s.Cache) > 4 {
		return fmt.Errorf("at most 4 cache disks, got %d", len(s.Cache))
	}
	return nil
}

// JailerConfig locates the jailer, Firecracker and the jail tree.
type JailerConfig struct {
	Jailer      string
	Firecracker string
	// ChrootBase is <data_dir>/jail.
	ChrootBase string
	// ParentCgroup is the delegated cgroup of the VMs, relative to the
	// cgroup root, for example system.slice/openfactory-worker.service/vms.
	ParentCgroup string
	// CgroupRoot is /sys/fs/cgroup.
	CgroupRoot string
}

// VMMOverheadMiB is added to the guest memory for the cgroup limit.
const VMMOverheadMiB = 128

// JailDir returns <chroot base>/firecracker/<vm id>.
func (c JailerConfig) JailDir(vmID string) string {
	return filepath.Join(c.ChrootBase, filepath.Base(c.Firecracker), vmID)
}

// Root returns the chroot of a VM.
func (c JailerConfig) Root(vmID string) string { return filepath.Join(c.JailDir(vmID), "root") }

// CgroupDir returns the cgroup of a VM.
func (c JailerConfig) CgroupDir(vmID string) string {
	root := c.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	return filepath.Join(root, c.ParentCgroup, vmID)
}

// Args returns the jailer command line of a spec (section 3.2 of the design).
func (c JailerConfig) Args(s Spec) []string {
	mem := int64(s.MemoryMiB+VMMOverheadMiB) << 20
	fsize := int64(s.ScratchGiB) << 30
	args := []string{
		c.Jailer,
		"--id", s.VMID,
		"--exec-file", c.Firecracker,
		"--uid", strconv.Itoa(s.UID), "--gid", strconv.Itoa(s.GID),
		"--chroot-base-dir", c.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", c.ParentCgroup,
		"--cgroup", "cpu.max=" + strconv.Itoa(s.VCPUs*100000) + " 100000",
		"--cgroup", "memory.max=" + strconv.FormatInt(mem, 10),
		"--cgroup", "memory.swap.max=0",
		"--cgroup", "pids.max=64",
		"--resource-limit", "no-file=1024",
	}
	if fsize > 0 {
		args = append(args, "--resource-limit", "fsize="+strconv.FormatInt(fsize, 10))
	}
	args = append(args,
		"--new-pid-ns",
		"--daemonize",
		"--",
		"--api-sock", "/run/firecracker.socket",
		"--level", "Warning",
		"--log-path", "/fc.log",
		"--http-api-max-payload-size", "65536",
		"--mmds-size-limit", "65536",
	)
	return args
}

// BootArgs returns the guest kernel command line.
func BootArgs(debug bool, arch string) string {
	console := "8250.nr_uarts=0 quiet loglevel=1"
	if debug {
		console = "console=ttyS0"
		if arch == "arm64" {
			console += " keep_bootcon"
		}
	}
	return "reboot=k panic=1 " + console + " ipv6.disable=1 random.trust_cpu=on rootfstype=ext4 init=/openfactory -- vm-init"
}

// ReadPID reads firecracker.pid in the jail of a VM.
func (c JailerConfig) ReadPID(vmID string) (int, error) {
	data, err := os.ReadFile(filepath.Join(c.Root(vmID), "firecracker.pid"))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("bad firecracker.pid %q", strings.TrimSpace(string(data)))
	}
	return pid, nil
}
