// Package harness holds the registry of agent harnesses (rtcore.Harness implementations)
// and helpers shared by the adapters in open/, claude/ and codex/: skill and MCP
// configuration injection into the sandbox, credential-to-env mapping, model catalogs
// and process streaming.
//
// Every harness runs inside a rtcore.Sandbox and only touches it through
// Sandbox.Start/Exec/WriteFile/ReadFile, so the same adapter works for local,
// docker, kubernetes and remote (direct) sandboxes.
package harness

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Registry maps harness types to implementations.
type Registry struct {
	mu sync.RWMutex
	m  map[rtcore.HarnessType]rtcore.Harness
}

// NewRegistry returns a registry pre-populated with hs.
func NewRegistry(hs ...rtcore.Harness) *Registry {
	r := &Registry{m: map[rtcore.HarnessType]rtcore.Harness{}}
	for _, h := range hs {
		r.Register(h)
	}
	return r
}

// Register adds or replaces a harness.
func (r *Registry) Register(h rtcore.Harness) {
	if h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[NormalizeType(string(h.Type()))] = h
}

// Get returns the harness for t (aliases accepted; empty selects open).
// The error wraps rtcore.ErrNotFound when the harness is not registered.
func (r *Registry) Get(t rtcore.HarnessType) (rtcore.Harness, error) {
	nt := NormalizeType(string(t))
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.m[nt]
	if !ok {
		return nil, fmt.Errorf("harness %q: %w", t, rtcore.ErrNotFound)
	}
	return h, nil
}

// Types lists the registered harness types, sorted.
func (r *Registry) Types() []rtcore.HarnessType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]rtcore.HarnessType, 0, len(r.m))
	for t := range r.m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NormalizeType canonicalizes a harness identifier: "claude-code" -> claude,
// "" -> claude (the default harness). Unknown values are returned lower-cased.
func NormalizeType(s string) rtcore.HarnessType {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "claude", "claude-code", "claude_code", "claudecode":
		return rtcore.HarnessClaude
	case "codex", "codex-cli":
		return rtcore.HarnessCodex
	}
	return rtcore.HarnessType(v)
}

// IsKnownType reports whether s names one of the built-in harness types (aliases accepted).
func IsKnownType(s string) bool {
	switch NormalizeType(s) {
	case rtcore.HarnessClaude, rtcore.HarnessCodex:
		return true
	}
	return false
}
