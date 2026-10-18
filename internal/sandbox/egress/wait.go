package egress

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// A network plugin applies a NetworkPolicy a short time after the Pod starts
// (measured on k3s: a connection at 5 ms passed, a connection at 250 ms was
// blocked). The policy exists before the Job, but the plugin programs it for
// the Pod only when the Pod has a network. A process of the task that opens a
// connection in that time is not limited.
//
// The wait closes this. An init container runs `openfactory netprobe
// wait-blocked` before every container that holds task code. It dials a
// target that the policy never allows, and ends when the dial fails. The
// network plugin limits the whole Pod, not one container, so a failed dial in
// the init container proves that the policy is active for the containers that
// follow. The init container ends with WaitExitCode if the dial still works
// when the time is over. The Pod then never starts the task, and the backend
// reports the end reason network_policy_timeout.
//
// The target is the kubelet of the node of the Pod, on the port 10250:
//
//   - A NetworkPolicy of a restricted task allows DNS, the control plane, the
//     egress proxy and, with the sidecar, TCP 80 and 443. It never allows the
//     address of a node on port 10250, so the policy blocks this target.
//   - Each node has a kubelet that listens on this port, so a connection works
//     without a policy. It is a real listener: a refused connection is not
//     taken as "open", because a plugin that rejects packets also gives
//     "connection refused".
//   - The port is not 80 or 443. The egress lock (see lock.go) refuses these
//     two ports from the first moment, so a probe on them would see a block
//     that comes from the lock and not from the policy.
//   - The Pod gets the address of its node from the downward API
//     (status.hostIP). This needs no permission and no knowledge of the
//     cluster.
//
// If the target is not reachable even without a policy (for example a firewall
// of the node refuses port 10250 from Pods), the dial fails at once, and the
// wait ends at once: the guarantee is then the same as without the wait. Set
// WaitParams.Addr to a target that is reachable in that case.
const (
	// WaitName is the name of the init container of the wait.
	WaitName = "egress-wait"
	// WaitHostEnv is the variable that holds the address of the node.
	WaitHostEnv = "OF_EGRESS_WAIT_HOST"
	// WaitPort is the default target port: the kubelet.
	WaitPort = 10250
	// DefaultWaitTimeout is the default time that the wait has.
	DefaultWaitTimeout = 30 * time.Second
	// WaitExitCode is the exit code of the init container when the time is
	// over and a direct connection still works (EX_TEMPFAIL of sysexits.h).
	WaitExitCode = 75

	// The settings of the dial loop. A dial that works resets the count of
	// failures. WaitConfirm failures in a row end the wait, so that one lost
	// packet before the policy is active does not end it.
	WaitInterval       = 100 * time.Millisecond
	WaitAttemptTimeout = 500 * time.Millisecond
	WaitConfirm        = 2
)

// WaitParams describes the wait container of one task.
type WaitParams struct {
	// Image must hold the openfactory binary.
	Image, PullPolicy string
	// Timeout is the time that the wait has. 0 means DefaultWaitTimeout.
	Timeout time.Duration
	// Addr is host:port of the target. Empty means the kubelet of the node.
	Addr string
}

// ValidateWaitAddr checks the target of the wait. An empty value is valid.
func ValidateWaitAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("egress wait target %q: want host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("egress wait target %q: the port must be a number from 1 to 65535", addr)
	}
	return nil
}

// WaitCommand returns the command of the wait container.
func WaitCommand(p WaitParams) []any {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	cmd := []any{"openfactory", "netprobe", "wait-blocked"}
	if p.Addr != "" {
		cmd = append(cmd, "--addr", p.Addr)
	} else {
		// Kubernetes replaces $(NAME) in a command with the variable.
		cmd = append(cmd, "--host", "$("+WaitHostEnv+")", "--port", strconv.Itoa(WaitPort))
	}
	return append(cmd, "--timeout", timeout.String())
}

// Wait returns the init container that ends when the NetworkPolicy of the Pod
// is active. Put it before every container that runs task code.
func Wait(p WaitParams) map[string]any {
	c := map[string]any{
		"name":            WaitName,
		"image":           p.Image,
		"imagePullPolicy": p.PullPolicy,
		"command":         WaitCommand(p),
		// The message of an end with an error comes from the log, so that the
		// status of the task can say why.
		"terminationMessagePolicy": "FallbackToLogsOnError",
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"memory": "128Mi"},
		},
	}
	if p.Addr == "" {
		c["env"] = []any{map[string]any{
			"name": WaitHostEnv,
			"valueFrom": map[string]any{"fieldRef": map[string]any{
				"apiVersion": "v1", "fieldPath": "status.hostIP",
			}},
		}}
	}
	return c
}

// WithWait puts the wait container first in a list of init containers. It
// replaces a container of the template with the same name. Call it before
// WithSidecar and WithLock, so that the order is the lock, the proxy, the
// wait, the rest.
func WithWait(init []any, wait map[string]any) []any {
	out := []any{wait}
	for _, c := range init {
		if m, _ := c.(map[string]any); m == nil || m["name"] != WaitName {
			out = append(out, c)
		}
	}
	return out
}

// WaitMessage returns the text that the wait prints when the time is over.
func WaitMessage(addr string, timeout time.Duration) string {
	return strings.Join([]string{
		"egress-wait: a direct connection to " + addr + " still works after " + timeout.String() + ".",
		"The NetworkPolicy of this Pod is not active, so the task did not start.",
	}, " ")
}
