// Package hostcheck collects the facts of a worker host and runs the
// checks of worker doctor: KVM, cgroup v2, kernel, tools, Firecracker,
// the VM subnet and firewall tools that would drop VM traffic.
package hostcheck

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

// Status of a check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Fix is the command or action that fixes the problem.
	Fix string `json:"fix,omitempty"`
}

// Env is the host as the checks see it. Tests replace every field.
type Env struct {
	// Root prefixes /proc, /sys, /dev and /etc paths ("" for the host).
	Root string
	// KVM opens /dev/kvm read-write and checks the API version.
	KVM func() error
	// Uname returns the kernel release and the machine.
	Uname    func() (release, machine string)
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) (string, error)
	// FreeDisk returns free and total bytes of the data directory.
	FreeDisk func(dir string) (free, total int64, err error)
}

// Host returns the Env of this host.
func Host() Env {
	return Env{
		KVM: checkKVM, Uname: uname, LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: fixed diagnostic tools
			return string(out), err
		},
		FreeDisk: freeDisk,
	}
}

func (e Env) read(p string) string {
	data, err := os.ReadFile(filepath.Join(e.Root, p))
	if err != nil {
		return ""
	}
	return string(data)
}

func (e Env) exists(p string) bool {
	_, err := os.Stat(filepath.Join(e.Root, p))
	return err == nil
}

// GoArch maps uname -m to the Go arch.
func GoArch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return machine
}

// KernelVersion parses "6.18.3-generic" into (6, 18).
func KernelVersion(release string) (major, minor int) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	major, _ = strconv.Atoi(parts[0])
	digits := strings.TrimLeftFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	end := strings.IndexFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		digits = parts[1][:end]
	}
	minor, _ = strconv.Atoi(digits)
	return major, minor
}

// MemoryMiB reads MemTotal of /proc/meminfo.
func (e Env) MemoryMiB() int64 {
	sc := bufio.NewScanner(strings.NewReader(e.read("/proc/meminfo")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// Facts returns the facts of the host for enrollment and hello.
func Facts(ctx context.Context, e Env, c config.Config, version string) direct.WorkerFacts {
	release, machine := e.Uname()
	f := direct.WorkerFacts{
		Arch: GoArch(machine), OS: "linux", Kernel: release, VCPUs: runtime.NumCPU(),
		MemoryMiB: e.MemoryMiB(), RuntimeVersion: version, KVM: e.KVM() == nil,
	}
	if e.FreeDisk != nil {
		dir := c.DataDir
		if _, err := os.Stat(dir); err != nil {
			dir = filepath.Dir(dir)
		}
		if free, total, err := e.FreeDisk(dir); err == nil {
			f.DiskFreeBytes, f.DiskTotalBytes = free, total
		}
	}
	if out, err := e.Run(ctx, filepath.Join(c.FirecrackerDir(), "firecracker"), "--version"); err == nil {
		f.FirecrackerVersion = firstLine(out)
	}
	if k, err := c.KernelPath(); err == nil {
		f.GuestKernel = filepath.Base(filepath.Dir(k))
	}
	if maj, min := KernelVersion(release); maj > 0 && !validatedKernel(maj, min) {
		f.Warnings = append(f.Warnings, "host kernel "+release+" is not a validated kernel (6.1 or 6.18)")
	}
	return f
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func validatedKernel(maj, min int) bool { return maj == 6 && (min == 1 || min == 18) }

// Result is the outcome of Doctor.
type Result struct {
	Checks []Check `json:"checks"`
	// ForwardDropped: a foreign firewall drops forwarded traffic, so
	// unrestricted VMs would have no internet.
	ForwardDropped bool `json:"forward_dropped"`
}

// Failed reports whether a check failed.
func (r Result) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Doctor runs every check.
func Doctor(ctx context.Context, e Env, c config.Config) Result {
	var r Result
	add := func(name string, st Status, detail, fix string) {
		r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: detail, Fix: fix})
	}
	release, machine := e.Uname()

	if err := e.KVM(); err != nil {
		add("kvm", Fail, err.Error(), "use a host with hardware virtualization (bare metal or a VPS with nested KVM), and make /dev/kvm read-write for root")
	} else {
		add("kvm", OK, "/dev/kvm is usable", "")
	}
	if a := GoArch(machine); a == "amd64" || a == "arm64" {
		add("arch", OK, a, "")
	} else {
		add("arch", Fail, "unsupported CPU architecture "+machine, "use an amd64 or arm64 host")
	}
	if e.exists("/sys/fs/cgroup/cgroup.controllers") {
		add("cgroup", OK, "cgroup v2 unified hierarchy", "")
	} else {
		add("cgroup", Fail, "no cgroup v2 unified hierarchy", "boot with systemd.unified_cgroup_hierarchy=1")
	}
	maj, min := KernelVersion(release)
	switch {
	case maj < 5 || maj == 5 && min < 15:
		add("kernel", Fail, "host kernel "+release+" is older than 5.15", "upgrade the host kernel to 6.1 or 6.18")
	case !validatedKernel(maj, min):
		add("kernel", Warn, "host kernel "+release+" works but is not validated", "use a 6.1 or 6.18 kernel")
	default:
		add("kernel", OK, release, "")
	}
	pkgs := map[string]string{"nft": "nftables", "ip": "iproute2", "mkfs.ext4": "e2fsprogs", "git": "git"}
	for _, tool := range []string{"nft", "ip", "mkfs.ext4", "git"} {
		if _, err := e.LookPath(tool); err != nil {
			add("tool "+tool, Fail, tool+" is missing", "install the package "+pkgs[tool]+" (apt-get install "+pkgs[tool]+" or dnf install "+pkgs[tool]+")")
		} else {
			add("tool "+tool, OK, "", "")
		}
	}
	fcDir := c.FirecrackerDir()
	for _, bin := range []string{"firecracker", "jailer"} {
		p := filepath.Join(fcDir, bin)
		out, err := e.Run(ctx, p, "--version")
		switch {
		case err != nil:
			add(bin, Fail, p+" does not run", "run deploy/machine/install.sh to install Firecracker "+c.Firecracker.Version)
		case !strings.Contains(out, strings.TrimPrefix(c.Firecracker.Version, "v")):
			add(bin, Fail, firstLine(out)+", want "+c.Firecracker.Version, "run deploy/machine/install.sh again")
		default:
			add(bin, OK, firstLine(out), "")
		}
	}
	if _, err := c.KernelPath(); err != nil {
		add("guest kernel", Fail, err.Error(), "run deploy/machine/install.sh")
	} else {
		add("guest kernel", OK, "", "")
	}
	r.Checks = append(r.Checks, e.subnetCheck(c))
	r.Checks = append(r.Checks, e.uidCheck(c))
	fw, dropped := e.firewallChecks(ctx)
	r.Checks = append(r.Checks, fw...)
	r.ForwardDropped = dropped
	if strings.TrimSpace(e.read("/sys/devices/system/cpu/smt/active")) == "1" && e.vulnerable() {
		add("smt", Warn, "SMT is on and the CPU reports vulnerabilities", "disable SMT for untrusted workloads (nosmt on the kernel command line)")
	}
	if strings.TrimSpace(e.read("/sys/kernel/mm/ksm/run")) == "1" {
		add("ksm", Warn, "kernel same-page merging is on", "echo 0 > /sys/kernel/mm/ksm/run")
	}
	if lines := strings.Count(strings.TrimSpace(e.read("/proc/swaps")), "\n"); lines > 0 {
		add("swap", Warn, "swap is on (VM cgroups set memory.swap.max=0)", "swapoff -a")
	}
	r.Checks = append(r.Checks, e.budgetChecks(c)...)
	if e.FreeDisk != nil {
		need := (int64(c.Slots)*int64(c.Slot.ScratchGiB) + int64(c.Disk.ReserveGiB)) << 30
		dir := c.DataDir
		if _, err := os.Stat(dir); err != nil {
			dir = filepath.Dir(dir)
		}
		if free, _, err := e.FreeDisk(dir); err == nil && free < need {
			add("disk", Warn, fmt.Sprintf("%d GiB free, slots need %d GiB", free>>30, need>>30), "free disk space or lower slots or slot.scratch_gib")
		}
	}
	return r
}

func (e Env) vulnerable() bool {
	matches, _ := filepath.Glob(filepath.Join(e.Root, "/sys/devices/system/cpu/vulnerabilities/*"))
	for _, m := range matches {
		if data, err := os.ReadFile(m); err == nil && strings.HasPrefix(string(data), "Vulnerable") {
			return true
		}
	}
	return false
}

// subnetCheck fails when the VM subnet overlaps a host route.
func (e Env) subnetCheck(c config.Config) Check {
	subnet := c.Subnet()
	sc := bufio.NewScanner(strings.NewReader(e.read("/proc/net/route")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[0] == "Iface" || strings.HasPrefix(f[0], "ofvm") || strings.HasPrefix(f[0], "oftpl") {
			continue
		}
		dst, err1 := hexIP(f[1])
		mask, err2 := hexIP(f[7])
		if err1 != nil || err2 != nil || dst.IsUnspecified() {
			continue
		}
		bits := 0
		for _, b := range mask.As4() {
			for i := 7; i >= 0; i-- {
				if b&(1<<i) != 0 {
					bits++
				}
			}
		}
		route := netip.PrefixFrom(dst, bits).Masked()
		if route.Overlaps(subnet) {
			return Check{Name: "subnet", Status: Fail, Detail: fmt.Sprintf("network.subnet %s overlaps the route %s on %s", subnet, route, f[0]), Fix: "set network.subnet in " + config.DefaultPath + " to an unused range"}
		}
	}
	return Check{Name: "subnet", Status: OK, Detail: subnet.String()}
}

// hexIP parses a little-endian hex IPv4 of /proc/net/route.
func hexIP(s string) (netip.Addr, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}), nil //nolint:gosec // G115: bytes of a 32-bit value
}

// uidCheck fails when a jail uid has a passwd entry or a subuid range.
func (e Env) uidCheck(c config.Config) Check {
	lo, hi := c.Jail.UIDBase, c.Jail.UIDBase+255
	sc := bufio.NewScanner(strings.NewReader(e.read("/etc/passwd")))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) > 2 {
			if uid, err := strconv.Atoi(f[2]); err == nil && uid >= lo && uid <= hi {
				return Check{Name: "jail uids", Status: Fail, Detail: fmt.Sprintf("user %s has uid %d inside the jail range %d-%d", f[0], uid, lo, hi), Fix: "set jail.uid_base to an unused range"}
			}
		}
	}
	for _, file := range []string{"/etc/subuid", "/etc/subgid"} {
		sc := bufio.NewScanner(strings.NewReader(e.read(file)))
		for sc.Scan() {
			f := strings.Split(sc.Text(), ":")
			if len(f) == 3 {
				start, _ := strconv.Atoi(f[1])
				n, _ := strconv.Atoi(f[2])
				if start <= hi && start+n > lo {
					return Check{Name: "jail uids", Status: Fail, Detail: fmt.Sprintf("%s gives %s the range %d+%d, which overlaps the jail uids", file, f[0], start, n), Fix: "set jail.uid_base to an unused range"}
				}
			}
		}
	}
	return Check{Name: "jail uids", Status: OK, Detail: fmt.Sprintf("%d-%d", lo, hi)}
}

// firewallChecks detects Docker, ufw and firewalld rules that drop VM traffic.
func (e Env) firewallChecks(ctx context.Context) ([]Check, bool) {
	var out []Check
	dropped := false
	if _, err := e.LookPath("iptables"); err == nil {
		rules, err := e.Run(ctx, "iptables", "-S", "FORWARD")
		if err == nil && strings.Contains(rules, "-P FORWARD DROP") {
			dropped = true
			fix := "iptables -I FORWARD -i ofvm+ -j ACCEPT && iptables -I FORWARD -o ofvm+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"
			if strings.Contains(rules, "DOCKER-USER") {
				fix = "iptables -I DOCKER-USER -i ofvm+ -j ACCEPT && iptables -I DOCKER-USER -o ofvm+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"
			}
			out = append(out, Check{Name: "iptables forward", Status: Warn, Detail: "the iptables FORWARD policy is DROP (Docker or ufw); unrestricted VMs have no internet and are refused", Fix: fix})
		}
	}
	if _, err := e.LookPath("ufw"); err == nil {
		if st, err := e.Run(ctx, "ufw", "status"); err == nil && strings.Contains(st, "Status: active") {
			dropped = dropped || strings.Contains(st, "deny (routed)")
			out = append(out, Check{Name: "ufw", Status: Warn, Detail: "ufw is active and drops input on the VM taps by default",
				Fix: "ufw allow in on ofvm+ to any port 3128,3129,53 proto tcp && ufw allow in on ofvm+ to any port 53 proto udp && ufw route allow in on ofvm+"})
		}
	}
	if _, err := e.LookPath("firewall-cmd"); err == nil {
		if st, err := e.Run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(st) == "running" {
			out = append(out, Check{Name: "firewalld", Status: Warn, Detail: "firewalld is running and may drop traffic on the VM taps",
				Fix: "put the ofvm+ interfaces in a zone that allows tcp 3128, 3129, 53 and udp 53, and allows forwarding (firewall-cmd --permanent --zone=trusted --add-interface=ofvm0 for each tap)"})
		}
	}
	if len(out) == 0 {
		out = append(out, Check{Name: "firewall", Status: OK, Detail: "no Docker, ufw or firewalld rule drops VM traffic"})
	}
	return out, dropped
}

func (e Env) budgetChecks(c config.Config) []Check {
	var out []Check
	memNeed := int64(c.Slots)*int64(c.Slot.MemoryMiB+128) + 2048
	if mem := e.MemoryMiB(); mem > 0 && memNeed > mem {
		out = append(out, Check{Name: "memory budget", Status: Fail, Detail: fmt.Sprintf("slots need %d MiB and the host has %d MiB", memNeed, mem), Fix: "lower slots or slot.memory_mib"})
	}
	cpus := runtime.NumCPU()
	need := c.Slots * c.Slot.VCPUs
	switch {
	case need > 2*cpus:
		out = append(out, Check{Name: "vcpu budget", Status: Fail, Detail: fmt.Sprintf("slots need %d vCPUs and the host has %d", need, cpus), Fix: "lower slots or slot.vcpus"})
	case need > cpus:
		out = append(out, Check{Name: "vcpu budget", Status: Warn, Detail: fmt.Sprintf("slots overcommit vCPUs: %d of %d", need, cpus)})
	}
	return out
}
