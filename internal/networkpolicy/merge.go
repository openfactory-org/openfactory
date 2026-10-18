package networkpolicy

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// NormalizeHost validates a host entry and returns its canonical (lowercase)
// form. Entries must be a hostname or a literal IP address; URLs, ports, CIDRs
// and wildcards are rejected.
func NormalizeHost(h string) (string, error) {
	s := strings.TrimSpace(h)
	if s == "" {
		return "", errors.New("host must not be empty")
	}
	switch {
	case strings.Contains(s, "://"):
		return "", fmt.Errorf("host %q must be a hostname or IP address, not a URL", h)
	case strings.Contains(s, "*"):
		return "", fmt.Errorf("host %q must not contain wildcards", h)
	case strings.Contains(s, "/"):
		if _, _, err := net.ParseCIDR(s); err == nil {
			return "", fmt.Errorf("host %q must not be a CIDR range", h)
		}
		return "", fmt.Errorf("host %q must be a hostname or IP address, not a URL or path", h)
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String(), nil
	}
	if strings.HasPrefix(s, "[") {
		return "", fmt.Errorf("host %q must not include brackets or a port", h)
	}
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("host %q must not include a port", h)
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if len(s) > 253 {
		return "", fmt.Errorf("host %q is longer than 253 characters", h)
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("host %q is not a valid hostname", h)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return "", fmt.Errorf("host %q is not a valid hostname", h)
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("host %q is not a valid hostname", h)
		}
	}
	return s, nil
}

// Validate checks every host entry and extends reference of a policy.
// A nil policy is valid (it contributes no restrictions).
func Validate(p *core.NetworkPolicy) error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, ref := range p.Extends {
		if _, _, err := Resolve(ref); err != nil {
			errs = append(errs, fmt.Errorf("extends: %w", err))
		}
	}
	for _, h := range p.AllowedHosts {
		if _, err := NormalizeHost(h); err != nil {
			errs = append(errs, fmt.Errorf("allowedHosts: %w", err))
		}
	}
	for _, h := range p.DeniedHosts {
		if _, err := NormalizeHost(h); err != nil {
			errs = append(errs, fmt.Errorf("deniedHosts: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Merge combines the factory-scope and agent-scope policies.
//
//   - If neither scope declares a policy (both nil), access is unrestricted.
//   - Otherwise the result is restricted: allowances from both scopes (explicit
//     allowedHosts and hosts supplied by extends) combine, and a denial at
//     either scope removes the host from the allowances of both scopes.
//   - A policy with only deniedHosts still restricts access (it is not an
//     unrestricted-access denylist); an empty policy contributes no hosts.
func Merge(factory, agent *core.NetworkPolicy) (core.EffectiveNetworkPolicy, error) {
	if factory == nil && agent == nil {
		return core.EffectiveNetworkPolicy{Restricted: false}, nil
	}
	if err := errors.Join(scopeErr("factory", Validate(factory)), scopeErr("agent", Validate(agent))); err != nil {
		return core.EffectiveNetworkPolicy{}, err
	}
	allowed := map[string]bool{}
	denied := map[string]bool{}
	for _, p := range []*core.NetworkPolicy{factory, agent} {
		if p == nil {
			continue
		}
		for _, ref := range p.Extends {
			_, hosts, _ := Resolve(ref)
			for _, h := range hosts {
				allowed[h] = true
			}
		}
		for _, h := range p.AllowedHosts {
			n, _ := NormalizeHost(h)
			allowed[n] = true
		}
		for _, h := range p.DeniedHosts {
			n, _ := NormalizeHost(h)
			denied[n] = true
		}
	}
	for h := range denied {
		delete(allowed, h)
	}
	return core.EffectiveNetworkPolicy{
		Restricted:   true,
		AllowedHosts: sortedKeys(allowed),
		DeniedHosts:  sortedKeys(denied),
	}, nil
}

func scopeErr(scope string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s networkPolicy: %w", scope, err)
}

// Allowed reports whether outbound access to host is permitted by policy.
// host may include a port or IPv6 brackets; matching is exact and
// case-insensitive (no implicit subdomain matching).
func Allowed(policy core.EffectiveNetworkPolicy, host string) bool {
	if !policy.Restricted {
		return true
	}
	h := stripPort(strings.TrimSpace(host))
	n, err := NormalizeHost(h)
	if err != nil {
		return false
	}
	for _, d := range policy.DeniedHosts {
		if d == n {
			return false
		}
	}
	for _, a := range policy.AllowedHosts {
		if a == n {
			return true
		}
	}
	return false
}

// WithRequired returns a copy of policy that additionally allows the given
// platform-required hosts (repository forges, configured MCP servers, image
// registries, the control plane). Required hosts cannot be blocked, even by
// deniedHosts. An unrestricted policy is returned unchanged.
func WithRequired(policy core.EffectiveNetworkPolicy, hosts ...string) core.EffectiveNetworkPolicy {
	if !policy.Restricted {
		return policy
	}
	allowed := map[string]bool{}
	for _, h := range policy.AllowedHosts {
		allowed[h] = true
	}
	required := map[string]bool{}
	for _, h := range hosts {
		if n, err := NormalizeHost(stripPort(h)); err == nil {
			allowed[n] = true
			required[n] = true
		}
	}
	var denied []string
	for _, d := range policy.DeniedHosts {
		if !required[d] {
			denied = append(denied, d)
		}
	}
	return core.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: sortedKeys(allowed), DeniedHosts: denied}
}

func stripPort(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			return h[1:i]
		}
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
