package engine

import (
	"net/url"
	"slices"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
	"github.com/openfactory-org/openfactory/internal/secrets"
)

// platformNeeds carries the resolved per-run inputs that determine which
// platform hosts a sandbox must always reach.
type platformNeeds struct {
	Auth       map[string]string         // harness credential env (may hold *_BASE_URL)
	MCPServers map[string]core.MCPServer // resolved MCP servers, including the orchestration server
	Cloud      *core.CloudProviders      // factory cloudProviders (token exchange endpoints)
}

// Public forge hosts a checkout and the forge CLIs/APIs need.
var forgeHosts = map[string][]string{
	"GITHUB":       {"github.com", "api.github.com", "codeload.github.com"},
	"GITLAB":       {"gitlab.com"},
	"AZURE_DEVOPS": {"dev.azure.com"},
}

// requiredHosts lists the platform hosts a restricted network policy can never
// block (networkpolicy.WithRequired), per the network policy rules:
//
//   - the control plane (OF_INTERNAL_URL, default OF_PUBLIC_URL) for the orchestration MCP server;
//   - the run's repository forges: the public forge hosts, plus every clone URL
//     host (a configured GitHub Enterprise / self-managed GitLab base URL);
//   - every configured MCP server URL host;
//   - the image registry host of the sandbox image;
//   - the harness's inference hosts: api.anthropic.com (claude),
//     api.openai.com (codex), a configured ANTHROPIC_BASE_URL /
//     OPENAI_BASE_URL host instead, and the Bedrock runtime host for Claude
//     Code on Bedrock;
//   - with cloudProviders, the cloud token exchange hosts (sts.amazonaws.com;
//     sts.googleapis.com and iamcredentials.googleapis.com).
func requiredHosts(r *core.Run, spec core.SandboxSpec, needs platformNeeds, sandboxURL, defaultImage string) []string {
	var hosts []string
	add := func(h ...string) {
		for _, v := range h {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" && !slices.Contains(hosts, v) {
				hosts = append(hosts, v)
			}
		}
	}
	addURL := func(raw string) {
		if raw == "" {
			return
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			add(u.Hostname())
		} else if !strings.Contains(raw, "://") {
			// scp-like git URL: git@host:owner/repo.git
			if at := strings.LastIndex(raw, "@"); at >= 0 {
				raw = raw[at+1:]
			}
			if h, _, ok := strings.Cut(raw, ":"); ok {
				add(h)
			}
		}
	}

	addURL(sandboxURL)

	for _, co := range spec.Repos {
		addURL(co.CloneURL)
		pub := forgeHosts[strings.ToUpper(co.Forge)]
		if co.CloneURL == "" || (len(pub) > 0 && hostOf(co.CloneURL) == pub[0]) || co.Forge == "AZURE_DEVOPS" {
			add(pub...)
		}
	}

	for _, s := range needs.MCPServers {
		addURL(s.URL)
	}

	def := defaultImage
	if def == "" {
		def = sandbox.DefaultImage
	}
	if img := sandbox.SelectImage(spec, def); img != "" {
		add(registryHost(secrets.ImageHost(img)))
	}

	add(inferenceHosts(r.Config.Harness, needs.Auth)...)

	// Cloud identity federation exchanges the run token at the cloud's STS.
	if c := needs.Cloud; c != nil {
		if c.AWS != nil {
			add("sts.amazonaws.com")
		}
		if c.GCP != nil {
			add("sts.googleapis.com", "iamcredentials.googleapis.com")
		}
	}
	return hosts
}

// inferenceHosts returns the model provider hosts the harness calls.
func inferenceHosts(h core.HarnessConfig, auth map[string]string) []string {
	anthropic := func() []string {
		if auth["CLAUDE_CODE_USE_BEDROCK"] != "" {
			if b := hostOf(auth["ANTHROPIC_BEDROCK_BASE_URL"]); b != "" {
				return []string{b}
			}
			region := auth["AWS_REGION"]
			if region == "" {
				region = "us-east-1"
			}
			return []string{"bedrock-runtime." + region + ".amazonaws.com"}
		}
		if b := hostOf(auth["ANTHROPIC_BASE_URL"]); b != "" {
			return []string{b}
		}
		return []string{"api.anthropic.com"}
	}
	openai := func() []string {
		if b := hostOf(auth["OPENAI_BASE_URL"]); b != "" {
			return []string{b}
		}
		return []string{"api.openai.com"}
	}
	switch h.Type {
	case core.HarnessClaude, "claude-code":
		return anthropic()
	case core.HarnessCodex:
		return openai()
	}
	return nil
}

func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// registryHost maps Docker Hub's canonical name to the host actually dialed.
func registryHost(h string) string {
	if h == "docker.io" {
		return "registry-1.docker.io"
	}
	return h
}

// RequiredHostsOptions carries what RequiredHosts needs besides the agent
// config. Each field may be empty.
type RequiredHostsOptions struct {
	// SandboxURL is the control plane URL that a sandbox reaches.
	SandboxURL string
	// DefaultImage is the sandbox image when neither the environment nor the machine names one.
	DefaultImage string
	// Auth is the harness credential environment. It can hold a *_BASE_URL.
	Auth map[string]string
	// MCPServers are the MCP servers of the run. Nil means cfg.MCPServers.
	MCPServers map[string]core.MCPServer
	// Cloud is the cloud provider setting of the factory.
	Cloud *core.CloudProviders
	// Environment is the environment of the run. It can name an image.
	Environment *core.Environment
	// Repos are the checkouts of the run.
	Repos []core.RepoCheckout
}

// RequiredHosts returns the hosts that a restricted network policy of a run
// must always allow. It has no side effects. See requiredHosts for the rules.
func RequiredHosts(cfg core.AgentConfig, opts RequiredHostsOptions) []string {
	spec := core.SandboxSpec{Environment: opts.Environment, Repos: opts.Repos}
	if cfg.Machine != nil {
		spec.Machine = *cfg.Machine
	}
	servers := opts.MCPServers
	if servers == nil {
		servers = cfg.MCPServers
	}
	needs := platformNeeds{Auth: opts.Auth, MCPServers: servers, Cloud: opts.Cloud}
	return requiredHosts(&core.Run{Config: cfg}, spec, needs, strings.TrimRight(opts.SandboxURL, "/"), opts.DefaultImage)
}
