package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/skills"
)

// OrchestrationServerName is the MCP server name injected into every run.
const OrchestrationServerName = "factory"

// PlannerOptions configures a Planner. Store is required.
type PlannerOptions struct {
	Store   core.Store
	Secrets core.SecretStore
	Forges  core.ForgeResolver
	// SandboxURL is the base URL that sandboxes use to reach the control plane
	// (OF_INTERNAL_URL). The orchestration MCP server lives at
	// SandboxURL + "/mcp/run/{runId}".
	SandboxURL string
	// ResolveManagedMCP resolves an agent MCP server declared by managedId (an
	// entry of the team's MCP registry, or a built-in server) into a url or
	// command transport for run. When nil, runs whose agents use managedId
	// servers fail. See mcpregistry.Service.Resolve.
	ResolveManagedMCP func(ctx context.Context, run *core.Run, managedID string) (core.MCPServer, error)
	Clock             func() time.Time
	Logger            *slog.Logger
}

// Planner computes the content of a runner session: the environment, the
// secrets, the repositories, the skills, the MCP servers and the system
// prompt of a run. It holds no run state. The runner API and the dispatcher
// call it.
type Planner struct {
	opts  PlannerOptions
	store core.Store
	log   *slog.Logger
}

// NewPlanner builds a Planner.
func NewPlanner(opts PlannerOptions) *Planner {
	opts.SandboxURL = strings.TrimRight(opts.SandboxURL, "/")
	p := &Planner{opts: opts, store: opts.Store, log: opts.Logger}
	if p.log == nil {
		p.log = slog.Default()
	}
	if opts.Clock == nil {
		p.opts.Clock = time.Now
	}
	return p
}

func (x *Planner) now() time.Time { return x.opts.Clock().UTC() }

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runEnv builds the environment shared by sandbox setup and the harness:
// environment variables, agent secrets, run metadata and OF_* run identity.
func (x *Planner) runEnv(ctx context.Context, r *core.Run) (map[string]string, error) {
	env := map[string]string{}
	if r.Config.EnvironmentID != "" {
		envr, err := teamEnvironment(ctx, x.store, r.TeamID, r.Config.EnvironmentID)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", r.Config.EnvironmentID, err)
		}
		for k, v := range envr.EnvVars {
			if strings.HasPrefix(k, MetadataEnvPrefix) {
				x.log.Warn("ignoring reserved environment variable from environment", "name", k, "run_id", r.ID)
				continue
			}
			env[k] = v
		}
	}
	for _, name := range r.Config.Secrets {
		if !envNameRe.MatchString(name) || strings.HasPrefix(name, MetadataEnvPrefix) {
			x.log.Warn("secret name is not a usable environment variable; skipped", "secret", name, "run_id", r.ID)
			continue
		}
		if x.opts.Secrets == nil {
			return nil, fmt.Errorf("secret %q requested but no secret store is configured", name)
		}
		v, err := x.opts.Secrets.Get(ctx, r.TeamID, r.FactoryID, name)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", name, err)
		}
		env[name] = string(v)
	}
	maps.Copy(env, MetadataEnv(r.Metadata))
	env["OF_RUN_ID"] = r.ID
	env["OF_FACTORY_ID"] = r.FactoryID
	if r.WorkItemID != "" {
		env["OF_WORK_ITEM_ID"] = r.WorkItemID
	}
	if r.ParentRunID != "" {
		env["OF_PARENT_RUN_ID"] = r.ParentRunID
	}
	if r.AgentName != "" {
		env["OF_AGENT_NAME"] = r.AgentName
	}
	if x.opts.SandboxURL != "" {
		env["OF_PUBLIC_URL"] = x.opts.SandboxURL
	}
	return env, nil
}

// checkouts returns the repositories of a run, each with a clone URL that
// holds a forge credential.
func (x *Planner) checkouts(ctx context.Context, r *core.Run, rt *FactoryRuntime) ([]core.RepoCheckout, error) {
	var repos []core.RepoCheckout
	var repoRefs []core.RepoRef
	if r.Config.EnvironmentID != "" {
		envr, err := teamEnvironment(ctx, x.store, r.TeamID, r.Config.EnvironmentID)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", r.Config.EnvironmentID, err)
		}
		for _, s := range envr.Repos {
			ref, err := ParseRepoSpec(s)
			if err != nil {
				return nil, err
			}
			repoRefs = append(repoRefs, ref)
		}
	} else {
		repoRefs = rt.Repositories
	}
	credUser := x.credentialUser(ctx, r, rt)
	startRefs, err := loadRunRepoRefs(ctx, x.store, r)
	if err != nil {
		return nil, err
	}
	// The forge credential of the run is limited to these repositories.
	scope := credentialScope(repoRefs, startRefs)
	for _, ref := range repoRefs {
		co := core.RepoCheckout{Forge: strings.ToUpper(ref.CodeForge), Owner: ref.Owner, Name: ref.Name, Path: repoDir(ref)}
		if x.opts.Forges == nil {
			return nil, fmt.Errorf("repository %s: no forge resolver configured", ref.FullName())
		}
		forge, err := x.forgeFor(ctx, r, co.Forge, ref.FullName(), credUser, scope[co.Forge])
		if err != nil {
			return nil, fmt.Errorf("repository %s: %w", ref.FullName(), err)
		}
		u, err := forge.CloneURL(ctx, ref.Owner, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("repository %s: clone url: %w", ref.FullName(), err)
		}
		co.CloneURL = u
		repos = append(repos, co)
	}
	if len(startRefs) > 0 {
		repos, err = applyRepoRefs(repos, startRefs, func(forgeName, owner, name string) (string, error) {
			if x.opts.Forges == nil {
				return "", errors.New("no forge resolver configured")
			}
			forge, err := x.forgeFor(ctx, r, forgeName, owner+"/"+name, credUser, scope[strings.ToUpper(forgeName)])
			if err != nil {
				return "", err
			}
			return forge.CloneURL(ctx, owner, name)
		})
		if err != nil {
			return nil, err
		}
	}
	return repos, nil
}

// ParseRepoSpec parses an environment repository entry: "forge:owner/name",
// "owner/name" (GitHub) or a forge URL path.
func ParseRepoSpec(s string) (core.RepoRef, error) {
	s = strings.TrimSpace(s)
	forge := "GITHUB"
	if i := strings.Index(s, ":"); i > 0 && !strings.Contains(s[:i], "/") {
		switch f := strings.ToUpper(strings.ReplaceAll(s[:i], "-", "_")); f {
		case "HTTPS", "HTTP":
			if u, err := url.Parse(s); err == nil {
				s = strings.TrimPrefix(u.Path, "/")
				switch {
				case strings.Contains(u.Host, "gitlab"):
					forge = "GITLAB"
				case strings.Contains(u.Host, "dev.azure.com"), strings.Contains(u.Host, "visualstudio.com"):
					forge = "AZURE_DEVOPS"
				}
			}
		default:
			forge = f
			s = s[i+1:]
		}
	}
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return core.RepoRef{}, fmt.Errorf("%w: invalid repository %q (want forge:owner/name)", core.ErrInvalid, s)
	}
	return core.RepoRef{CodeForge: forge, Owner: s[:i], Name: s[i+1:]}, nil
}

func repoDir(ref core.RepoRef) string { return ref.Name }

func (x *Planner) repoPaths(rt *FactoryRuntime, r *core.Run) []string {
	if r.Config.EnvironmentID != "" {
		return nil // environment-defined layouts start at the workspace root
	}
	out := make([]string, 0, len(rt.Repositories))
	for _, ref := range rt.Repositories {
		out = append(out, repoDir(ref))
	}
	return out
}

// runSkills returns the skills of a run in this order: agent skills,
// factory skills, then the built-in skills for the agent type, the forges of
// the repositories and the connected integrations. The harness adds the
// repository skills that it finds before the built-in skills.
func (x *Planner) runSkills(ctx context.Context, r *core.Run, rt *FactoryRuntime, wi *core.WorkItem) []core.Skill {
	return runSkillsFor(ctx, x.store, r, rt, wi)
}

func runSkillsFor(ctx context.Context, st core.Store, r *core.Run, rt *FactoryRuntime, wi *core.WorkItem) []core.Skill {
	c := skills.Context{AgentType: r.Config.Type, SelfImprovement: r.TriggeredBy == selfImprovementSource}
	if wi != nil && wi.Source.Provider == selfImprovementSource {
		c.SelfImprovement = true
	}
	var factory []core.Skill
	if rt != nil {
		factory = rt.Skills
		for _, repo := range rt.Repositories {
			c.Forges = append(c.Forges, repo.CodeForge)
		}
	}
	if integ, err := st.GetFactoryIntegrations(ctx, r.FactoryID); err == nil {
		for p := range integ {
			c.Integrations = append(c.Integrations, p)
		}
		sort.Strings(c.Integrations)
	}
	return skills.Merge(r.Config.Skills, factory, nil, skills.ForRun(c))
}

// selfImprovementSource is the trigger and the source provider of
// Self-improvement runs and work items (see internal/scoring).
const selfImprovementSource = "self_improvement"

// mcpServers expands {{SECRET}} placeholders in env/headers and injects the
// orchestration server.
func (x *Planner) mcpServers(ctx context.Context, r *core.Run) (map[string]core.MCPServer, error) {
	out := make(map[string]core.MCPServer, len(r.Config.MCPServers)+1)
	for name, s := range r.Config.MCPServers {
		if name == OrchestrationServerName {
			x.log.Warn("agent MCP server name collides with the orchestration server; replaced", "run_id", r.ID)
			continue
		}
		if s.ManagedID != "" {
			resolved, err := x.resolveManaged(ctx, r, name, s)
			if err != nil {
				return nil, err
			}
			s = resolved
		}
		s.Args = slices.Clone(s.Args)
		var err error
		if s.Env, err = x.expandMap(ctx, r, s.Env); err != nil {
			return nil, fmt.Errorf("mcp server %q env: %w", name, err)
		}
		if s.Headers, err = x.expandMap(ctx, r, s.Headers); err != nil {
			return nil, fmt.Errorf("mcp server %q headers: %w", name, err)
		}
		out[name] = s
	}
	if x.opts.SandboxURL != "" {
		// The runner sends its session token to this server (see
		// sessionTokenServers), so the entry has no Authorization header.
		out[OrchestrationServerName] = core.MCPServer{URL: x.opts.SandboxURL + "/mcp/run/" + r.ID}
	}
	return out, nil
}

// resolveManaged turns a managedId server into its registered transport. Env
// and headers declared on the agent are layered over the registered ones.
func (x *Planner) resolveManaged(ctx context.Context, run *core.Run, name string, s core.MCPServer) (core.MCPServer, error) {
	if x.opts.ResolveManagedMCP == nil {
		return s, fmt.Errorf("mcp server %q: managedId %q cannot be resolved (no MCP registry configured)", name, s.ManagedID)
	}
	got, err := x.opts.ResolveManagedMCP(ctx, run, s.ManagedID)
	if err != nil {
		return s, fmt.Errorf("mcp server %q: managedId %q: %w", name, s.ManagedID, err)
	}
	if got.URL == "" && got.Command == "" {
		return s, fmt.Errorf("mcp server %q: managedId %q has no url or command", name, s.ManagedID)
	}
	got.ManagedID = ""
	got.Env = mergeStrMaps(got.Env, s.Env)
	agentHeaders := s.Headers
	if x.opts.SandboxURL != "" && strings.HasPrefix(got.URL, x.opts.SandboxURL+"/mcp/") {
		// Control-plane servers (proxy, built-in) authenticate with the
		// session token of the runner: the agent must not replace it.
		agentHeaders = make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			if !strings.EqualFold(k, "Authorization") {
				agentHeaders[k] = v
			}
		}
	}
	got.Headers = mergeStrMaps(got.Headers, agentHeaders)
	return got, nil
}

func mergeStrMaps(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

func (x *Planner) expandMap(ctx context.Context, r *core.Run, m map[string]string) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.Contains(v, "{{") {
			if x.opts.Secrets == nil {
				return nil, errors.New("secret placeholders require a secret store")
			}
			ev, err := x.opts.Secrets.Expand(ctx, r.TeamID, r.FactoryID, v)
			if err != nil {
				return nil, err
			}
			v = ev
		}
		out[k] = v
	}
	return out, nil
}

// harnessAuth resolves the harness credential of a run into the environment
// variables the harness expects. env is the environment of the run (runEnv).
// See resolveHarnessAuth.
func (x *Planner) harnessAuth(ctx context.Context, r *core.Run, env map[string]string) (map[string]string, error) {
	auth, _, err := x.resolveHarnessAuth(ctx, r, env)
	return auth, err
}

// resolveHarnessAuth resolves the harness credential of a run. The order is:
//
//  1. harness.auth of the effective configuration (definition and
//     overrides): managedSecret maps the named secret, workerEnvironment
//     passes nothing.
//  2. Without harness.auth, when env (environment variables and agent
//     secrets of the run) already holds a model credential, nothing: the
//     CLI uses that credential, as before.
//  3. Otherwise the default credential (findDefaultCredential): the one
//     factory or team secret whose type the harness accepts.
//  4. Otherwise nothing.
//
// The HarnessCredential result names the source and never holds a value.
func (x *Planner) resolveHarnessAuth(ctx context.Context, r *core.Run, env map[string]string) (map[string]string, HarnessCredential, error) {
	if a := r.Config.Harness.Auth; a != nil {
		if a.Source == CredentialWorkerEnvironment {
			return nil, HarnessCredential{Source: CredentialWorkerEnvironment}, nil
		}
		if a.SecretName == "" || (a.Source != "" && a.Source != CredentialManagedSecret) {
			return nil, HarnessCredential{Source: CredentialNone}, nil
		}
		auth, typ, err := x.secretAuth(ctx, r, r.FactoryID, a.SecretName)
		return auth, HarnessCredential{Source: CredentialManagedSecret, SecretName: a.SecretName, Type: typ}, err
	}
	if harness.HasCredentialEnv(r.Config.Harness.Type, env) {
		return nil, HarnessCredential{Source: CredentialNone, Reason: CredentialReasonRunEnv}, nil
	}
	cred, err := findDefaultCredential(ctx, x.opts.Secrets, r.TeamID, r.FactoryID, r.Config.Harness.Type)
	if err != nil || !cred.IsDefault() {
		return nil, cred, err
	}
	factoryID := ""
	if cred.Scope == CredentialScopeFactory {
		factoryID = r.FactoryID
	}
	auth, _, err := x.secretAuth(ctx, r, factoryID, cred.SecretName)
	return auth, cred, err
}

// secretAuth maps the managed secret name to harness environment variables.
// An empty factoryID reads the team secret.
func (x *Planner) secretAuth(ctx context.Context, r *core.Run, factoryID, name string) (map[string]string, core.SecretType, error) {
	if x.opts.Secrets == nil {
		return nil, "", fmt.Errorf("harness auth secret %q: no secret store configured", name)
	}
	val, err := x.opts.Secrets.Get(ctx, r.TeamID, factoryID, name)
	if err != nil {
		return nil, "", fmt.Errorf("harness auth secret %q: %w", name, err)
	}
	typ := secrets.TypeOf(ctx, x.opts.Secrets, r.TeamID, factoryID, name)
	return HarnessAuthEnv(r.Config.Harness.Type, r.Config.Harness.Model, typ, val), typ, nil
}

// HarnessAuthEnv maps a credential to harness environment variables.
func HarnessAuthEnv(h core.HarnessType, model string, typ core.SecretType, value []byte) map[string]string {
	v := strings.TrimSpace(string(value))
	switch typ {
	case core.SecretAnthropicKey:
		return map[string]string{"ANTHROPIC_API_KEY": v}
	case core.SecretAnthropicOAuth:
		// A Claude Code OAuth token works only with the claude CLI.
		if h != core.HarnessClaude {
			return map[string]string{}
		}
		return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": v}
	case core.SecretOpenAIKey:
		return openAIEnv(h, v)
	case core.SecretBedrockKey:
		env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
		cred := parseCredential(value)
		if key := firstOf(cred, "api_key", "apiKey", "token", "bearer_token"); key != "" {
			env["AWS_BEARER_TOKEN_BEDROCK"] = key
		} else {
			env["AWS_BEARER_TOKEN_BEDROCK"] = v
		}
		if region := firstOf(cred, "region", "aws_region", "AWS_REGION"); region != "" {
			env["AWS_REGION"] = region
		}
		return env
	case core.SecretBedrockAccess:
		cred := parseCredential(value)
		env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
		set := func(k string, keys ...string) {
			if s := firstOf(cred, keys...); s != "" {
				env[k] = s
			}
		}
		set("AWS_ACCESS_KEY_ID", "access_key_id", "accessKeyId", "aws_access_key_id", "AWS_ACCESS_KEY_ID")
		set("AWS_SECRET_ACCESS_KEY", "secret_access_key", "secretAccessKey", "aws_secret_access_key", "AWS_SECRET_ACCESS_KEY")
		set("AWS_SESSION_TOKEN", "session_token", "sessionToken", "aws_session_token", "AWS_SESSION_TOKEN")
		set("AWS_REGION", "region", "aws_region", "AWS_REGION")
		return env
	}
	switch h {
	case core.HarnessCodex:
		return openAIEnv(h, v)
	case core.HarnessClaude:
		return map[string]string{"ANTHROPIC_API_KEY": v}
	default:
		if looksOpenAI(model) {
			return openAIEnv(h, v)
		}
		return map[string]string{"ANTHROPIC_API_KEY": v}
	}
}

func openAIEnv(h core.HarnessType, v string) map[string]string {
	env := map[string]string{"OPENAI_API_KEY": v}
	if h == core.HarnessCodex {
		env["CODEX_API_KEY"] = v
	}
	return env
}

func looksOpenAI(model string) bool {
	m := strings.ToLower(model)
	for _, p := range []string{"gpt", "o1", "o3", "o4", "openai/", "codex"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

// parseCredential decodes a JSON object credential, falling back to {"password": value}.
func parseCredential(v []byte) map[string]string {
	out := map[string]string{}
	var obj map[string]any
	if err := json.Unmarshal(v, &obj); err == nil {
		for k, val := range obj {
			switch t := val.(type) {
			case string:
				out[k] = t
			case nil:
			default:
				b, _ := json.Marshal(t)
				out[k] = string(b)
			}
		}
		return out
	}
	out["password"] = strings.TrimSpace(string(v))
	return out
}
