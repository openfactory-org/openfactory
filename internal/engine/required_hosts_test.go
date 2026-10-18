package engine

import (
	"slices"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/networkpolicy"
)

// TestRestrictedPolicyKeepsPlatformHosts is a regression test: a restricted
// network policy used to always-allow only the OF_PUBLIC_URL host. Forge,
// MCP server, image registry and inference hosts are platform-required too,
// and deniedHosts cannot block them.
func TestRestrictedPolicyKeepsPlatformHosts(t *testing.T) {
	env := NewTestEnv(t, func(d *core.CompiledDefinition) {
		a := &d.Agents[0]
		a.NetworkPolicy = core.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"pypi.org"}, DeniedHosts: []string{"github.com", "evil.test"}}
		a.MCPServers = map[string]core.MCPServer{"sentry": {URL: "https://mcp.sentry.test/mcp"}, "local": {Command: "npx"}}
		a.Machine = &core.MachineSpec{Name: "r", OS: "linux", Image: "registry.example.com/team/build:1"}
	})
	cfg := env.Agent(t, "foreman")
	hosts := RequiredHosts(cfg, RequiredHostsOptions{
		SandboxURL: "http://cp.test",
		Repos:      []core.RepoCheckout{{Forge: "GITHUB", Owner: "acme", Name: "api", CloneURL: "https://github.com/acme/api.git"}},
	})
	pol := networkpolicy.WithRequired(cfg.NetworkPolicy, hosts...)
	if !pol.Restricted {
		t.Fatal("policy no longer restricted")
	}
	for _, h := range []string{"cp.test", "github.com", "api.github.com", "codeload.github.com", "mcp.sentry.test", "registry.example.com", "api.anthropic.com", "pypi.org"} {
		if !slices.Contains(pol.AllowedHosts, h) {
			t.Errorf("allowed hosts %v missing %s", pol.AllowedHosts, h)
		}
	}
	if !slices.Equal(pol.DeniedHosts, []string{"evil.test"}) {
		t.Errorf("denied hosts = %v (a required host must not stay denied)", pol.DeniedHosts)
	}
}

func TestInferenceHosts(t *testing.T) {
	tests := []struct {
		name string
		h    core.HarnessConfig
		auth map[string]string
		want []string
	}{
		{"claude", core.HarnessConfig{Type: core.HarnessClaude}, nil, []string{"api.anthropic.com"}},
		{"claude custom base", core.HarnessConfig{Type: core.HarnessClaude}, map[string]string{"ANTHROPIC_BASE_URL": "https://llm-gw.internal:8443/anthropic"}, []string{"llm-gw.internal"}},
		{"claude bedrock", core.HarnessConfig{Type: core.HarnessClaude}, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "eu-west-1"}, []string{"bedrock-runtime.eu-west-1.amazonaws.com"}},
		{"codex", core.HarnessConfig{Type: core.HarnessCodex}, nil, []string{"api.openai.com"}},
		{"codex custom base", core.HarnessConfig{Type: core.HarnessCodex}, map[string]string{"OPENAI_BASE_URL": "https://openai.proxy.test/v1"}, []string{"openai.proxy.test"}},
		{"unknown harness", core.HarnessConfig{Type: "open", Model: "claude-sonnet-4-5"}, nil, nil},
	}
	for _, tc := range tests {
		if got := inferenceHosts(tc.h, tc.auth); !slices.Equal(got, tc.want) {
			t.Errorf("%s: hosts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRequiredHostsExported(t *testing.T) {
	cfg := func(typ core.HarnessType) core.AgentConfig {
		return core.AgentConfig{Harness: core.HarnessConfig{Type: typ, Model: "claude-sonnet-5-5"}}
	}
	opts := RequiredHostsOptions{SandboxURL: "https://cp.example.com/"}
	if h := RequiredHosts(cfg(core.HarnessClaude), opts); !slices.Contains(h, "api.anthropic.com") || !slices.Contains(h, "cp.example.com") {
		t.Errorf("claude: hosts %v", h)
	}
	if h := RequiredHosts(cfg(core.HarnessCodex), opts); !slices.Contains(h, "api.openai.com") || slices.Contains(h, "api.anthropic.com") {
		t.Errorf("codex: hosts %v", h)
	}
}
