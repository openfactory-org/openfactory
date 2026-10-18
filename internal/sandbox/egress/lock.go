package egress

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The proxy sidecar shares the network of the task container, and a
// NetworkPolicy cannot select one container. The policy of a Pod with a
// sidecar therefore has to allow TCP 80 and 443 to any address, and a program
// of the task that ignores the proxy variables can use these ports directly.
//
// The egress lock closes this. An init container with the capability
// NET_ADMIN adds a firewall rule to the network of the Pod: a TCP connection to
// port 80 or 443 is refused, unless the process that opens it runs as the user
// of the proxy. The task container runs as another user, so only the proxy can
// leave the Pod on these ports. The NetworkPolicy still blocks every other
// port. The rule lives in the network namespace of the Pod and goes away with
// the Pod.

const (
	// LockNone and LockIPTables are the values of the setting that chooses
	// how the backend closes the direct path on ports 80 and 443.
	LockNone     = "none"
	LockIPTables = "iptables"

	// LockName is the name of the init container that adds the rules.
	LockName = "egress-lock"
	// LockChain is the name of the chain of the rules.
	LockChain = "OF_EGRESS_LOCK"
	// DefaultProxyUser is the numeric user of the proxy sidecar with the
	// lock. It is the "nobody" user of many distroless images.
	DefaultProxyUser int64 = 65532
)

// ValidateLock checks the value of the lock setting. An empty value means
// LockNone.
func ValidateLock(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", LockNone, LockIPTables:
		return nil
	}
	return fmt.Errorf("egress lock %q: want %q or %q", v, LockNone, LockIPTables)
}

// LockEnabled reports whether the value of the lock setting turns it on.
func LockEnabled(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), LockIPTables)
}

// ProxyUserFor returns the user of the proxy sidecar for a task that runs as
// taskUser. The two users must differ, because the rule tells them apart by
// user. A task user of 0 or less (the user of the image) gets the default
// proxy user: the task container must then run as another user than that.
func ProxyUserFor(taskUser int64) int64 {
	return ProxyUserAvoiding(taskUser)
}

// ProxyUserAvoiding returns the first user from DefaultProxyUser up that is
// none of the given users: the users that code of the task can run as.
func ProxyUserAvoiding(users ...int64) int64 {
	used := map[int64]bool{}
	for _, u := range users {
		used[u] = true
	}
	u := DefaultProxyUser
	for used[u] {
		u++
	}
	return u
}

// LockControlViaProxy reports whether the Pod must reach the control plane
// through the proxy when the lock is on. The lock refuses direct connections
// to ports 80 and 443, so a control plane on one of these ports is not
// reachable directly. The proxy then always allows its host.
func LockControlViaProxy(controlURL string) bool {
	u, err := url.Parse(controlURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	n, _ := strconv.Atoi(port)
	return n == 80 || n == 443
}

// LockScript returns the shell script of the lock container. The only value
// in it is the numeric user of the proxy. The script uses iptables, and also
// ip6tables when the Pod has an IPv6 address.
func LockScript(proxyUser int64) string {
	return fmt.Sprintf(`set -eu
lock() {
  ipt=$1
  $ipt -w 5 -N %[1]s 2>/dev/null || $ipt -w 5 -F %[1]s
  $ipt -w 5 -A %[1]s -o lo -j RETURN
  $ipt -w 5 -A %[1]s -m owner --uid-owner %[2]d -j RETURN
  $ipt -w 5 -A %[1]s -p tcp -j REJECT --reject-with tcp-reset
  $ipt -w 5 -A OUTPUT -p tcp -m multiport --dports 80,443 -j %[1]s
}
lock iptables
if grep -qv ' lo$' /proc/net/if_inet6 2>/dev/null; then
  lock ip6tables
fi
`, LockChain, proxyUser)
}

// LockParams describes the lock container of one task.
type LockParams struct {
	// Image must hold sh, grep, iptables and ip6tables. The image of the
	// proxy sidecar does.
	Image, PullPolicy string
	// ProxyUser is the user of the proxy sidecar.
	ProxyUser int64
}

// Lock returns the init container that adds the rules. It runs as root with
// the capabilities NET_ADMIN and NET_RAW only, and it ends before the next
// init container starts. Put it before the proxy sidecar.
func Lock(p LockParams) map[string]any {
	return map[string]any{
		"name":            LockName,
		"image":           p.Image,
		"imagePullPolicy": p.PullPolicy,
		"command":         []any{"/bin/sh", "-c", LockScript(p.ProxyUser)},
		"securityContext": map[string]any{
			"runAsUser":                0,
			"runAsNonRoot":             false,
			"allowPrivilegeEscalation": false,
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
				"add":  []any{"NET_ADMIN", "NET_RAW"},
			},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
			"limits":   map[string]any{"memory": "64Mi"},
		},
	}
}

// WithLock puts the lock container first in a list of init containers. It
// replaces a container of the template with the same name. Call it after
// WithSidecar, so that the order is the lock, the proxy, the rest.
func WithLock(init []any, lock map[string]any) []any {
	out := []any{lock}
	for _, c := range init {
		if m, _ := c.(map[string]any); m == nil || m["name"] != LockName {
			out = append(out, c)
		}
	}
	return out
}

// HardenTask sets allowPrivilegeEscalation to false for a container that
// does not set it. It always sets runAsNonRoot to true and drops all
// capabilities, and it removes SETUID and SETGID from the added ones. A task
// that could become root, or change its user, could switch to the user of the
// proxy and pass the lock.
func HardenTask(container map[string]any) {
	sc, _ := container["securityContext"].(map[string]any)
	if sc == nil {
		sc = map[string]any{}
	}
	if _, ok := sc["allowPrivilegeEscalation"]; !ok {
		sc["allowPrivilegeEscalation"] = false
	}
	sc["runAsNonRoot"] = true
	caps, _ := sc["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	caps["drop"] = []any{"ALL"}
	if add, ok := caps["add"].([]any); ok {
		var kept []any
		for _, c := range add {
			switch n, _ := c.(string); strings.TrimPrefix(strings.ToUpper(n), "CAP_") {
			case "SETUID", "SETGID", "ALL":
			default:
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			delete(caps, "add")
		} else {
			caps["add"] = kept
		}
	}
	sc["capabilities"] = caps
	container["securityContext"] = sc
}
