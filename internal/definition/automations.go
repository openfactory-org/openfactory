package definition

import (
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

func (c *compiler) compileAutomations() {
	env := triggerEnv{integrationsSet: c.def.IntegrationsSet, integrations: map[string]bool{}, webhooks: map[string]bool{}}
	for _, i := range c.def.Integrations {
		env.integrations[i.Type] = true
	}
	for name := range c.t.webhooks {
		env.webhooks[name] = true
	}
	for _, name := range sortedNames(c.t.automations) {
		p := c.t.automations[name]
		if a, ok := c.parseAutomation(p, name, c.t.files[p], env); ok {
			c.def.Automations = append(c.def.Automations, a)
		}
	}
}

func (c *compiler) parseAutomation(file, name, content string, env triggerEnv) (core.Automation, bool) {
	before := len(c.d.errs)
	a := core.Automation{Name: name, Enabled: true}
	dc, m, body, ok := parseMarkdown(c.d, file, content, true)
	if !ok || m == nil {
		return a, false
	}
	defer m.finish()
	a.Prompt = strings.TrimSpace(body)
	if a.Prompt == "" {
		dc.errf(nil, "", "the automation's Markdown body (its starting prompt) must not be empty")
	}
	if v := m.get("enabled"); v != nil && !isNull(v) {
		if b, ok := dc.boolean(v, "enabled"); ok {
			a.Enabled = b
		}
	}
	if v := m.get("displayName"); v != nil && !isNull(v) {
		a.DisplayName, _ = dc.str(v, "displayName")
	}
	if v := m.get("runAs"); v != nil && !isNull(v) {
		// The principal of the runs that the automation starts. The
		// preflight checks that it exists.
		if s, ok := dc.nonEmptyStr(v, "runAs"); ok {
			if _, _, err := core.ParseRunAs(s); err != nil {
				dc.errf(v, "runAs", "runAs must be factory, user:<email> or serviceAccount:<name>")
			} else if s != core.RunAsFactory {
				a.RunAs = s
			}
		}
	}
	a.Agent = c.foreman
	if v := m.get("agent"); v != nil && !isNull(v) {
		if s, ok := dc.nonEmptyStr(v, "agent"); ok {
			a.Agent = s
			if _, exists := c.t.agents[s]; !exists {
				dc.errf(v, "agent", "agent %q does not match any agents/<name>/agent.md", s)
			}
		}
	}

	if tv := m.require("triggers"); tv != nil {
		items := dc.seq(tv, "triggers")
		if isSeq(tv) && len(items) == 0 {
			dc.errf(tv, "triggers", "triggers must list at least one trigger")
		}
		unnamed := 0
		names := map[string]bool{}
		for i, it := range items {
			p := indexPath("triggers", i)
			t, good := parseTrigger(dc, it, p, env)
			if !good {
				continue
			}
			if t.Schedule != nil {
				if t.Schedule.Name == "" {
					unnamed++
					if unnamed > 1 {
						dc.errf(it, p+".schedule.name", "at most one schedule trigger in an automation can omit schedule.name")
					}
				} else if names[t.Schedule.Name] {
					dc.errf(it, p+".schedule.name", "schedule name %q is used more than once in this automation", t.Schedule.Name)
				} else {
					names[t.Schedule.Name] = true
				}
			}
			a.Triggers = append(a.Triggers, t)
		}
	}

	e := dc.parseExec(m, false, true)
	a.Overrides = c.automationOverrides(dc, a.Agent, e)
	return a, len(c.d.errs) == before
}

// automationOverrides resolves an automation's execution settings against the
// target agent. Harness, Secrets and MCPServers hold complete effective values
// (factory-wide secrets and MCP servers included) so the engine can apply them
// by replacement; Model is left empty.
func (c *compiler) automationOverrides(dc *doc, agentName string, e execSettings) *core.AgentOverrides {
	set := e.Model != nil || e.Harness != nil || e.Machine != nil || e.EnvSet || e.SecretsSet || e.MCPSet || e.WorkerSet || e.Idle != nil
	if !set {
		return nil
	}
	o := &core.AgentOverrides{}
	agent := c.agents[agentName]
	var base core.AgentConfig
	if agent != nil {
		base = *agent
	}
	worker := base.WorkerHost
	if e.WorkerSet {
		w := e.Worker
		o.WorkerHost = &w
		worker = w
	}
	if e.Model != nil || e.Harness != nil {
		if agent != nil && c.f.harnessOK {
			h := applyHarness(base.Harness, e.Model, e.Harness)
			o.Harness = &h
			c.checkEffectiveHarness(dc, "", h, e.Harness, worker, true)
		}
	} else if agent != nil && e.WorkerSet {
		c.checkEffectiveHarness(dc, "", base.Harness, nil, worker, false)
	}
	if e.Machine != nil {
		c.machineRef(dc, *e.Machine, "machine", dc.line(e.machineNode))
		o.Machine = *e.Machine
	}
	if e.EnvSet {
		env := e.Env
		o.EnvironmentID = &env
	}
	if e.SecretsSet {
		o.Secrets = dedupe(e.Secrets, c.def.Secrets)
	}
	if e.MCPSet {
		c.checkMCPConflicts(dc, "mcpServers", e.mcpNode, e.MCP)
		o.MCPServers = mergeMCP(e.MCP, c.f.mcp)
		if o.MCPServers == nil {
			o.MCPServers = map[string]core.MCPServer{}
		}
	}
	if e.Idle != nil {
		i := *e.Idle
		o.IdleTimeoutMinutes = &i
	}
	return o
}
