package definition

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/networkpolicy"
)

var agentTypes = []string{"CUSTOM", "FOREMAN", "MAIN", "TRIAGE", "SPEC", "IMPLEMENT", "REVIEW", "VERIFY"}

type agentSpec struct {
	name, file   string
	c            *doc
	node         *yaml.Node
	description  string
	typ          core.AgentType
	typeNode     *yaml.Node
	credential   string
	snapshot     *bool
	spawnSet     bool
	spawn        []string
	spawnNode    *yaml.Node
	np           *core.NetworkPolicy
	npNode       *yaml.Node
	exec         execSettings
	instructions string
}

// parseMarkdown splits a Markdown resource and parses its frontmatter. It
// returns the frontmatter mapping (nil when absent and not required) and body.
func parseMarkdown(d *diag, file, content string, requireFM bool) (*doc, *mapping, string, bool) {
	fm, body, has, off, err := splitFrontmatter(content)
	c := &doc{d: d, file: file, offset: off}
	if err != nil {
		d.add(file, 1, "", "%v", err)
		return c, nil, "", false
	}
	if !has {
		if requireFM {
			d.add(file, 1, "", "file must start with YAML frontmatter delimited by --- lines")
			return c, nil, body, false
		}
		return c, nil, body, true
	}
	root := c.parse(fm)
	if root == nil {
		return c, nil, body, false
	}
	m := c.mapping(root, "")
	if m == nil {
		return c, nil, body, false
	}
	return c, m, body, true
}

func parseAgent(d *diag, file, name, content string) *agentSpec {
	c, m, body, ok := parseMarkdown(d, file, content, false)
	if !ok {
		return nil
	}
	a := &agentSpec{name: name, file: file, c: c, typ: core.AgentCustom, instructions: strings.TrimSpace(body)}
	if m == nil {
		a.exec = execSettings{c: c}
		return a
	}
	defer m.finish()
	a.node = m.node
	if v := m.get("description"); v != nil && !isNull(v) {
		a.description, _ = c.str(v, "description")
	}
	if v := m.get("agentType"); v != nil && !isNull(v) {
		if s, ok := c.enum(v, "agentType", agentTypes...); ok {
			if s == "MAIN" {
				s = "FOREMAN"
			}
			a.typ, a.typeNode = core.AgentType(s), v
		}
	}
	if v := m.get("credentialStrategy"); v != nil && !isNull(v) {
		a.credential, _ = c.enum(v, "credentialStrategy", "EXECUTOR", "CREATOR")
	}
	if v := m.get("snapshot"); v != nil && !isNull(v) {
		if b, ok := c.boolean(v, "snapshot"); ok {
			a.snapshot = &b
		}
	}
	if m.has("spawnableBy") {
		v := m.get("spawnableBy")
		if !isNull(v) {
			if list, ok := c.uniqueStrList(v, "spawnableBy"); ok {
				a.spawnSet, a.spawn, a.spawnNode = true, list, v
			}
		}
	}
	if m.has("networkPolicy") {
		a.npNode = m.get("networkPolicy")
		a.np, _ = c.parseNetworkPolicy(a.npNode, "networkPolicy")
	}
	a.exec = c.parseExec(m, false, true)
	return a
}

func (c *compiler) compileAgents() {
	t := c.t
	if len(t.agents) == 0 {
		c.d.add("agents", 0, "", "a definition must include at least one agents/<name>/agent.md")
		return
	}
	var specs []*agentSpec
	for _, name := range sortedNames(t.agents) {
		p := t.agents[name]
		if a := parseAgent(c.d, p, name, t.files[p]); a != nil {
			specs = append(specs, a)
		}
	}
	var foremen []*agentSpec
	for _, a := range specs {
		if a.typ == core.AgentForeman {
			foremen = append(foremen, a)
		}
	}
	switch {
	case len(foremen) == 0 && len(specs) == len(t.agents):
		c.d.add("agents", 0, "agentType", "exactly one agent must have agentType FOREMAN (alias MAIN); none does")
	case len(foremen) > 1:
		var names []string
		for _, a := range foremen {
			names = append(names, a.name)
		}
		for _, a := range foremen[1:] {
			a.c.errf(a.typeNode, "agentType", "exactly one agent can be the foreman; found %s", strings.Join(names, ", "))
		}
	}
	if len(foremen) >= 1 {
		c.foreman = foremen[0].name
	}

	for _, a := range specs {
		cfg := c.resolveAgent(a)
		c.agents[a.name] = cfg
	}
	// spawnableBy references need every agent name.
	for _, a := range specs {
		if !a.spawnSet {
			continue
		}
		items := resolve(a.spawnNode).Content
		for i, n := range a.spawn {
			if _, ok := t.agents[n]; !ok {
				a.c.errf(items[i], indexPath("spawnableBy", i), "spawnableBy names unknown agent %q", n)
			}
		}
	}
	names := sortedNames(c.agents)
	for _, n := range names {
		c.def.Agents = append(c.def.Agents, *c.agents[n])
	}
}

func (c *compiler) resolveAgent(a *agentSpec) *core.AgentConfig {
	f, e, dc := c.f, a.exec, a.c
	cfg := &core.AgentConfig{
		Name:               a.name,
		Description:        a.description,
		Type:               a.typ,
		Instructions:       a.instructions,
		CredentialStrategy: f.def.CredentialStrategy,
		IdleTimeoutMinutes: 0,
	}
	if a.credential != "" {
		cfg.CredentialStrategy = a.credential
	}
	if f.harnessOK {
		cfg.Harness = applyHarness(f.defaultHarness, e.Model, e.Harness)
	}
	cfg.WorkerHost = f.defaults.Worker
	if e.WorkerSet {
		cfg.WorkerHost = e.Worker
	}
	c.checkEffectiveHarness(dc, "", cfg.Harness, e.Harness, cfg.WorkerHost, e.Model != nil || e.Harness != nil)

	machine := f.defaults.Machine
	machineDoc, machineNode, machineField := f.c, f.defaults.machineNode, "agentDefaults.machine"
	if e.Machine != nil {
		machine, machineDoc, machineNode, machineField = e.Machine, dc, e.machineNode, "machine"
	}
	if machine != nil {
		if spec := c.machineRef(machineDoc, *machine, machineField, machineDoc.line(machineNode)); spec != nil {
			cfg.Machine = spec
		}
	}
	cfg.EnvironmentID = f.defaults.Env
	if e.EnvSet {
		cfg.EnvironmentID = e.Env
	}
	base := f.defaults.Secrets
	if e.SecretsSet {
		base = e.Secrets
	}
	cfg.Secrets = dedupe(base, f.def.Secrets)
	if len(cfg.Secrets) == 0 {
		cfg.Secrets = nil
	}
	mcpBase := f.defaults.MCP
	if e.MCPSet {
		mcpBase = e.MCP
		c.checkMCPConflicts(dc, "mcpServers", e.mcpNode, e.MCP)
	}
	cfg.MCPServers = mergeMCP(mcpBase, f.mcp)
	if np, err := networkpolicy.Merge(f.networkPolicy, a.np); err == nil {
		cfg.NetworkPolicy = np
	} // invalid policies were reported while parsing
	if cfg.NetworkPolicy.AllowedHosts == nil {
		cfg.NetworkPolicy.AllowedHosts = []string{}
	}
	if cfg.NetworkPolicy.DeniedHosts == nil {
		cfg.NetworkPolicy.DeniedHosts = []string{}
	}
	if e.Idle != nil {
		cfg.IdleTimeoutMinutes = *e.Idle
		cfg.IdleTimeoutSet = true
	}
	if a.snapshot != nil {
		b := *a.snapshot
		cfg.Snapshot = &b
	}
	if a.spawnSet {
		cfg.SpawnableBy = append([]string{}, a.spawn...)
	}
	cfg.Skills = c.agentSkills(a.name)
	return cfg
}

// checkEffectiveHarness validates harness rules that depend on the resolved
// configuration: workerEnvironment auth requires a worker host.
func (c *compiler) checkEffectiveHarness(dc *doc, prefix string, h core.HarnessConfig, ov *harnessOverride, workerHost string, overridden bool) {
	if !overridden && ov == nil {
		// Inherited unchanged from agentDefaults; only the worker host can change.
		if h.Auth != nil && h.Auth.Source == "workerEnvironment" && workerHost == "" {
			dc.errf(nil, prefix+"workerHost", "the inherited harness uses auth.source workerEnvironment, which requires a self-hosted workerHost; this %s clears it", kindOf(prefix))
		}
		return
	}
	node := (*yaml.Node)(nil)
	if ov != nil {
		node = ov.node
	}
	if h.Auth != nil && h.Auth.Source == "workerEnvironment" && workerHost == "" {
		if ov != nil && ov.authNode != nil {
			node = ov.authNode
		}
		dc.errf(node, prefix+"harness.auth.source", "auth.source workerEnvironment requires a self-hosted workerHost")
	}
}

func kindOf(prefix string) string {
	if prefix == "" {
		return "file"
	}
	return strings.TrimSuffix(prefix, ".")
}

func (c *compiler) checkMCPConflicts(dc *doc, field string, node *yaml.Node, m map[string]core.MCPServer) {
	for name := range m {
		if _, dup := c.f.mcp[name]; dup {
			dc.errf(node, field+"."+name, "MCP server %q is already granted factory-wide by factory.yaml mcpServers", name)
		}
	}
}

func mergeMCP(base, factory map[string]core.MCPServer) map[string]core.MCPServer {
	if len(base) == 0 && len(factory) == 0 {
		return nil
	}
	out := copyMCP(base)
	if out == nil {
		out = map[string]core.MCPServer{}
	}
	for k, v := range factory {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}
