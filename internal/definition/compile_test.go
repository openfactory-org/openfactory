package definition

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

const baseFactoryHead = `schemaVersion: v1
name: test
repositories:
  - owner: acme
    name: api
`

const baseFactory = baseFactoryHead + `agentDefaults:
  model: claude-opus-5-5
`

const foremanMD = "---\nagentType: FOREMAN\n---\nCoordinate the work.\n"

const linuxMachine = `platform:
  os: linux
  linux:
    image: ubuntu:24.04
`

// tree builds a definition from the base (factory.yaml + foreman) plus
// path/content pairs. A content of "-" deletes the path.
func defTree(kv ...string) map[string]string {
	f := map[string]string{"factory.yaml": baseFactory, "agents/foreman/agent.md": foremanMD}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(f, kv[i])
		} else {
			f[kv[i]] = kv[i+1]
		}
	}
	return f
}

// factoryWith returns factory.yaml with extra top-level YAML appended.
func factoryWith(extra string) string { return baseFactory + extra }

// factoryDefaults returns factory.yaml with a custom agentDefaults body.
func factoryDefaults(defaults string) string {
	return baseFactoryHead + "agentDefaults:\n" + indent(defaults, "  ")
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func agentMD(frontmatter, body string) string {
	if body == "" {
		body = "Do the work."
	}
	return "---\n" + frontmatter + "---\n" + body + "\n"
}

func automationMD(frontmatter string) string {
	return "---\n" + frontmatter + "---\nHandle the event.\n"
}

func mustCompile(t *testing.T, files map[string]string) *core.CompiledDefinition {
	t.Helper()
	def, errs := Compile(files)
	if len(errs) > 0 {
		var b strings.Builder
		for _, e := range errs {
			fmt.Fprintf(&b, "\n  %s:%d [%s] %s", e.File, e.Line, e.Field, e.Message)
		}
		t.Fatalf("unexpected errors:%s", b.String())
	}
	if def == nil {
		t.Fatal("nil definition without errors")
	}
	return def
}

func agentByName(t *testing.T, def *core.CompiledDefinition, name string) core.AgentConfig {
	t.Helper()
	for _, a := range def.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %s not found", name)
	return core.AgentConfig{}
}

type errCase struct {
	name  string
	files map[string]string
	file  string // expected file of the error
	msg   string // expected substring
	line  int    // expected line (0 = don't check)
}

func runErrCases(t *testing.T, cases []errCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, errs := Compile(tc.files)
			if def != nil {
				t.Errorf("expected nil definition on error")
			}
			for _, e := range errs {
				if e.File == tc.file && strings.Contains(e.Message, tc.msg) {
					if tc.line != 0 && e.Line != tc.line {
						t.Errorf("error %q at line %d, want line %d", e.Message, e.Line, tc.line)
					}
					return
				}
			}
			var got []string
			for _, e := range errs {
				got = append(got, fmt.Sprintf("%s:%d %s", e.File, e.Line, e.Message))
			}
			t.Errorf("no error in %s containing %q; got:\n  %s", tc.file, tc.msg, strings.Join(got, "\n  "))
		})
	}
}

func TestMinimalDefinition(t *testing.T) {
	def := mustCompile(t, defTree())
	if def.SchemaVersion != "v1" || def.Name != "test" {
		t.Errorf("got %+v", def)
	}
	if !def.GithubOnboardingNotices || def.FactoryConfigChecks != "main" {
		t.Error("factory defaults not applied")
	}
	if def.IntegrationsSet {
		t.Error("IntegrationsSet must be false when integrations is omitted")
	}
	if def.Repositories[0].CodeForge != "GITHUB" {
		t.Errorf("default codeForge = %q", def.Repositories[0].CodeForge)
	}
	f := agentByName(t, def, "foreman")
	if f.Type != core.AgentForeman || f.Instructions != "Coordinate the work." {
		t.Errorf("foreman = %+v", f)
	}
	if f.Harness != (core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-opus-5-5"}) {
		t.Errorf("harness = %+v", f.Harness)
	}
	if f.SpawnableBy != nil {
		t.Error("spawnableBy should be nil (foreman only) when omitted")
	}
	if f.NetworkPolicy.Restricted {
		t.Error("network should be unrestricted without policies")
	}
}

func TestFactoryValidation(t *testing.T) {
	runErrCases(t, []errCase{
		{"missing factory.yaml", defTree("factory.yaml", "-"), "factory.yaml", "factory.yaml is required", 0},
		{"wrong schemaVersion", defTree("factory.yaml", strings.Replace(baseFactory, "schemaVersion: v1\n", "schemaVersion: v2\n", 1)), "factory.yaml", "schemaVersion must be v1", 1},
		{"missing name", defTree("factory.yaml", strings.Replace(baseFactory, "name: test\n", "", 1)), "factory.yaml", "name is required", 0},
		{"missing repositories", defTree("factory.yaml", "schemaVersion: v1\nname: x\nagentDefaults:\n  model: m\n"), "factory.yaml", "repositories is required", 0},
		{"unknown key with line", defTree("factory.yaml", factoryWith("bogus: 1\n")), "factory.yaml", `unknown key "bogus"`, 8},
		{"unknown nested key", defTree("factory.yaml", factoryWith("github:\n  checks: all\n")), "factory.yaml", `unknown key "github.checks"`, 9},
		{"duplicate key", defTree("factory.yaml", factoryWith("name: again\n")), "factory.yaml", `duplicate key "name"`, 8},
		{"invalid yaml", defTree("factory.yaml", baseFactory+"  - : [\n"), "factory.yaml", "invalid YAML", 0},
		{"not a mapping", defTree("factory.yaml", "- a\n- b\n"), "factory.yaml", "document must be an object", 1},
		{"name wrong type", defTree("factory.yaml", strings.Replace(baseFactory, "name: test", "name: [x]", 1)), "factory.yaml", "name must be a string", 2},
		{"alias chars", defTree("factory.yaml", factoryWith("alias: \"bad/alias\"\n")), "factory.yaml", "alias must be 1-60", 8},
		{"alias too long", defTree("factory.yaml", factoryWith("alias: "+strings.Repeat("a", 61)+"\n")), "factory.yaml", "alias must be 1-60", 8},
		{"credentialStrategy", defTree("factory.yaml", factoryWith("credentialStrategy: OWNER\n")), "factory.yaml", "credentialStrategy must be one of EXECUTOR, CREATOR", 8},
		{"factoryConfigChecks", defTree("factory.yaml", factoryWith("github:\n  factoryConfigChecks: some\n")), "factory.yaml", "must be one of main, all", 9},
		{"githubOnboardingNotices type", defTree("factory.yaml", factoryWith("automations:\n  githubOnboardingNotices: nope\n")), "factory.yaml", "must be true or false", 9},
		{"secret name", defTree("factory.yaml", factoryWith("secrets: [bad-name]\n")), "factory.yaml", "not a valid secret name", 8},
		{"duplicate secret", defTree("factory.yaml", factoryWith("secrets: [A, A]\n")), "factory.yaml", "more than once", 8},
	})
}

func TestAgentDefaultsValidation(t *testing.T) {
	runErrCases(t, []errCase{
		{"missing agentDefaults", defTree("factory.yaml", baseFactoryHead), "factory.yaml", "agentDefaults is required", 0},
		{"neither model nor harness", defTree("factory.yaml", factoryDefaults("machine: x\n"), "machines/x.yaml", linuxMachine), "factory.yaml", "exactly one of model or harness", 0},
		{"both model and harness", defTree("factory.yaml", factoryDefaults("model: a\nharness:\n  type: codex\n  model: b\n")), "factory.yaml", "at most one of model or harness", 8},
		{"harness requires type", defTree("factory.yaml", factoryDefaults("harness:\n  model: b\n")), "factory.yaml", "agentDefaults.harness.type is required", 0},
		{"harness requires model", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n")), "factory.yaml", "agentDefaults.harness.model is required", 0},
		{"harness bad type", defTree("factory.yaml", factoryDefaults("harness:\n  type: gemini\n  model: x\n")), "factory.yaml", "must be one of claude, codex, claude-code", 8},
		{"open is not a harness", defTree("factory.yaml", factoryDefaults("harness:\n  type: open\n  model: x\n")), "factory.yaml", "must be one of claude, codex, claude-code", 8},
		{"shorthand model selects no harness", defTree("factory.yaml", factoryDefaults("model: auto\n")), "factory.yaml", "selects no harness", 0},
		{"auth source required", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: x\n  auth:\n    secretName: K\n")), "factory.yaml", "auth.source is required", 0},
		{"auth bad source", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: x\n  auth:\n    source: env\n")), "factory.yaml", "must be one of managedSecret, workerEnvironment", 0},
		{"managedSecret requires secretName", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: x\n  auth:\n    source: managedSecret\n")), "factory.yaml", "secretName is required when auth.source is managedSecret", 0},
		{"workerEnvironment rejects secretName", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: x\n  workerHost: w\n  auth:\n    source: workerEnvironment\n    secretName: K\n")), "factory.yaml", "not allowed when auth.source is workerEnvironment", 0},
		{"workerEnvironment requires workerHost", defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: x\n  auth:\n    source: workerEnvironment\n")), "factory.yaml", "requires agentDefaults.workerHost", 0},
		{"unknown agentDefaults key", defTree("factory.yaml", factoryDefaults("model: m\nidleTimeoutMinutes: 5\n")), "factory.yaml", `unknown key "agentDefaults.idleTimeoutMinutes"`, 8},
		{"default machine missing", defTree("factory.yaml", factoryDefaults("model: m\nmachine: nope\n")), "factory.yaml", `"nope" does not match any machines`, 8},
		{"default machine must be linux", defTree("factory.yaml", factoryDefaults("model: m\nmachine: mac\n"), "machines/mac.yaml", "platform:\n  os: macos\n"), "factory.yaml", "must be a Linux machine", 8},
		{"mcp conflict with factory-wide", defTree("factory.yaml", factoryDefaults("model: m\nmcpServers:\n  s:\n    command: x\n")+"mcpServers:\n  s:\n    command: y\n"), "factory.yaml", "already granted factory-wide", 0},
	})
}

func TestHarnessAliasAndAuth(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryDefaults("harness:\n  type: claude-code\n  model: claude-opus-5-5\n  reasoningLevel: high\n  auth:\n    source: managedSecret\n    secretName: ANTHROPIC_API_KEY\n")))
	h := agentByName(t, def, "foreman").Harness
	want := core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-opus-5-5", ReasoningLevel: "high", Auth: &core.HarnessAuth{Source: "managedSecret", SecretName: "ANTHROPIC_API_KEY"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("harness = %+v", h)
	}
	def = mustCompile(t, defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: gpt\n  auth:\n    source: workerEnvironment\nworkerHost: my-worker\n")))
	f := agentByName(t, def, "foreman")
	if f.WorkerHost != "my-worker" || f.Harness.Auth.Source != "workerEnvironment" {
		t.Errorf("got %+v", f)
	}
}

func TestRepositoriesAndForges(t *testing.T) {
	runErrCases(t, []errCase{
		{"bad forge", defTree("factory.yaml", factoryWith("codeForges: [BITBUCKET]\n")), "factory.yaml", "codeForges values must be one of", 8},
		{"empty forges requires empty repos", defTree("factory.yaml", factoryWith("codeForges: []\n")), "factory.yaml", "repositories must be empty when codeForges is an empty list", 0},
		{"multi forge requires codeForge", defTree("factory.yaml", factoryWith("codeForges: [GITHUB, GITLAB]\n")), "factory.yaml", "codeForge is required when codeForges lists more than one", 0},
		{"repo forge not enabled", defTree("factory.yaml", strings.Replace(baseFactory, "  - owner: acme", "  - codeForge: GITLAB\n    owner: acme", 1)+"codeForges: [GITHUB]\n"), "factory.yaml", "is not listed in codeForges", 4},
		{"repo missing owner", defTree("factory.yaml", strings.Replace(baseFactory, "  - owner: acme\n", "  - codeForge: GITHUB\n", 1)), "factory.yaml", "repositories[0].owner is required", 0},
		{"github owner with slash", defTree("factory.yaml", strings.Replace(baseFactory, "owner: acme", "owner: acme/sub", 1)), "factory.yaml", "is not a valid owner for GITHUB", 0},
		{"repo name with slash", defTree("factory.yaml", strings.Replace(baseFactory, "name: api", "name: a/b", 1)), "factory.yaml", "must be a single repository name", 0},
		{"duplicate repo", defTree("factory.yaml", strings.Replace(baseFactory, "    name: api\n", "    name: api\n  - owner: ACME\n    name: API\n", 1)), "factory.yaml", "listed more than once", 0},
		{"substitution source not owned", defTree("factory.yaml", factoryWith("benchmarkRepoSubstitutions:\n  - source: {codeForge: GITHUB, owner: other, name: x}\n    target: {codeForge: GITHUB, owner: fork, name: x}\n")), "factory.yaml", "must be one of the factory's repositories", 0},
		{"substitution target owned", defTree("factory.yaml", factoryWith("benchmarkRepoSubstitutions:\n  - source: {codeForge: GITHUB, owner: acme, name: api}\n    target: {codeForge: GITHUB, owner: acme, name: api}\n")), "factory.yaml", "must not be one of the factory's repositories", 0},
		{"substitution must be github", defTree("factory.yaml", factoryWith("benchmarkRepoSubstitutions:\n  - source: {codeForge: GITLAB, owner: acme, name: api}\n    target: {codeForge: GITHUB, owner: f, name: x}\n")), "factory.yaml", "must be GITHUB", 0},
	})

	def := mustCompile(t, defTree("factory.yaml", strings.Replace(baseFactory, "  - owner: acme\n    name: api\n", "  - owner: acme/platform\n    name: infra\n", 1)+"codeForges: [GITLAB]\n"))
	if def.Repositories[0].CodeForge != "GITLAB" || def.Repositories[0].Owner != "acme/platform" {
		t.Errorf("single forge default not applied: %+v", def.Repositories)
	}
	def = mustCompile(t, defTree("factory.yaml", strings.Replace(baseFactory, "  - owner: acme\n    name: api\n", "  []\n", 1)+"codeForges: []\n"))
	if len(def.Repositories) != 0 || def.CodeForges == nil {
		t.Errorf("empty forges: %+v", def)
	}
	def = mustCompile(t, defTree("factory.yaml", factoryWith("benchmarkRepoSubstitutions:\n  - source: {codeForge: GITHUB, owner: acme, name: api}\n    target: {codeForge: GITHUB, owner: bench, name: api}\n")))
	if subs := def.BenchmarkRepoSubstitutions; len(subs) != 1 || subs[0].Source.Owner != "acme" || subs[0].Target.Owner != "bench" || subs[0].Target.Name != "api" || subs[0].Target.CodeForge != "GITHUB" {
		t.Errorf("benchmark repo substitutions = %+v", subs)
	}
}

func TestIntegrations(t *testing.T) {
	runErrCases(t, []errCase{
		{"linear and jira exclusive", defTree("factory.yaml", factoryWith("integrations:\n  - type: linear\n  - type: jira\n")), "factory.yaml", "mutually exclusive", 0},
		{"duplicate integration", defTree("factory.yaml", factoryWith("integrations:\n  - type: slack\n  - type: slack\n")), "factory.yaml", "declared more than once", 0},
		{"unknown integration", defTree("factory.yaml", factoryWith("integrations:\n  - type: github\n")), "factory.yaml", "must be one of slack, microsoft-teams, linear, jira", 0},
		{"settings on wrong type", defTree("factory.yaml", factoryWith("integrations:\n  - type: linear\n    slack:\n      autoRespondToThreadReplies: true\n")), "factory.yaml", "only valid on an integration of type slack", 0},
		{"missing type", defTree("factory.yaml", factoryWith("integrations:\n  - slack: {}\n")), "factory.yaml", "integrations[0].type is required", 0},
	})
	def := mustCompile(t, defTree("factory.yaml", factoryWith("integrations:\n  - type: slack\n    slack:\n      autoRespondToThreadReplies: false\n  - type: linear\n    linear:\n      teamIds: [abc]\n")))
	if !def.IntegrationsSet || len(def.Integrations) != 2 || def.Integrations[0].Settings["autoRespondToThreadReplies"] != "false" {
		t.Errorf("integrations = %+v", def.Integrations)
	}
	def = mustCompile(t, defTree("factory.yaml", factoryWith("integrations: []\n")))
	if !def.IntegrationsSet || len(def.Integrations) != 0 {
		t.Error("empty integrations must detach every provider")
	}
}

func TestMCPServerValidation(t *testing.T) {
	mcp := func(body string) map[string]string {
		return defTree("factory.yaml", factoryWith("mcpServers:\n"+indent(body, "  ")))
	}
	runErrCases(t, []errCase{
		{"two transports", mcp("s:\n  command: x\n  url: https://a.example\n"), "factory.yaml", "exactly one of managedId, command or url", 0},
		{"no transport", mcp("s:\n  args: [a]\n"), "factory.yaml", "exactly one of managedId, command or url", 0},
		{"args without command", mcp("s:\n  url: https://a.example\n  args: [a]\n"), "factory.yaml", "args is only valid with command", 0},
		{"env without command", mcp("s:\n  managedId: m\n  env: {A: b}\n"), "factory.yaml", "env is only valid with command", 0},
		{"headers without url", mcp("s:\n  command: x\n  headers: {A: b}\n"), "factory.yaml", "headers is only valid with url", 0},
		{"relative url", mcp("s:\n  url: /mcp\n"), "factory.yaml", "absolute http or https URL", 0},
		{"ftp url", mcp("s:\n  url: ftp://a.example/x\n"), "factory.yaml", "absolute http or https URL", 0},
		{"reserved name", mcp("factory:\n  command: x\n"), "factory.yaml", "reserved for the orchestration server", 0},
		{"bad name", mcp("\"bad name\":\n  command: x\n"), "factory.yaml", "must be 1-64 letters", 0},
		{"unknown key", mcp("s:\n  command: x\n  cwd: /tmp\n"), "factory.yaml", `unknown key "mcpServers.s.cwd"`, 0},
		{"env value not string", mcp("s:\n  command: x\n  env: {A: [1]}\n"), "factory.yaml", "must be a string", 0},
	})
	def := mustCompile(t, mcp("a:\n  managedId: abc\nb:\n  command: uvx\n  args: [dbt-mcp]\n  env: {TOKEN: \"{{DBT_TOKEN}}\"}\nc:\n  url: https://mcp.example.com/x\n  headers: {Authorization: \"Bearer {{T}}\"}\n"))
	srv := agentByName(t, def, "foreman").MCPServers
	if len(srv) != 3 || srv["b"].Args[0] != "dbt-mcp" || srv["b"].Env["TOKEN"] != "{{DBT_TOKEN}}" || srv["c"].Headers["Authorization"] != "Bearer {{T}}" {
		t.Errorf("mcp = %+v", srv)
	}
}

func TestNetworkPolicyValidation(t *testing.T) {
	np := func(body string) map[string]string {
		return defTree("factory.yaml", factoryWith("networkPolicy:\n"+indent(body, "  ")))
	}
	runErrCases(t, []errCase{
		{"url host", np("allowedHosts: [\"https://a.example.com\"]\n"), "factory.yaml", "not a URL", 9},
		{"port", np("allowedHosts: [\"a.example.com:443\"]\n"), "factory.yaml", "port", 9},
		{"cidr", np("deniedHosts: [\"10.0.0.0/8\"]\n"), "factory.yaml", "CIDR", 9},
		{"wildcard", np("allowedHosts: [\"*.example.com\"]\n"), "factory.yaml", "wildcards", 9},
		{"unknown policy", np("extends: builtin/nope@1.0.0\n"), "factory.yaml", "unknown built-in policy", 9},
		{"bad selector", np("extends: [builtin/codex@~1]\n"), "factory.yaml", "invalid version selector", 9},
		{"unknown key", np("allowed: []\n"), "factory.yaml", `unknown key "networkPolicy.allowed"`, 9},
		{"agent-level bad host", defTree("agents/foreman/agent.md", agentMD("agentType: FOREMAN\nnetworkPolicy:\n  allowedHosts: [\"a b\"]\n", "")), "agents/foreman/agent.md", "not a valid hostname", 4},
	})

	files := np("extends: builtin/codex@1.0.0\nallowedHosts: [API.Example.com]\ndeniedHosts: [blocked.example.com, chatgpt.com]\n")
	files["agents/reviewer/agent.md"] = agentMD("networkPolicy:\n  allowedHosts: [docs.example.com, blocked.example.com]\n", "")
	files["agents/open/agent.md"] = agentMD("networkPolicy: null\n", "")
	def := mustCompile(t, files)
	r := agentByName(t, def, "reviewer").NetworkPolicy
	if !r.Restricted {
		t.Fatal("expected restricted policy")
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has(r.AllowedHosts, "api.example.com") || !has(r.AllowedHosts, "docs.example.com") || !has(r.AllowedHosts, "api.openai.com") {
		t.Errorf("allowed = %v", r.AllowedHosts)
	}
	if has(r.AllowedHosts, "blocked.example.com") || has(r.AllowedHosts, "chatgpt.com") {
		t.Errorf("denials must win: %v", r.AllowedHosts)
	}
	if fr := agentByName(t, def, "foreman").NetworkPolicy; has(fr.AllowedHosts, "docs.example.com") {
		t.Error("agent allowance leaked into another agent")
	}
	// Empty factory policy denies everything (restricted, no hosts).
	def = mustCompile(t, defTree("factory.yaml", factoryWith("networkPolicy: {}\n")))
	if p := agentByName(t, def, "foreman").NetworkPolicy; !p.Restricted || len(p.AllowedHosts) != 0 {
		t.Errorf("empty policy = %+v", p)
	}
}

func TestSelfImprovementAndCloud(t *testing.T) {
	runErrCases(t, []errCase{
		{"threshold low", defTree("factory.yaml", factoryWith("selfImprovement:\n  failedRunThreshold: 0\n")), "factory.yaml", "between 1 and 50", 9},
		{"threshold high", defTree("factory.yaml", factoryWith("selfImprovement:\n  failedRunThreshold: 51\n")), "factory.yaml", "between 1 and 50", 9},
		{"reviewerType", defTree("factory.yaml", factoryWith("selfImprovement:\n  reviewerType: everyone\n")), "factory.yaml", "must be one of admins, team, custom, none", 9},
		{"custom requires emails", defTree("factory.yaml", factoryWith("selfImprovement:\n  reviewerType: custom\n")), "factory.yaml", "required when reviewerType is custom", 0},
		{"emails only for custom", defTree("factory.yaml", factoryWith("selfImprovement:\n  reviewerEmails: [a@b.co]\n")), "factory.yaml", "only valid when reviewerType is custom", 9},
		{"bad email", defTree("factory.yaml", factoryWith("selfImprovement:\n  reviewerType: custom\n  reviewerEmails: [nope]\n")), "factory.yaml", "is not an email address", 10},
		{"gcp project number unquoted", defTree("factory.yaml", factoryWith("cloudProviders:\n  gcp:\n    projectNumber: 123\n    workloadIdentityFederationPoolId: p\n    workloadIdentityFederationProviderId: q\n")), "factory.yaml", "quote 123", 10},
		{"gcp missing pool", defTree("factory.yaml", factoryWith("cloudProviders:\n  gcp:\n    projectNumber: \"1\"\n    workloadIdentityFederationProviderId: q\n")), "factory.yaml", "workloadIdentityFederationPoolId is required", 0},
		{"aws arn", defTree("factory.yaml", factoryWith("cloudProviders:\n  aws:\n    roleArn: role\n")), "factory.yaml", "not an IAM role ARN", 10},
		{"both cloud keys", defTree("factory.yaml", factoryWith("cloudProviders: {}\nproviders:\n  aws: {roleArn: \"arn:aws:iam::1:role/x\"}\n")), "factory.yaml", "providers", 0},
	})
	def := mustCompile(t, defTree("factory.yaml", factoryWith("cloudProviders:\n  aws:\n    roleArn: arn:aws:iam::123456789012:role/factory\nselfImprovement:\n  reviewerType: custom\n  reviewerEmails: [a@example.com]\n")))
	if def.CloudProviders == nil || def.CloudProviders.AWS.RoleARN == "" {
		t.Error("cloudProviders not applied")
	}
	if def.SelfImprovement.ReviewerType != "custom" || def.SelfImprovement.ReviewerEmails[0] != "a@example.com" {
		t.Errorf("selfImprovement = %+v", def.SelfImprovement)
	}
	def = mustCompile(t, defTree("factory.yaml", factoryWith("selfImprovement: {}\n")))
	if def.SelfImprovement.ReviewerType != "admins" {
		t.Error("reviewerType default must be admins")
	}
}

func TestAgentValidation(t *testing.T) {
	runErrCases(t, []errCase{
		{"no agents", defTree("agents/foreman/agent.md", "-"), "agents", "at least one agents/<name>/agent.md", 0},
		{"no foreman", defTree("agents/foreman/agent.md", agentMD("agentType: CUSTOM\n", "")), "agents", "exactly one agent must have agentType FOREMAN", 0},
		{"two foremen", defTree("agents/other/agent.md", agentMD("agentType: MAIN\n", "")), "agents/other/agent.md", "exactly one agent can be the foreman", 2},
		{"bad agentType", defTree("agents/x/agent.md", agentMD("agentType: BOSS\n", "")), "agents/x/agent.md", "agentType must be one of", 2},
		{"unknown key line", defTree("agents/x/agent.md", agentMD("description: d\nname: x\n", "")), "agents/x/agent.md", `unknown key "name"`, 3},
		{"model and harness", defTree("agents/x/agent.md", agentMD("model: a\nharness:\n  model: b\n", "")), "agents/x/agent.md", "at most one of model or harness", 3},
		{"idle low", defTree("agents/x/agent.md", agentMD("idleTimeoutMinutes: -1\n", "")), "agents/x/agent.md", "between 0 and 60", 2},
		{"snapshot not boolean", defTree("agents/x/agent.md", agentMD("snapshot: sometimes\n", "")), "agents/x/agent.md", "snapshot must be true or false", 2},
		{"idle high", defTree("agents/x/agent.md", agentMD("idleTimeoutMinutes: 61\n", "")), "agents/x/agent.md", "between 0 and 60", 2},
		{"spawnableBy unknown", defTree("agents/x/agent.md", agentMD("spawnableBy: [foreman, ghost]\n", "")), "agents/x/agent.md", `unknown agent "ghost"`, 2},
		{"spawnableBy duplicate", defTree("agents/x/agent.md", agentMD("spawnableBy: [foreman, foreman]\n", "")), "agents/x/agent.md", "more than once", 0},
		{"unknown machine", defTree("agents/x/agent.md", agentMD("machine: nope\n", "")), "agents/x/agent.md", `machine "nope" does not match`, 2},
		{"credentialStrategy", defTree("agents/x/agent.md", agentMD("credentialStrategy: ME\n", "")), "agents/x/agent.md", "must be one of EXECUTOR, CREATOR", 2},
		{"unclosed frontmatter", defTree("agents/x/agent.md", "---\ndescription: d\n"), "agents/x/agent.md", "not closed", 1},
		{"bad frontmatter yaml", defTree("agents/x/agent.md", "---\ndescription: [\n---\nbody\n"), "agents/x/agent.md", "invalid YAML", 0},
		{"misplaced agent file", defTree("agents/x.md", "hi"), "agents/x.md", "agents/<name>/agent.md", 0},
		{"bad agent name", defTree("agents/-x/agent.md", foremanMD), "agents/-x/agent.md", "agent name", 0},
		{"change harness type without model", defTree("agents/x/agent.md", agentMD("harness:\n  type: codex\n", "")), "agents/x/agent.md", "model is required when changing the harness type", 3},
		{"workerEnvironment without host", defTree("agents/x/agent.md", agentMD("harness:\n  type: claude\n  model: m\n  auth:\n    source: workerEnvironment\n", "")), "agents/x/agent.md", "requires a self-hosted workerHost", 0},
		{"clearing worker host breaks inherited auth",
			defTree("factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: m\n  auth:\n    source: workerEnvironment\nworkerHost: w\n"), "agents/x/agent.md", agentMD("workerHost: null\n", "")),
			"agents/x/agent.md", "requires a self-hosted workerHost", 0},
		{"agent mcp conflicts with factory-wide", defTree("factory.yaml", factoryWith("mcpServers:\n  s:\n    command: x\n"), "agents/x/agent.md", agentMD("mcpServers:\n  s:\n    command: y\n", "")), "agents/x/agent.md", "already granted factory-wide", 0},
	})
}

func TestAgentResolution(t *testing.T) {
	factory := factoryDefaults(`harness:
  type: codex
  model: gpt-5.3-codex
  reasoningLevel: high
  auth:
    source: managedSecret
    secretName: OPENAI_API_KEY
machine: linux
environmentId: ENV1
secrets: [DEFAULT_SECRET]
mcpServers:
  default-mcp:
    managedId: m1
workerHost: worker-1
`) + "secrets: [FACTORY_SECRET]\nmcpServers:\n  factory-mcp:\n    url: https://mcp.example.com\ncredentialStrategy: CREATOR\n"
	files := defTree(
		"factory.yaml", factory,
		"machines/linux.yaml", linuxMachine,
		"machines/mac.yaml", "platform:\n  os: macos\n",
		"agents/foreman/agent.md", foremanMD,
		"agents/sparse/agent.md", agentMD("harness:\n  model: gpt-6\n", ""),
		"agents/noauth/agent.md", agentMD("harness:\n  auth: null\n", ""),
		"agents/switch/agent.md", agentMD("harness:\n  type: claude-code\n  model: claude-opus-5-5\n", ""),
		"agents/shorthand/agent.md", agentMD("model: claude-sonnet-5-5\nmachine: mac\nenvironmentId: null\nworkerHost: \"\"\n", ""),
		"agents/samecli/agent.md", agentMD("model: gpt-5\n", ""),
		"agents/secrets/agent.md", agentMD("secrets: [AGENT_SECRET]\nmcpServers:\n  agent-mcp:\n    command: run\n", ""),
		"agents/nosecrets/agent.md", agentMD("secrets: []\nmcpServers: {}\n", ""),
		"agents/spawn/agent.md", agentMD("spawnableBy: []\nidleTimeoutMinutes: 15\ncredentialStrategy: EXECUTOR\ndescription: Spawn test\nagentType: VERIFY\n", "Instructions here."),
		"agents/plain/agent.md", "Just instructions, no frontmatter.\n",
	)
	def := mustCompile(t, files)

	f := agentByName(t, def, "foreman")
	if f.Harness.Type != core.HarnessCodex || f.Harness.ReasoningLevel != "high" || f.Harness.Auth.SecretName != "OPENAI_API_KEY" {
		t.Errorf("inherited harness = %+v", f.Harness)
	}
	if f.Machine == nil || f.Machine.Name != "linux" || f.EnvironmentID != "ENV1" || f.WorkerHost != "worker-1" {
		t.Errorf("inherited settings = %+v", f)
	}
	if !reflect.DeepEqual(f.Secrets, []string{"DEFAULT_SECRET", "FACTORY_SECRET"}) {
		t.Errorf("secrets = %v", f.Secrets)
	}
	if len(f.MCPServers) != 2 || f.MCPServers["default-mcp"].ManagedID != "m1" || f.MCPServers["factory-mcp"].URL == "" {
		t.Errorf("mcp = %v", f.MCPServers)
	}
	if f.CredentialStrategy != "CREATOR" {
		t.Errorf("credential = %q", f.CredentialStrategy)
	}

	if h := agentByName(t, def, "sparse").Harness; h.Type != core.HarnessCodex || h.Model != "gpt-6" || h.Auth == nil || h.ReasoningLevel != "high" {
		t.Errorf("sparse override = %+v", h)
	}
	if h := agentByName(t, def, "noauth").Harness; h.Auth != nil || h.Model != "gpt-5.3-codex" {
		t.Errorf("auth: null must clear inherited auth: %+v", h)
	}
	if h := agentByName(t, def, "switch").Harness; h != (core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-opus-5-5"}) {
		t.Errorf("type switch must drop inherited auth/reasoning: %+v", h)
	}
	s := agentByName(t, def, "shorthand")
	// A Claude model selects the claude harness; the codex settings do not carry over.
	if s.Harness != (core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-sonnet-5-5"}) {
		t.Errorf("model shorthand = %+v", s.Harness)
	}
	// An OpenAI model keeps the codex harness and its settings.
	if h := agentByName(t, def, "samecli").Harness; h.Type != core.HarnessCodex || h.Model != "gpt-5" || h.Auth == nil || h.ReasoningLevel != "high" {
		t.Errorf("same-harness shorthand = %+v", h)
	}
	if s.Machine.OS != "macos" || s.EnvironmentID != "" || s.WorkerHost != "" {
		t.Errorf("overrides = %+v", s)
	}
	sec := agentByName(t, def, "secrets")
	if !reflect.DeepEqual(sec.Secrets, []string{"AGENT_SECRET", "FACTORY_SECRET"}) {
		t.Errorf("agent secrets must replace defaults: %v", sec.Secrets)
	}
	if _, ok := sec.MCPServers["default-mcp"]; ok || len(sec.MCPServers) != 2 {
		t.Errorf("agent mcp must replace defaults, keep factory-wide: %v", sec.MCPServers)
	}
	ns := agentByName(t, def, "nosecrets")
	if !reflect.DeepEqual(ns.Secrets, []string{"FACTORY_SECRET"}) || len(ns.MCPServers) != 1 {
		t.Errorf("empty lists remove defaults but keep factory-wide: %v %v", ns.Secrets, ns.MCPServers)
	}
	sp := agentByName(t, def, "spawn")
	if sp.SpawnableBy == nil || len(sp.SpawnableBy) != 0 {
		t.Errorf("spawnableBy [] must be empty non-nil: %#v", sp.SpawnableBy)
	}
	if b, _ := json.Marshal(sp.SpawnableBy); string(b) != "[]" {
		t.Errorf("spawnableBy JSON = %s", b)
	}
	if sp.IdleTimeoutMinutes != 15 || sp.CredentialStrategy != "EXECUTOR" || sp.Type != core.AgentVerify || sp.Description != "Spawn test" || sp.Instructions != "Instructions here." {
		t.Errorf("spawn = %+v", sp)
	}
	if p := agentByName(t, def, "plain"); p.Type != core.AgentCustom || p.Instructions != "Just instructions, no frontmatter." {
		t.Errorf("plain = %+v", p)
	}
	// Agents are sorted by name.
	names := make([]string, len(def.Agents))
	for i, a := range def.Agents {
		names[i] = a.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("agents not sorted: %v", names)
	}
}

func TestMainAliasIsForeman(t *testing.T) {
	def := mustCompile(t, defTree("agents/foreman/agent.md", agentMD("agentType: MAIN\n", "")))
	if agentByName(t, def, "foreman").Type != core.AgentForeman {
		t.Error("MAIN must normalize to FOREMAN")
	}
}

func TestAutomationValidation(t *testing.T) {
	trig := func(trigger string) map[string]string {
		return defTree("automations/a/automation.md", automationMD("triggers:\n"+indent(trigger, "  ")))
	}
	withInteg := func(integ, trigger string) map[string]string {
		f := trig(trigger)
		f["factory.yaml"] = factoryWith("integrations:\n  - type: " + integ + "\n")
		return f
	}
	withHooks := func(f map[string]string, names ...string) map[string]string {
		for _, n := range names {
			f["webhooks/"+n+".yaml"] = "secretName: HOOK\n"
		}
		return f
	}
	runErrCases(t, []errCase{
		{"no frontmatter", defTree("automations/a/automation.md", "Just text\n"), "automations/a/automation.md", "must start with YAML frontmatter", 1},
		{"empty body", defTree("automations/a/automation.md", "---\ntriggers:\n  - provider: schedule\n    event: cron_fired\n    schedule: {cron: \"@daily\"}\n---\n\n"), "automations/a/automation.md", "starting prompt", 0},
		{"missing triggers", defTree("automations/a/automation.md", automationMD("enabled: true\n")), "automations/a/automation.md", "triggers is required", 0},
		{"empty triggers", defTree("automations/a/automation.md", automationMD("triggers: []\n")), "automations/a/automation.md", "at least one trigger", 2},
		{"unknown agent", defTree("automations/a/automation.md", automationMD("agent: ghost\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", `agent "ghost" does not match`, 2},
		{"unknown provider", trig("- provider: bitbucket\n  event: push\n"), "automations/a/automation.md", "unknown trigger provider", 3},
		{"dotted event", trig("- provider: github\n  event: github.push\n"), "automations/a/automation.md", `unknown github event "github.push"`, 4},
		{"unknown key", defTree("automations/a/automation.md", automationMD("trigger: []\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", `unknown key "trigger"`, 2},
		{"unknown trigger key", trig("- provider: github\n  event: push\n  filters: {}\n"), "automations/a/automation.md", `unknown key "triggers[0].filters"`, 5},
		// GitHub
		{"unknown filter key", trig("- provider: github\n  event: push\n  filter:\n    labels: [x]\n"), "automations/a/automation.md", `unknown filter key "labels"`, 6},
		{"skip_bots value", trig("- provider: github\n  event: push\n  filter:\n    skip_bots: some\n"), "automations/a/automation.md", "must be one of all, self, none", 6},
		{"pr_numbers ints", trig("- provider: github\n  event: pull_request_opened\n  filter:\n    pr_numbers: [\"42\"]\n"), "automations/a/automation.md", "must be an integer", 6},
		{"review_states enum", trig("- provider: github\n  event: pull_request_review_submitted\n  filter:\n    review_states: [rejected]\n"), "automations/a/automation.md", "must be one of approved, changes_requested, commented", 6},
		{"alias with native", trig("- provider: github\n  event: pull_request_opened\n  filter:\n    base_branches: [main]\n    baseBranches: [dev]\n"), "automations/a/automation.md", "don't combine baseBranches", 7},
		{"unknown operator", trig("- provider: github\n  event: push\n  filter:\n    branches:\n      any: [main]\n"), "automations/a/automation.md", `unknown matcher operator "any"`, 7},
		{"in and not_in overlap", trig("- provider: github\n  event: issue_labeled\n  filter:\n    labels:\n      in: [a]\n      not_in: [a]\n"), "automations/a/automation.md", "appears in both in and not_in", 0},
		{"scalar matcher", trig("- provider: github\n  event: push\n  filter:\n    branches: main\n"), "automations/a/automation.md", "must be a list of values or a matcher object", 6},
		// Linear
		{"linear alias matcher object", withInteg("linear", "- provider: linear\n  event: issue_labeled\n  filter:\n    teams:\n      in: [Eng]\n"), "automations/a/automation.md", "accepts only a plain list", 0},
		{"linear issues ints", withInteg("linear", "- provider: linear\n  event: comment_created\n  filter:\n    issues: [ENG-42]\n"), "automations/a/automation.md", "must be an integer", 6},
		{"linear issues not on issue_labeled", withInteg("linear", "- provider: linear\n  event: issue_labeled\n  filter:\n    issues: [42]\n"), "automations/a/automation.md", `unknown filter key "issues"`, 6},
		{"linear uuid", withInteg("linear", "- provider: linear\n  event: issue_labeled\n  filter:\n    team_ids: [Engineering]\n"), "automations/a/automation.md", "must be a UUID", 6},
		{"linear alias with native", withInteg("linear", "- provider: linear\n  event: issue_labeled\n  filter:\n    team_ids: [\"2f1c1a5e-8f0a-4c55-9e1c-3d2b7a9e4f10\"]\n    teams: [Eng]\n"), "automations/a/automation.md", "don't combine teams", 0},
		// Slack
		{"slack itemUsers only on reaction_added", withInteg("slack", "- provider: slack\n  event: app_mention\n  filter:\n    itemUsers: [bob]\n"), "automations/a/automation.md", `unknown filter key "itemUsers"`, 0},
		{"slack channels not_in", withInteg("slack", "- provider: slack\n  event: message_posted\n  filter:\n    channels:\n      not_in: [x]\n"), "automations/a/automation.md", "accepts only a plain list", 0},
		{"slack without integration", withInteg("linear", "- provider: slack\n  event: app_mention\n"), "automations/a/automation.md", "require the slack integration", 3},
		// Teams
		{"teams requires filter", withInteg("microsoft-teams", "- provider: teams\n  event: app_mention\n"), "automations/a/automation.md", "filter is required", 0},
		{"teams requires channel_ids", withInteg("microsoft-teams", "- provider: teams\n  event: app_mention\n  filter:\n    team_ids: [t]\n"), "automations/a/automation.md", "channel_ids is required", 0},
		{"teams exactly one team", withInteg("microsoft-teams", "- provider: teams\n  event: app_mention\n  filter:\n    team_ids: [a, b]\n    channel_ids: [c]\n"), "automations/a/automation.md", "exactly one value", 6},
		{"teams in only", withInteg("microsoft-teams", "- provider: teams\n  event: message_posted\n  filter:\n    team_ids: [a]\n    channel_ids:\n      not_in: [c]\n"), "automations/a/automation.md", "supports only in", 0},
		{"teams alias with native", withInteg("microsoft-teams", "- provider: teams\n  event: app_mention\n  filter:\n    team_ids: [t]\n    channels: [eng]\n    channel_ids: [c]\n"), "automations/a/automation.md", "don't combine channels", 0},
		{"teams users alias not_in", withInteg("microsoft-teams", "- provider: teams\n  event: app_mention\n  filter:\n    team_ids: [t]\n    channel_ids: [c]\n    users:\n      not_in: [x]\n"), "automations/a/automation.md", "accepts only a plain list", 0},
		// Jira
		{"jira statuses only on status_changed", withInteg("jira", "- provider: jira\n  event: issue_created\n  filter:\n    statuses: [Done]\n"), "automations/a/automation.md", `unknown filter key "statuses"`, 0},
		{"jira projects alias with native", withInteg("jira", "- provider: jira\n  event: issue_created\n  filter:\n    projects: [Website]\n    project_keys: [WEB]\n"), "automations/a/automation.md", "don't combine projects", 0},
		// Webhook
		{"webhook requires filter", trig("- provider: webhook\n  event: received\n"), "automations/a/automation.md", "filter is required", 0},
		{"webhook requires webhook_ids", trig("- provider: webhook\n  event: received\n  filter:\n    payload: {a: [1]}\n"), "automations/a/automation.md", "webhook_ids is required", 0},
		{"webhook one id", withHooks(trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [a, b]\n"), "a", "b"), "automations/a/automation.md", "one-item list", 6},
		{"webhook unknown name", withHooks(trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [nope]\n"), "b", "a"), "automations/a/automation.md", `webhook "nope" does not exist; known: a, b`, 6},
		{"webhook unknown name in", withHooks(trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids:\n      in: [nope]\n"), "a"), "automations/a/automation.md", `webhook "nope" does not exist; known: a`, 7},
		{"webhook none defined", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [nope]\n"), "automations/a/automation.md", "known: none", 6},
		{"webhook file name", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [ci.yaml]\n"), "automations/a/automation.md", "not a file name", 0},
		{"payload scalar leaf", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload:\n      status: failed\n"), "automations/a/automation.md", "must be a list of allowed values", 8},
		{"payload exists false with in", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload:\n      env: {exists: false, in: [prod]}\n"), "automations/a/automation.md", "exists: false with a non-empty in", 0},
		{"payload overlap", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload:\n      env: {in: [a], not_in: [a]}\n"), "automations/a/automation.md", "appears in both in and not_in", 0},
		{"payload too deep", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload: " + deepPattern(11) + "\n"), "automations/a/automation.md", "deeper than 10 levels", 0},
		{"payload too many values", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload:\n      a: [" + numbers(33) + "]\n"), "automations/a/automation.md", "more than 32 values", 0},
		{"payload too many leaves", trig("- provider: webhook\n  event: received\n  filter:\n    webhook_ids: [w]\n    payload: " + manyLeaves(65) + "\n"), "automations/a/automation.md", "more than 64 leaf fields", 0},
		// Schedule
		{"schedule required", trig("- provider: schedule\n  event: cron_fired\n"), "automations/a/automation.md", "schedule is required", 0},
		{"schedule rejects filter", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"@daily\"}\n  filter: {}\n"), "automations/a/automation.md", "accept no filter", 6},
		{"schedule_ids managed", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"@daily\"}\n  filter:\n    schedule_ids: [x]\n"), "automations/a/automation.md", "accept no filter", 0},
		{"bad cron", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"61 * * * *\"}\n"), "automations/a/automation.md", "schedule.cron", 5},
		{"six-field cron", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"0 0 9 * * 1\"}\n"), "automations/a/automation.md", "five-field", 5},
		{"tz prefix", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"TZ=Europe/Paris 0 9 * * *\"}\n"), "automations/a/automation.md", "always UTC", 5},
		{"bad descriptor", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"@sometimes\"}\n"), "automations/a/automation.md", "schedule.cron", 5},
		{"two unnamed schedules", trig("- {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n- {provider: schedule, event: cron_fired, schedule: {cron: \"@hourly\"}}\n"), "automations/a/automation.md", "at most one schedule trigger", 4},
		{"duplicate schedule names", trig("- {provider: schedule, event: cron_fired, schedule: {name: a, cron: \"@daily\"}}\n- {provider: schedule, event: cron_fired, schedule: {name: a, cron: \"@hourly\"}}\n"), "automations/a/automation.md", "used more than once", 4},
		{"schedule on github trigger", trig("- provider: github\n  event: push\n  schedule: {cron: \"@daily\"}\n"), "automations/a/automation.md", "only valid for schedule/cron_fired", 5},
		{"schedule unknown key", trig("- provider: schedule\n  event: cron_fired\n  schedule: {cron: \"@daily\", tz: UTC}\n"), "automations/a/automation.md", `unknown key "triggers[0].schedule.tz"`, 0},
		// Others
		{"gitlab mentioned managed", trig("- provider: gitlab\n  event: bot_mentioned\n  filter:\n    mentioned: [bot]\n"), "automations/a/automation.md", "server-managed", 6},
		{"azure push unavailable", trig("- provider: azure_devops\n  event: push\n"), "automations/a/automation.md", "not available", 4},
		{"azure repos uuid", trig("- provider: azure_devops\n  event: pull_request_created\n  filter:\n    repos: [my-repo]\n"), "automations/a/automation.md", "must be a UUID", 6},
		{"azure source_repos managed", trig("- provider: azure_devops\n  event: pull_request_created\n  filter:\n    source_repos: [x]\n"), "automations/a/automation.md", "server-managed", 6},
		{"factory provider unknown", trig("- provider: factory\n  event: work_item_stage_changed\n"), "automations/a/automation.md", `unknown trigger provider "factory"`, 3},
		{"jira status ids strings", withInteg("jira", "- provider: jira\n  event: status_changed\n  filter:\n    status_ids: [10001]\n"), "automations/a/automation.md", "must be a string", 6},
		// Overrides
		{"override machine", defTree("automations/a/automation.md", automationMD("machine: nope\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", `machine "nope" does not match`, 2},
		{"override idle", defTree("automations/a/automation.md", automationMD("idleTimeoutMinutes: 90\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", "between 0 and 60", 2},
		{"override model and harness", defTree("automations/a/automation.md", automationMD("model: a\nharness: {model: b}\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", "at most one of model or harness", 3},
		{"enabled type", defTree("automations/a/automation.md", automationMD("enabled: yes please\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n")), "automations/a/automation.md", "must be true or false", 2},
	})
}

func deepPattern(depth int) string {
	s := "[1]"
	for i := 0; i < depth; i++ {
		s = "{a: " + s + "}"
	}
	return s
}

func numbers(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprint(i)
	}
	return strings.Join(parts, ", ")
}

func manyLeaves(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("k%d: [1]", i)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func TestAutomationCompilation(t *testing.T) {
	files := defTree(
		"factory.yaml", factoryDefaults("harness:\n  type: codex\n  model: gpt\n  auth: {source: managedSecret, secretName: OPENAI_API_KEY}\nsecrets: [D]\n")+"secrets: [F]\nmcpServers:\n  fw:\n    managedId: x\nintegrations:\n  - type: slack\n  - type: microsoft-teams\n  - type: linear\n",
		"machines/linux.yaml", linuxMachine,
		"webhooks/ci.yaml", "secretName: CI_SECRET\n",
		"agents/reviewer/agent.md", agentMD("agentType: REVIEW\n", ""),
		"automations/default/automation.md", automationMD(`triggers:
  - provider: github
    event: pull_request_opened
    filter:
      repos: [acme/api]
      baseBranches: [main]
      prNumbers:
        not_in: [1]
      labels:
        not_in: [wip]
      paths: null
      keywords: []
      skip_bots: self
`),
		"automations/override/automation.md", automationMD(`agent: reviewer
enabled: false
displayName: Override test
harness:
  model: gpt-6
machine: linux
environmentId: null
secrets: [S]
mcpServers:
  extra:
    command: run
workerHost: null
idleTimeoutMinutes: 7
triggers:
  - provider: teams
    event: app_mention
    filter:
      team_ids: ["11111111-1111-4111-8111-111111111111"]
      channel_ids:
        in: ["19:example@thread.tacv2"]
  - provider: slack
    event: reaction_added
    filter:
      channels: ["#eng "]
      itemUsers: [alex@example.com]
      emojis: [eyes]
  - provider: linear
    event: comment_created
    filter:
      teams: [Engineering]
      issues: [42]
  - provider: webhook
    event: received
    filter:
      webhook_ids: [ci]
      payload:
        action: [opened, reopened]
        pull_request:
          draft: [false]
        sender:
          type:
            not_in: [Bot]
  - provider: schedule
    event: cron_fired
    schedule:
      name: weekday
      cron: "0 9 * * 1-5"
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "@every 1h"
`),
		"automations/model/automation.md", automationMD("model: claude-haiku-5-5\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n"),
	)
	def := mustCompile(t, files)
	byName := map[string]core.Automation{}
	for _, a := range def.Automations {
		byName[a.Name] = a
	}
	d := byName["default"]
	if d.Agent != "foreman" || !d.Enabled || d.Overrides != nil || d.Prompt != "Handle the event." {
		t.Errorf("default automation = %+v", d)
	}
	var filter map[string]any
	if err := json.Unmarshal(d.Triggers[0].Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if _, ok := filter["base_branches"]; !ok {
		t.Errorf("baseBranches alias must be rewritten: %v", filter)
	}
	if _, ok := filter["pr_numbers"].(map[string]any)["not_in"]; !ok {
		t.Errorf("prNumbers matcher lost: %v", filter)
	}
	if _, ok := filter["paths"]; ok {
		t.Error("null matcher must be dropped")
	}
	if _, ok := filter["keywords"]; ok {
		t.Error("empty matcher must be dropped")
	}
	if filter["skip_bots"] != "self" {
		t.Errorf("skip_bots = %v", filter["skip_bots"])
	}

	o := byName["override"]
	if o.Agent != "reviewer" || o.Enabled || o.DisplayName != "Override test" {
		t.Errorf("override automation = %+v", o)
	}
	ov := o.Overrides
	if ov == nil || ov.Harness == nil {
		t.Fatalf("overrides = %+v", ov)
	}
	if ov.Harness.Type != core.HarnessCodex || ov.Harness.Model != "gpt-6" || ov.Harness.Auth == nil {
		t.Errorf("sparse harness override = %+v", ov.Harness)
	}
	if ov.Machine != "linux" || ov.EnvironmentID == nil || *ov.EnvironmentID != "" || ov.WorkerHost == nil || *ov.WorkerHost != "" {
		t.Errorf("overrides = %+v", ov)
	}
	if !reflect.DeepEqual(ov.Secrets, []string{"S", "F"}) {
		t.Errorf("override secrets = %v", ov.Secrets)
	}
	if len(ov.MCPServers) != 2 || ov.MCPServers["fw"].ManagedID != "x" {
		t.Errorf("override mcp = %v", ov.MCPServers)
	}
	if ov.IdleTimeoutMinutes == nil || *ov.IdleTimeoutMinutes != 7 {
		t.Errorf("override = %+v", ov)
	}
	if o.Triggers[0].Provider != ProviderTeams {
		t.Errorf("teams provider must normalize to %s, got %s", ProviderTeams, o.Triggers[0].Provider)
	}
	var slack map[string]any
	_ = json.Unmarshal(o.Triggers[1].Filter, &slack)
	if ch := slack["channels"].([]any); ch[0] != "#eng" {
		t.Errorf("slack channel names must be trimmed: %v", ch)
	}
	if o.Triggers[4].Schedule.Name != "weekday" || o.Triggers[5].Schedule.Cron != "@every 1h" {
		t.Errorf("schedules = %+v %+v", o.Triggers[4].Schedule, o.Triggers[5].Schedule)
	}
	if o.Triggers[4].Filter != nil {
		t.Error("schedule trigger must have no filter")
	}
	var wh map[string]any
	_ = json.Unmarshal(o.Triggers[3].Filter, &wh)
	if wh["webhook_ids"].([]any)[0] != "ci" || wh["payload"] == nil {
		t.Errorf("webhook filter = %v", wh)
	}

	m := byName["model"]
	if m.Overrides == nil || *m.Overrides.Harness != (core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-haiku-5-5"}) {
		t.Errorf("model shorthand override = %+v", m.Overrides)
	}
}

func TestMachineValidation(t *testing.T) {
	r := func(content string) map[string]string { return defTree("machines/r.yaml", content) }
	runErrCases(t, []errCase{
		{"missing platform", r("description: x\n"), "machines/r.yaml", "platform is required", 0},
		{"linux requires image", r("platform:\n  os: linux\n"), "machines/r.yaml", "require platform.linux.image", 0},
		{"default os linux requires image", r("platform: {}\n"), "machines/r.yaml", "require platform.linux.image", 0},
		{"bad os", r("platform:\n  os: freebsd\n"), "machines/r.yaml", "must be one of linux, macos, windows", 2},
		{"mac arch", r("platform:\n  os: macos\n  arch: x86_64\n"), "machines/r.yaml", "macOS machines support only aarch64", 3},
		{"windows arch", r("platform:\n  os: windows\n  arch: aarch64\n"), "machines/r.yaml", "Windows machines support only x86_64", 3},
		{"mac version", r("platform:\n  os: macos\n  mac:\n    version: \"13\"\n"), "machines/r.yaml", "must be one of 14, 15, 26, 27", 4},
		{"mac version unquoted", r("platform:\n  os: macos\n  mac:\n    version: 26\n"), "machines/r.yaml", "quote 26", 4},
		{"linux settings on mac", r("platform:\n  os: macos\n  linux:\n    image: x\n"), "machines/r.yaml", "only valid when platform.os is linux", 4},
		{"mac settings on windows", r("platform:\n  os: windows\n  mac: {version: \"26\"}\n"), "machines/r.yaml", "only valid when platform.os is macos", 3},
		{"shape both", r("instanceShape:\n  vcpus: 4\n" + linuxMachine), "machines/r.yaml", "both vcpus and memoryGb", 2},
		{"shape power of two", r("instanceShape:\n  vcpus: 6\n  memoryGb: 8\n" + linuxMachine), "machines/r.yaml", "positive powers of two", 2},
		{"windows shape power of two", r("instanceShape:\n  vcpus: 4\n  memoryGb: 12\nplatform:\n  os: windows\n"), "machines/r.yaml", "positive powers of two", 2},
		{"mac shape", r("instanceShape:\n  vcpus: 4\n  memoryGb: 8\nplatform:\n  os: macos\n"), "machines/r.yaml", "macOS instanceShape must be one of", 2},
		{"retention", r("failureSessionRetentionMinutes: 61\n" + linuxMachine), "machines/r.yaml", "between 1 and 60", 1},
		{"unknown key", r("image: x\n" + linuxMachine), "machines/r.yaml", `unknown key "image"`, 1},
		{"registry secret name", r("platform:\n  linux:\n    image: x\n    registryCredentialSecretName: bad-name\n"), "machines/r.yaml", "not a valid secret name", 4},
		{"yml extension", defTree("machines/r.yml", linuxMachine), "machines/r.yml", "must use the .yaml extension", 0},
	})
	def := mustCompile(t, defTree(
		"machines/mac.yaml", "instanceShape: {vcpus: 12, memoryGb: 56}\nplatform:\n  os: macos\n",
		"machines/win.yaml", "setupCommands: [\"pwsh -File setup.ps1\"]\ninstanceShape: {vcpus: 4, memoryGb: 8}\nplatform:\n  os: windows\n",
		"machines/arm.yaml", "description: d\nfailureSessionRetentionMinutes: 10\nplatform:\n  arch: aarch64\n  linux:\n    image: ghcr.io/acme/img:1\n    registryCredentialSecretName: GHCR\n",
	))
	mac := def.Machines["mac"]
	if mac.OS != "macos" || mac.Arch != "aarch64" || mac.MacVersion != "26" || mac.VCPUs != 12 || mac.MemoryGB != 56 {
		t.Errorf("mac = %+v", mac)
	}
	if w := def.Machines["win"]; w.OS != "windows" || w.Arch != "x86_64" || len(w.SetupCommands) != 1 {
		t.Errorf("win = %+v", w)
	}
	if a := def.Machines["arm"]; a.OS != "linux" || a.Arch != "aarch64" || a.RegistryCredentialSecretName != "GHCR" || a.FailureSessionRetentionMinutes != 10 || a.Name != "arm" {
		t.Errorf("arm = %+v", a)
	}
}

func TestWebhookValidation(t *testing.T) {
	w := func(content string) map[string]string { return defTree("webhooks/w.yaml", content) }
	many := defTree()
	for i := 0; i <= MaxWebhooks; i++ {
		many[fmt.Sprintf("webhooks/w%02d.yaml", i)] = "secretName: S\n"
	}
	runErrCases(t, []errCase{
		{"missing secret", w("authMode: token\n"), "webhooks/w.yaml", "secretName is required", 0},
		{"bad auth mode", w("authMode: basic\nsecretName: S\n"), "webhooks/w.yaml", "must be one of token, url_token, signature", 1},
		{"signature requires scheme", w("authMode: signature\nsecretName: S\n"), "webhooks/w.yaml", "signatureScheme is required", 0},
		{"scheme only with signature", w("signatureScheme: github\nsecretName: S\n"), "webhooks/w.yaml", "only valid when authMode is signature", 1},
		{"bad scheme", w("authMode: signature\nsignatureScheme: twilio\nsecretName: S\n"), "webhooks/w.yaml", "must be one of github, pagerduty", 2},
		{"credential header", w("secretName: S\ndeliveryIdHeader: Authorization\n"), "webhooks/w.yaml", "credential-bearing", 2},
		{"signature header", w("secretName: S\ndeliveryIdHeader: X-Hub-Signature-256\n"), "webhooks/w.yaml", "credential-bearing", 2},
		{"long header", w("secretName: S\ndeliveryIdHeader: X-" + strings.Repeat("a", 63) + "\n"), "webhooks/w.yaml", "1-64 characters", 2},
		{"invalid header", w("secretName: S\ndeliveryIdHeader: \"X Id\"\n"), "webhooks/w.yaml", "not a valid HTTP header name", 2},
		{"enabled type", w("secretName: S\nenabled: maybe\n"), "webhooks/w.yaml", "must be true or false", 2},
		{"too many", many, "webhooks", "at most 20 webhooks", 0},
	})
	def := mustCompile(t, defTree(
		"webhooks/a.yaml", "secretName: A\n",
		"webhooks/b.yaml", "authMode: url_token\nsecretName: B\nenabled: false\ndeliveryIdHeader: x-ci-run-id\n",
		"webhooks/c.yaml", "authMode: signature\nsignatureScheme: standard_webhooks\nsecretName: C\n",
	))
	if a := def.Webhooks[0]; a.AuthMode != "token" || !a.Enabled || a.Name != "a" {
		t.Errorf("a = %+v", a)
	}
	if b := def.Webhooks[1]; b.AuthMode != "url_token" || b.Enabled || b.DeliveryIDHeader != "X-Ci-Run-Id" {
		t.Errorf("b = %+v", b)
	}
	if c := def.Webhooks[2]; c.SignatureScheme != "standard_webhooks" {
		t.Errorf("c = %+v", c)
	}
}

func TestURLTokenSecretAndHeaderHelpers(t *testing.T) {
	if !ValidURLTokenSecret("abc-DEF_1.2~x") || ValidURLTokenSecret("a/b") || ValidURLTokenSecret("") || ValidURLTokenSecret("a b") {
		t.Error("ValidURLTokenSecret")
	}
	if ok, _ := ValidDeliveryIDHeader("X-Request-Id"); !ok {
		t.Error("X-Request-Id should be valid")
	}
	if ok, _ := ValidDeliveryIDHeader("cookie"); ok {
		t.Error("cookie should be rejected")
	}
}

const scorerBase = `name: s
agents: [foreman]
labels:
  - value: pass
    score: 1
  - value: fail
    score: 0
passingScore: 1
model: claude-haiku-5-5
`

func scorerMD(fm string) string { return "---\n" + fm + "---\nJudge the run.\n" }

func TestScorerValidation(t *testing.T) {
	s := func(fm string) map[string]string { return defTree("scorers/s/scorer.md", scorerMD(fm)) }
	labels := func(n int) string {
		var b strings.Builder
		b.WriteString("labels:\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  - value: l%d\n    score: %d\n", i, i%2)
		}
		return b.String()
	}
	runErrCases(t, []errCase{
		{"missing name", s(strings.Replace(scorerBase, "name: s\n", "", 1)), "scorers/s/scorer.md", "name is required", 0},
		{"missing model", s(strings.Replace(scorerBase, "model: claude-haiku-5-5\n", "", 1)), "scorers/s/scorer.md", "model is required", 0},
		{"missing passingScore", s(strings.Replace(scorerBase, "passingScore: 1\n", "", 1)), "scorers/s/scorer.md", "passingScore is required", 0},
		{"passingScore range", s(strings.Replace(scorerBase, "passingScore: 1", "passingScore: 1.5", 1)), "scorers/s/scorer.md", "passingScore must be between 0 and 1", 0},
		{"samplingRate range", s(scorerBase + "samplingRate: 101\n"), "scorers/s/scorer.md", "samplingRate must be between 0 and 100", 11},
		{"output", s(scorerBase + "output: score\n"), "scorers/s/scorer.md", "must be one of classification", 11},
		{"no agents", s(strings.Replace(scorerBase, "agents: [foreman]", "agents: []", 1)), "scorers/s/scorer.md", "at least one agent", 0},
		{"unknown agent", s(strings.Replace(scorerBase, "agents: [foreman]", "agents: [ghost]", 1)), "scorers/s/scorer.md", `scorer agent "ghost"`, 3},
		{"agent object missing name", s(strings.Replace(scorerBase, "agents: [foreman]", "agents:\n  - includeDescendants: true", 1)), "scorers/s/scorer.md", "agents[0].name is required", 0},
		{"zero labels", s(strings.Replace(scorerBase, scorerBase[strings.Index(scorerBase, "labels:"):strings.Index(scorerBase, "passingScore")], "labels: []\n", 1)), "scorers/s/scorer.md", "1 to 20 classifications", 0},
		{"21 labels", s("name: s\nagents: [foreman]\npassingScore: 0.5\nmodel: m\n" + labels(21)), "scorers/s/scorer.md", "1 to 20 classifications (found 21)", 0},
		{"duplicate label", s(strings.Replace(scorerBase, "value: fail", "value: pass", 1)), "scorers/s/scorer.md", `label value "pass" is used more than once`, 0},
		{"label score range", s(strings.Replace(scorerBase, "score: 0\n", "score: -0.5\n", 1)), "scorers/s/scorer.md", "score must be between 0 and 1", 0},
		{"no passing label", s(strings.Replace(scorerBase, "score: 1\n", "score: 0.5\n", 1)), "scorers/s/scorer.md", "at least one label must meet passingScore", 0},
		{"no failing label", s(strings.Replace(scorerBase, "passingScore: 1", "passingScore: 0", 1)), "scorers/s/scorer.md", "at least one label must fall below passingScore", 0},
		{"empty rubric", defTree("scorers/s/scorer.md", "---\n"+scorerBase+"---\n"), "scorers/s/scorer.md", "rubric", 0},
		{"unknown machine", s(scorerBase + "machine: nope\n"), "scorers/s/scorer.md", `machine "nope" does not match`, 11},
		{"duplicate scorer names", defTree("scorers/s/scorer.md", scorerMD(scorerBase), "scorers/t/scorer.md", scorerMD(scorerBase)), "scorers/t/scorer.md", "already used by", 0},
		{"misplaced scorer", defTree("scorers/s.md", "x"), "scorers/s.md", "scorers/<slug>/scorer.md", 0},
	})

	def := mustCompile(t, defTree(
		"machines/linux.yaml", linuxMachine,
		"scorers/s/scorer.md", scorerMD(strings.Replace(scorerBase, "agents: [foreman]", "agents:\n  - name: foreman\n    includeDescendants: true", 1)+"samplingRate: 12.345\nselfImprovement: true\ndescription: d\noutput: classification\nmachine: linux\nsecrets: [X]\nmcpServers:\n  m: {managedId: q}\n"),
		"scorers/t/scorer.md", scorerMD(strings.Replace(scorerBase, "name: s", "name: t", 1)),
	))
	sc := def.Scorers[0]
	if sc.Slug != "s" || sc.SamplingRate != 12.35 || !sc.SelfImprovement || !sc.Agents[0].IncludeDescendants || sc.Rubric != "Judge the run." {
		t.Errorf("scorer = %+v", sc)
	}
	if sc.Machine != "linux" || len(sc.Secrets) != 1 || sc.Secrets[0] != "X" || sc.MCPServers["m"].ManagedID != "q" {
		t.Errorf("scorer execution settings = machine %q secrets %v mcp %v", sc.Machine, sc.Secrets, sc.MCPServers)
	}
	if def.Scorers[1].SamplingRate != 10 {
		t.Errorf("default samplingRate = %v", def.Scorers[1].SamplingRate)
	}
}

func TestRoundHundredths(t *testing.T) {
	for in, want := range map[string]float64{"12.345": 12.35, "0.005": 0.01, "0.004": 0, "100": 100, "33.333": 33.33, "2.675": 2.68, "1e1": 10} {
		if got := roundHundredths(in, 0); got != want {
			t.Errorf("roundHundredths(%s) = %v, want %v", in, got, want)
		}
	}
}

const suiteBase = `name: Suite
agent: foreman
tasks: [t1]
`

const taskBase = `title: Task
prompt: Do it.
successCriteria: It is done.
`

func TestBenchmarkValidation(t *testing.T) {
	b := func(suite string, kv ...string) map[string]string {
		f := defTree(append([]string{"benchmarks/s/suite.yaml", suite, "benchmarks/s/tasks/t1.yaml", taskBase}, kv...)...)
		return f
	}
	sha := strings.Repeat("a", 40)
	cfgs := func(n int) string {
		var s strings.Builder
		s.WriteString("configurations:\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&s, "  - name: c%d\n", i)
		}
		return s.String()
	}
	runErrCases(t, []errCase{
		{"missing name", b("agent: foreman\ntasks: [t1]\n"), "benchmarks/s/suite.yaml", "name is required", 0},
		{"missing agent", b("name: S\ntasks: [t1]\n"), "benchmarks/s/suite.yaml", "agent is required", 0},
		{"unknown agent", b("name: S\nagent: ghost\ntasks: [t1]\n"), "benchmarks/s/suite.yaml", `agent "ghost" does not match`, 2},
		{"zero configurations", b(suiteBase + "configurations: []\n"), "benchmarks/s/suite.yaml", "1 to 6 configurations", 0},
		{"seven configurations", b(suiteBase + cfgs(7)), "benchmarks/s/suite.yaml", "1 to 6 configurations (found 7)", 0},
		{"duplicate configuration", b(suiteBase + "configurations:\n  - name: a\n  - name: a\n"), "benchmarks/s/suite.yaml", `configuration name "a" is used more than once`, 0},
		{"pin unknown agent", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - agent: ghost\n        model: m\n"), "benchmarks/s/suite.yaml", `agent "ghost" does not match`, 0},
		{"pin twice", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - {agent: foreman, model: m}\n      - {agent: foreman, model: n}\n"), "benchmarks/s/suite.yaml", "listed more than once in this configuration", 0},
		{"pin neither", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - agent: foreman\n"), "benchmarks/s/suite.yaml", "exactly one of model or harness", 0},
		{"pin both", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - {agent: foreman, model: m, harness: {type: codex, model: x}}\n"), "benchmarks/s/suite.yaml", "exactly one of model or harness", 0},
		{"pin harness needs model", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - {agent: foreman, harness: {type: codex}}\n"), "benchmarks/s/suite.yaml", "harness.model is required", 0},
		{"pin model selects no harness", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - {agent: foreman, model: auto}\n"), "benchmarks/s/suite.yaml", "selects no harness", 0},
		{"pin unknown key", b(suiteBase + "configurations:\n  - name: a\n    agents:\n      - {agent: foreman, model: m, machine: x}\n"), "benchmarks/s/suite.yaml", `unknown key "configurations[0].agents[0].machine"`, 0},
		{"task listed without file", b("name: S\nagent: foreman\ntasks: [t1, t2]\n"), "benchmarks/s/suite.yaml", `task "t2" has no file`, 3},
		{"task file not listed", b(suiteBase, "benchmarks/s/tasks/t9.yaml", taskBase), "benchmarks/s/tasks/t9.yaml", "not listed", 0},
		{"duplicate task", b("name: S\nagent: foreman\ntasks: [t1, t1]\n"), "benchmarks/s/suite.yaml", "more than once", 0},
		{"task missing title", b(suiteBase, "benchmarks/s/tasks/t1.yaml", "prompt: p\nsuccessCriteria: c\n"), "benchmarks/s/tasks/t1.yaml", "title is required", 0},
		{"task missing criteria", b(suiteBase, "benchmarks/s/tasks/t1.yaml", "title: t\nprompt: p\n"), "benchmarks/s/tasks/t1.yaml", "successCriteria is required", 0},
		{"task unknown key", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"difficulty: hard\n"), "benchmarks/s/tasks/t1.yaml", `unknown key "difficulty"`, 4},
		{"ref short sha", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"startingRepoRefs:\n  - github.com:acme/api@abc123\n"), "benchmarks/s/tasks/t1.yaml", "full 40-character commit SHA", 5},
		{"ref branch", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"startingRepoRefs:\n  - {codeForge: GITHUB, owner: acme, repo: api, ref: main}\n"), "benchmarks/s/tasks/t1.yaml", "not a branch or tag", 5},
		{"ref missing field", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"startingRepoRefs:\n  - {codeForge: GITHUB, owner: acme, ref: "+sha+"}\n"), "benchmarks/s/tasks/t1.yaml", "repo is required", 0},
		{"ref bad host", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"startingRepoRefs:\n  - bitbucket.org:acme/api@"+sha+"\n"), "benchmarks/s/tasks/t1.yaml", "must have the form", 5},
		{"ref azure forge", b(suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"startingRepoRefs:\n  - {codeForge: AZURE_DEVOPS, owner: a, repo: b, ref: "+sha+"}\n"), "benchmarks/s/tasks/t1.yaml", "must be one of GITHUB, GITLAB", 5},
		{"tasks without suite", defTree("benchmarks/x/tasks/t.yaml", taskBase), "benchmarks/x", "no suite.yaml", 0},
		{"duplicate suite names", defTree("benchmarks/a/suite.yaml", "name: Same\nagent: foreman\n", "benchmarks/b/suite.yaml", "name: Same\nagent: foreman\n"), "benchmarks/b/suite.yaml", "already used by", 0},
	})

	def := mustCompile(t, b(suiteBase+"description: d\nconfigurations:\n  - name: base\n    agents:\n      - {agent: foreman, model: claude-sonnet-5-5}\n  - name: cc\n    agents:\n      - agent: foreman\n        harness: {type: claude-code, model: m, auth: {source: managedSecret, secretName: K}}\n",
		"benchmarks/s/tasks/t1.yaml", taskBase+"sourceRunId: run_1\ntags: [a, b]\nlinearSeed: {issues: [{title: x}]}\nstartingRepoRefs:\n  - gitlab.com:acme/platform/api@"+sha+"\n  - {codeForge: GITHUB, owner: acme, repo: api, ref: "+sha+"}\n"))
	s := def.Benchmarks[0]
	if s.Slug != "s" || s.Name != "Suite" || len(s.Configurations) != 2 || s.Configurations[1].Agents[0].Harness.Type != core.HarnessClaude {
		t.Errorf("suite = %+v", s)
	}
	task := s.Tasks[0]
	if task.Slug != "t1" || len(task.Tags) != 2 || task.SourceRunID != "run_1" || task.TrackerSeed == nil {
		t.Errorf("task = %+v", task)
	}
	if r := task.StartingRepoRefs[0]; r.CodeForge != "GITLAB" || r.Owner != "acme/platform" || r.Repo != "api" || r.Ref != sha {
		t.Errorf("gitlab ref = %+v", r)
	}
	// A suite without tasks can be saved.
	mustCompile(t, defTree("benchmarks/empty/suite.yaml", "name: Empty\nagent: foreman\n"))
}

func TestParseRepoAtCommit(t *testing.T) {
	sha := strings.Repeat("0", 40)
	tests := map[string]bool{
		"github.com:acme/api@" + sha:                                      true,
		"gitlab.com:acme/sub/api@" + sha:                                  true,
		"github.com:acme/sub/api@" + sha:                                  false,
		"github.com:acme@" + sha:                                          false,
		"github.com:acme/api@" + sha[:39]:                                 false,
		"github.com:acme/api@" + strings.ToUpper(strings.Repeat("a", 40)): false,
		"github.com:acme/api":                                             false,
		"example.com:acme/api@" + sha:                                     false,
	}
	for in, want := range tests {
		if _, ok := ParseRepoAtCommit(in); ok != want {
			t.Errorf("ParseRepoAtCommit(%q) = %v, want %v", in, ok, want)
		}
	}
}

func TestSkills(t *testing.T) {
	skill := func(name, desc string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\nlicense: MIT\n---\n# Steps\n"
	}
	runErrCases(t, []errCase{
		{"missing SKILL.md", defTree("skills/s/notes.md", "x"), "skills/s", "has no SKILL.md", 0},
		{"missing description", defTree("skills/s/SKILL.md", "---\nname: s\n---\nx\n"), "skills/s/SKILL.md", "description is required", 0},
		{"missing frontmatter", defTree("skills/s/SKILL.md", "# Just text\n"), "skills/s/SKILL.md", "must start with YAML frontmatter", 1},
		{"name mismatch", defTree("skills/s/SKILL.md", skill("other", "d")), "skills/s/SKILL.md", "must match its directory name", 2},
		{"bad name", defTree("skills/Bad/SKILL.md", skill("Bad", "d")), "skills/Bad/SKILL.md", "lowercase letters", 2},
		{"skills for unknown agent", defTree("agents/ghost/skills/s/SKILL.md", skill("s", "d")), "agents/ghost/skills/s", "has no agents/ghost/agent.md", 0},
	})
	def := mustCompile(t, defTree(
		"skills/conventions/SKILL.md", skill("conventions", "Shared conventions"),
		"skills/conventions/reference/style.md", "style",
		"agents/foreman/skills/triage/SKILL.md", skill("triage", "Runbook"),
		"agents/foreman/skills/triage/scripts/run.sh", "echo hi",
	))
	if len(def.Skills) != 1 || def.Skills[0].Scope != "factory" || def.Skills[0].Files["reference/style.md"] != "style" || def.Skills[0].Path != "skills/conventions/SKILL.md" {
		t.Errorf("factory skills = %+v", def.Skills)
	}
	f := agentByName(t, def, "foreman")
	if len(f.Skills) != 1 || f.Skills[0].Scope != "foreman" || f.Skills[0].Description != "Runbook" || f.Skills[0].Files["scripts/run.sh"] != "echo hi" || !strings.Contains(f.Skills[0].Content, "# Steps") {
		t.Errorf("agent skills = %+v", f.Skills)
	}
}

func TestPathsAndUnknownFiles(t *testing.T) {
	files := defTree("README.md", "docs", "./agents/x/agent.md", agentMD("description: d\n", ""), "docs/guide.md", "x", "agents/x/notes.txt", "n")
	def := mustCompile(t, files)
	agentByName(t, def, "x")
	runErrCases(t, []errCase{
		{"duplicate normalized path", defTree("./factory.yaml", baseFactory), "factory.yaml", "listed more than once", 0},
		{"factory.yml", defTree("factory.yml", baseFactory), "factory.yml", "must be named factory.yaml", 0},
	})
	if NormalizePath("../../etc/passwd") != "etc/passwd" || NormalizePath("a\\b") != "a/b" || NormalizePath("/") != "" {
		t.Error("NormalizePath")
	}
}

func TestErrorsSortedAndDefinitionNil(t *testing.T) {
	_, errs := Compile(defTree("machines/b.yaml", "x: 1\n", "machines/a.yaml", "y: 1\n", "agents/foreman/agent.md", agentMD("bogus: 1\n", "")))
	if len(errs) < 3 {
		t.Fatalf("errs = %v", errs)
	}
	for i := 1; i < len(errs); i++ {
		if errs[i-1].File > errs[i].File || (errs[i-1].File == errs[i].File && errs[i-1].Line > errs[i].Line) {
			t.Errorf("errors not sorted: %v", errs)
		}
	}
	if errs[0].Error() == "" {
		t.Error("ValidationError.Error empty")
	}
}

func TestTriggerCatalog(t *testing.T) {
	providers := TriggerProviders()
	want := []string{"azure_devops", "github", "gitlab", "jira", "linear", "microsoft_teams", "schedule", "slack", "webhook"}
	if !reflect.DeepEqual(providers, want) {
		t.Errorf("providers = %v", providers)
	}
	ev := TriggerEvents("linear")
	found := false
	for _, e := range ev {
		if e.Event == "comment_created" {
			found = e.Aliases["issues"] == "issue_ids"
		}
		if e.Event == "issue_labeled" {
			if _, ok := e.Aliases["issues"]; ok {
				t.Error("issues alias must not apply to issue_labeled")
			}
		}
	}
	if !found {
		t.Error("linear comment_created aliases")
	}
	if len(TriggerEvents("teams")) != 2 || TriggerEvents("nope") != nil {
		t.Error("TriggerEvents")
	}
	for _, e := range TriggerEvents("webhook") {
		if e.FilterRule != "required" {
			t.Errorf("webhook filter rule = %s", e.FilterRule)
		}
	}
	if err := ValidateCron("*/15 * * * *"); err != nil {
		t.Error(err)
	}
	if err := ValidateCron("@weekly"); err != nil {
		t.Error(err)
	}
}

func TestAgentSnapshotSetting(t *testing.T) {
	def := mustCompile(t, defTree(
		"agents/off/agent.md", agentMD("snapshot: false\n", ""),
		"agents/on/agent.md", agentMD("snapshot: true\n", ""),
		"agents/unset/agent.md", agentMD("description: d\n", ""),
	))
	if s := agentByName(t, def, "off").Snapshot; s == nil || *s {
		t.Errorf("off: Snapshot = %v, want false", s)
	}
	if s := agentByName(t, def, "on").Snapshot; s == nil || !*s {
		t.Errorf("on: Snapshot = %v, want true", s)
	}
	if s := agentByName(t, def, "unset").Snapshot; s != nil {
		t.Errorf("unset: Snapshot = %v, want nil", *s)
	}
}

func TestMaxConcurrentRuns(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryWith("maxConcurrentRuns: 3\n")))
	if def.MaxConcurrentRuns != 3 {
		t.Errorf("MaxConcurrentRuns = %d, want 3", def.MaxConcurrentRuns)
	}
	if mustCompile(t, defTree()).MaxConcurrentRuns != 0 {
		t.Error("MaxConcurrentRuns must be 0 when the key is omitted")
	}
	runErrCases(t, []errCase{
		{"zero", defTree("factory.yaml", factoryWith("maxConcurrentRuns: 0\n")), "factory.yaml", "maxConcurrentRuns must be 1 or more", 0},
		{"text", defTree("factory.yaml", factoryWith("maxConcurrentRuns: many\n")), "factory.yaml", "maxConcurrentRuns", 0},
	})
}

func TestApproversAndRestrictFollowups(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryWith("approvers: [ada@example.test, release-bot]\nrestrictFollowupsToStarter: true\n")))
	if len(def.Approvers) != 2 || def.Approvers[0] != "ada@example.test" || def.Approvers[1] != "release-bot" {
		t.Errorf("Approvers = %v", def.Approvers)
	}
	if !def.RestrictFollowupsToStarter {
		t.Error("RestrictFollowupsToStarter = false, want true")
	}
	plain := mustCompile(t, defTree())
	if plain.Approvers != nil || plain.RestrictFollowupsToStarter {
		t.Errorf("defaults: approvers %v, restrict %v", plain.Approvers, plain.RestrictFollowupsToStarter)
	}
	runErrCases(t, []errCase{
		{"empty approvers", defTree("factory.yaml", factoryWith("approvers: []\n")), "factory.yaml", "approvers must list at least one", 0},
		{"duplicate approver", defTree("factory.yaml", factoryWith("approvers: [a@example.test, a@example.test]\n")), "factory.yaml", "more than once", 0},
		{"approvers not a list", defTree("factory.yaml", factoryWith("approvers: ada\n")), "factory.yaml", "approvers must be a list", 0},
		{"restrict not a bool", defTree("factory.yaml", factoryWith("restrictFollowupsToStarter: maybe\n")), "factory.yaml", "restrictFollowupsToStarter", 0},
	})
}

// Jira and Teams filters can name projects, statuses, channels and users.
// A Teams trigger needs a channel, and the alias channels is enough.
func TestJiraAndTeamsNameAliases(t *testing.T) {
	files := defTree("automations/a/automation.md", automationMD(`triggers:
  - provider: teams
    event: app_mention
    filter:
      team_ids: ["0f5c2a1e-8b4d-4c3a-9e2f-7a6b5c4d3e21"]
      channels: ["#incidents "]
      users: [Alice Adams]
  - provider: jira
    event: status_changed
    filter:
      projects: [Website]
      statuses: [Done, "In Review"]
`))
	files["factory.yaml"] = factoryWith("integrations:\n  - type: microsoft-teams\n  - type: jira\n")
	def, errs := Compile(files)
	if def == nil {
		t.Fatalf("errors: %+v", errs)
	}
	var tm, jr map[string]any
	for _, tr := range def.Automations[0].Triggers {
		var f map[string]any
		_ = json.Unmarshal(tr.Filter, &f)
		if tr.Provider == "jira" {
			jr = f
		} else {
			tm = f
		}
	}
	if fmt.Sprint(tm["channels"]) != "[#incidents]" || fmt.Sprint(tm["users"]) != "[Alice Adams]" || tm["channel_ids"] != nil {
		t.Errorf("teams filter = %v", tm)
	}
	if fmt.Sprint(jr["projects"]) != "[Website]" || fmt.Sprint(jr["statuses"]) != "[Done In Review]" {
		t.Errorf("jira filter = %v", jr)
	}
}
