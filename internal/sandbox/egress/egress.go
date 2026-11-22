// Package egress builds the Kubernetes objects that limit the network of a
// sandbox Pod: the NetworkPolicy and the netguard proxy sidecar. The task
// Job backend (k8stask) uses it.
//
// A NetworkPolicy cannot match host names. It allows egress to DNS, to the
// control plane and to the proxy ports. The hosts of the task policy are
// enforced by the proxy sidecar.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	// NamespaceLabel is the label that Kubernetes sets on each namespace.
	NamespaceLabel = "kubernetes.io/metadata.name"
	// ProxyPort is the port of the netguard sidecar.
	ProxyPort = 3128
	// ProxySidecarName is the name of the sidecar container.
	ProxySidecarName = "egress-proxy"
	// ReportPort is the port of the report of blocked hosts of the sidecar. It
	// listens on the loopback address of the Pod.
	ReportPort = 3129
	// ReportEnv is the variable with the URL of that report. A task with a
	// proxy sidecar has it. The runner reads the report and sends one
	// diagnostic for each blocked host.
	ReportEnv = "OF_NETGUARD_REPORT"
	// LocalNoProxy is the part of NO_PROXY that never changes.
	LocalNoProxy = "localhost,127.0.0.1"
	// ModeUnrestricted is the only mode of a task policy that gets no
	// NetworkPolicy. Any other mode counts as restricted, so that an unknown
	// mode fails closed.
	ModeUnrestricted = "unrestricted"
)

// Restricted reports whether a task policy needs a NetworkPolicy.
func Restricted(p *core.TaskNetworkPolicy) bool {
	return p != nil && p.Mode != ModeUnrestricted
}

// Peer is a set of Pods that a restricted task can reach.
type Peer struct {
	Namespace string
	Labels    map[string]any
}

// Empty reports whether the peer selects nothing.
func (p Peer) Empty() bool { return len(p.Labels) == 0 }

// NewPeer parses a label selector string of "key=value" terms. An empty
// selector gives an empty peer.
func NewPeer(what, namespace, selector string) (Peer, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if namespace != "" {
			return Peer{}, fmt.Errorf("%w: %s namespace is set without a selector", core.ErrInvalid, what)
		}
		return Peer{}, nil
	}
	labels := map[string]any{}
	for _, term := range strings.Split(selector, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(term), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return Peer{}, fmt.Errorf("%w: %s selector %q: each term must be key=value", core.ErrInvalid, what, selector)
		}
		labels[k] = v
	}
	return Peer{Namespace: namespace, Labels: labels}, nil
}

// Rule returns an egress rule for the peer.
func (p Peer) Rule() map[string]any {
	to := map[string]any{"podSelector": map[string]any{"matchLabels": copyMap(p.Labels)}}
	if p.Namespace != "" {
		to["namespaceSelector"] = map[string]any{"matchLabels": map[string]any{NamespaceLabel: p.Namespace}}
	}
	return map[string]any{"to": []any{to}}
}

// DNSRule returns the egress rule for DNS to any namespace.
func DNSRule() map[string]any {
	return map[string]any{
		"to": []any{map[string]any{"namespaceSelector": map[string]any{}}},
		"ports": []any{
			map[string]any{"protocol": "UDP", "port": 53},
			map[string]any{"protocol": "TCP", "port": 53},
		},
	}
}

// ControlURLRule returns an egress rule for the host of the control URL if
// that host is an IP address, else nil.
func ControlURLRule(raw string) map[string]any {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return nil
	}
	cidr := ip.String() + "/128"
	if ip4 := ip.To4(); ip4 != nil {
		cidr = ip4.String() + "/32"
	}
	rule := map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": cidr}}}}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	if n, err := strconv.Atoi(port); err == nil {
		rule["ports"] = []any{map[string]any{"protocol": "TCP", "port": n}}
	}
	return rule
}

// ProxyPortRule returns the egress rule of the proxy sidecar: TCP 80 and 443
// to any address. A NetworkPolicy cannot select one container of a Pod, so
// the rule opens these ports for the task container too.
func ProxyPortRule() map[string]any {
	return map[string]any{"ports": []any{
		map[string]any{"protocol": "TCP", "port": 80},
		map[string]any{"protocol": "TCP", "port": 443},
	}}
}

// PolicyParams describes one NetworkPolicy.
type PolicyParams struct {
	// Name and Namespace of the NetworkPolicy.
	Name, Namespace string
	// SelectorKey and SelectorValue select the Pod of the task.
	SelectorKey, SelectorValue string
	// Labels and Annotations of the NetworkPolicy.
	Labels, Annotations map[string]any
	// ControlPlane selects the Pods of the control plane. If it is empty,
	// ControlURL is used when its host is an IP address.
	ControlPlane Peer
	ControlURL   string
	// ExtraURLs are more URLs whose host the policy allows if it is an IP
	// address, for example the address of a proxy of the control plane.
	ExtraURLs []string
	// Sidecar is true if the Pod has a proxy sidecar.
	Sidecar bool
}

// Manifest builds the NetworkPolicy of a restricted task. It allows egress to
// DNS and to the control plane only. With a proxy sidecar, it also allows TCP
// 80 and 443 to any address, because the sidecar shares the network of the
// task container.
func Manifest(p PolicyParams) map[string]any {
	egress := []any{DNSRule()}
	if !p.ControlPlane.Empty() {
		egress = append(egress, p.ControlPlane.Rule())
	} else if r := ControlURLRule(p.ControlURL); r != nil {
		egress = append(egress, r)
	}
	for _, raw := range p.ExtraURLs {
		if r := ControlURLRule(raw); r != nil {
			egress = append(egress, r)
		}
	}
	if p.Sidecar {
		egress = append(egress, ProxyPortRule())
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":        p.Name,
			"namespace":   p.Namespace,
			"labels":      p.Labels,
			"annotations": p.Annotations,
		},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{p.SelectorKey: p.SelectorValue}},
			"policyTypes": []any{"Egress"},
			"egress":      egress,
		},
	}
}

// SidecarURL is the proxy URL of a Pod with a proxy sidecar.
func SidecarURL() string { return "http://127.0.0.1:" + strconv.Itoa(ProxyPort) }

// SidecarReportURL is the URL of the report of blocked hosts of the sidecar.
func SidecarReportURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(ReportPort) + "/blocked"
}

// Env returns the proxy variables for a proxy URL. The hosts in bypass, and
// the loopback addresses, do not use the proxy.
func Env(proxyURL string, bypass ...string) map[string]string {
	noProxy := LocalNoProxy
	for _, h := range bypass {
		if h != "" {
			noProxy = h + "," + noProxy
		}
	}
	return map[string]string{
		"HTTPS_PROXY": proxyURL, "HTTP_PROXY": proxyURL, "https_proxy": proxyURL, "http_proxy": proxyURL,
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}
}

// SidecarParams describes the proxy sidecar of one task.
type SidecarParams struct {
	Image, PullPolicy string
	// User is the numeric user of the sidecar. It must not be 0.
	User int64
	// Policy is the policy of the task. Its hosts become arguments.
	Policy *core.TaskNetworkPolicy
	// AlwaysAllow lists hosts that the proxy allows even when the policy
	// denies them. The control plane is one of them when the Pod reaches it
	// through the proxy (see LockControlViaProxy).
	AlwaysAllow []string
}

// Sidecar returns the native sidecar that runs the netguard proxy for one
// task. It is an init container with restartPolicy Always, so that it starts
// before the task container and the Pod can complete when the task container
// ends. The startup probe holds the task container until the proxy listens.
func Sidecar(p SidecarParams) map[string]any {
	listen := "127.0.0.1:" + strconv.Itoa(ProxyPort)
	command := []any{"openfactory", "netguard", "--listen", listen, "--report-listen", "127.0.0.1:" + strconv.Itoa(ReportPort)}
	if p.Policy != nil {
		for _, h := range p.Policy.AllowedHosts {
			command = append(command, "--allow", h)
		}
		for _, h := range p.Policy.DeniedHosts {
			command = append(command, "--deny", h)
		}
	}
	for _, h := range p.AlwaysAllow {
		if h != "" {
			command = append(command, "--always-allow", h)
		}
	}
	// Platform hosts, such as an in-cluster control plane, may resolve to a
	// private address. netguard refuses that for every other host.
	for _, h := range p.AlwaysAllow {
		if h != "" && !strings.HasPrefix(h, ".") && !strings.HasPrefix(h, "*.") {
			command = append(command, "--allow-private-host", h)
		}
	}
	return map[string]any{
		"name":            ProxySidecarName,
		"image":           p.Image,
		"imagePullPolicy": p.PullPolicy,
		"command":         command,
		"restartPolicy":   "Always",
		"securityContext": map[string]any{
			"runAsNonRoot":             true,
			"runAsUser":                p.User,
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"memory": "128Mi"},
		},
		// An exec probe runs in the network space of the Pod. A tcpSocket
		// probe is made by the kubelet, from the network space of the node,
		// so it cannot reach a proxy that listens on the loopback address
		// of the Pod: it would never pass, and the Pod would restart the
		// proxy for ever.
		"startupProbe": map[string]any{
			"exec": map[string]any{"command": []any{
				"openfactory", "netprobe", "dial", "--addr", "127.0.0.1:" + strconv.Itoa(ProxyPort),
			}},
			"periodSeconds":    1,
			"timeoutSeconds":   3,
			"failureThreshold": 30,
		},
	}
}

// WithSidecar puts the sidecar first in a list of init containers. It
// replaces a container of the template with the same name.
func WithSidecar(init []any, sidecar map[string]any) []any {
	out := []any{sidecar}
	for _, c := range init {
		if m, _ := c.(map[string]any); m == nil || m["name"] != ProxySidecarName {
			out = append(out, c)
		}
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// AccessVerbs are the verbs that a backend needs on NetworkPolicies.
var AccessVerbs = []string{"create", "get", "patch", "delete"}

// CheckAccess asks, with a SelfSubjectAccessReview, whether the account can
// use each verb of AccessVerbs on NetworkPolicies in the namespace. The
// review function posts the body to the API and decodes the answer into out.
// It returns the denied verbs, sorted. An error means that a review failed,
// so the answer is unknown.
func CheckAccess(ctx context.Context, namespace string, review func(ctx context.Context, body map[string]any, out any) error) ([]string, error) {
	var denied []string
	for _, verb := range AccessVerbs {
		body := map[string]any{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"spec": map[string]any{"resourceAttributes": map[string]any{
				"namespace": namespace, "verb": verb, "group": "networking.k8s.io", "resource": "networkpolicies",
			}},
		}
		var out struct {
			Status struct {
				Allowed bool `json:"allowed"`
			} `json:"status"`
		}
		if err := review(ctx, body, &out); err != nil {
			return nil, fmt.Errorf("access review for %s: %w", verb, err)
		}
		if !out.Status.Allowed {
			denied = append(denied, verb)
		}
	}
	sort.Strings(denied)
	return denied, nil
}
