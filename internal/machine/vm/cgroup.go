package vm

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cgroups manages the delegated cgroup v2 subtree of the agent service.
// With Delegate=yes, systemd gives the service its cgroup. The agent moves
// itself into a leaf "agent" and puts the VMs under "vms", because a cgroup
// that enables controllers for its children may hold no process.
type Cgroups struct {
	// Root is /sys/fs/cgroup.
	Root string
}

// Controllers enabled for the VMs.
var Controllers = []string{"cpu", "memory", "pids", "io"}

// SelfCgroup parses the cgroup v2 path of a process from the content of
// /proc/<pid>/cgroup ("0::/system.slice/x.service").
func SelfCgroup(procCgroup string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(procCgroup))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return strings.TrimPrefix(rest, "/"), nil
		}
	}
	return "", errors.New("no cgroup v2 entry; the host needs the unified cgroup hierarchy")
}

// Setup prepares the subtree under service (relative to Root): it creates
// service/agent and moves pid there, creates service/vms and enables the
// controllers in service and in vms. It returns the parent cgroup of the
// VMs, relative to Root. A service path that already ends in /agent (an
// agent restart in the same unit) is taken as the leaf.
func (c Cgroups) Setup(service string, pid int) (string, error) {
	service = strings.TrimSuffix(service, "/agent")
	if service == "" {
		return "", errors.New("the agent runs in the root cgroup; run it as the systemd unit with Delegate=yes")
	}
	base := filepath.Join(c.Root, service)
	agent := filepath.Join(base, "agent")
	vms := filepath.Join(base, "vms")
	for _, d := range []string{agent, vms} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("cgroup %s: %w", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(agent, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return "", fmt.Errorf("move the agent into %s: %w", agent, err)
	}
	ctl := "+" + strings.Join(Controllers, " +")
	for _, d := range []string{base, vms} {
		if err := os.WriteFile(filepath.Join(d, "cgroup.subtree_control"), []byte(ctl), 0o644); err != nil {
			return "", fmt.Errorf("enable %s in %s: %w (is Delegate=yes set?)", ctl, d, err)
		}
	}
	return filepath.Join(service, "vms"), nil
}

// Kill writes 1 to cgroup.kill of dir (Linux 5.14+).
func (c Cgroups) Kill(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
}

// Procs returns the PIDs in dir.
func (c Cgroups) Procs(dir string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// WaitEmpty polls until dir holds no process, then removes it. A missing
// directory counts as removed.
func (c Cgroups) WaitEmpty(dir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		procs, err := c.Procs(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil && len(procs) == 0 {
			if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s still has processes %v", dir, procs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// OOMKills returns the oom_kill counter of memory.events in dir.
func (c Cgroups) OOMKills(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}
