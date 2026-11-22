package backend

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/vm"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// endFlags are what the host decided about a task before its VM ended.
type endFlags struct {
	cancelled bool
	deadline  bool
	netFailed bool
}

// MapExit maps the end of a VM to the final task status fields (section 13
// of the design):
//
//	host cgroup oom_kill > 0             → container_oom
//	network helper failed (restricted)   → network_policy_timeout
//	max_session_seconds + 60 s passed    → deadline_exceeded
//	task.cancel                          → cancelled
//	no exit report                       → unknown
//	vm-init failed before the runner     → process_failed
//	runner exit 0                        → completed
//	runner SIGKILL with guest oom kills  → container_oom
//	runner exit non-zero or signal       → process_failed
func MapExit(e vm.Exit, f endFlags) (rtcore.SandboxReason, *int, string) {
	var code *int
	var tail string
	if e.Report != nil {
		code = e.Report.ExitCode
		tail = strings.TrimSpace(e.Report.OutputTail)
	}
	switch {
	case e.HostOOMKills > 0:
		return rtcore.SandboxContainerOOM, code, "VM overhead exceeded host limit"
	case f.netFailed:
		return rtcore.SandboxNetworkPolicyTimeout, code, "the network helper of the VM stopped"
	case f.deadline:
		return rtcore.SandboxDeadlineExceeded, code, "the session passed max_session_seconds"
	case f.cancelled:
		return rtcore.SandboxCancelled, code, ""
	case e.Report == nil:
		msg := "the VM ended without an exit report"
		if e.Message != "" {
			msg = e.Message
		}
		return rtcore.SandboxUnknown, nil, msg
	case e.Report.Phase == ctlstream.ExitPhaseInit:
		return rtcore.SandboxProcessFailed, code, "vm-init failed: " + e.Report.Error
	}
	r := e.Report
	if r.Signal == int(syscall.SIGKILL) && r.OOMKills > 0 {
		return rtcore.SandboxContainerOOM, code, fmt.Sprintf("the guest killed %d process(es) for memory", r.OOMKills)
	}
	if r.Signal == 0 && code != nil && *code == 0 {
		return rtcore.SandboxCompleted, code, ""
	}
	msg := "the runner failed"
	switch {
	case r.Signal != 0:
		msg = "the runner was killed by signal " + strconv.Itoa(r.Signal)
	case code != nil:
		msg = "the runner exited with code " + strconv.Itoa(*code)
	}
	if r.OOMKills > 0 {
		msg += fmt.Sprintf(" (guest oom kills: %d)", r.OOMKills)
	}
	if tail != "" {
		msg += ": " + lastLine(tail)
	}
	return rtcore.SandboxProcessFailed, code, msg
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := lines[len(lines)-1]
	if len(l) > 512 {
		l = l[len(l)-512:]
	}
	return l
}

// ParseCPU parses a CPU quantity ("2", "1.5", "500m") into millicores.
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if m, ok := strings.CutSuffix(s, "m"); ok {
		return strconv.ParseInt(m, 10, 64)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("cpu %q", s)
	}
	return int64(f * 1000), nil
}

// ParseMemory parses a memory quantity ("4Gi", "512Mi", "4G", "1000000")
// into bytes.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1000}, {"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1000 * 1000 * 1000 * 1000},
	}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("memory %q", s)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("memory %q", s)
	}
	return n, nil
}
