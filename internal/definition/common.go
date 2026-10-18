package definition

import (
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/networkpolicy"
)

// SchemaVersion is the current definition schema version.
const SchemaVersion = "v1"

// ReservedMCPServerName is injected into every run (the orchestration MCP
// server) and cannot be declared by a definition.
const ReservedMCPServerName = "factory"

var (
	resourceNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	secretNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	aliasRe         = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,60}$`)
	reasoningRe     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	mcpServerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	emailRe         = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// normalizeHarnessType maps accepted harness identifiers to canonical ones.
func normalizeHarnessType(s string) (core.HarnessType, bool) {
	switch s {
	case "claude", "claude-code":
		return core.HarnessClaude, true
	case "codex":
		return core.HarnessCodex, true
	}
	return "", false
}

// harnessOverride is a parsed harness object. For agentDefaults and benchmark
// pins it is complete; for agents and automations it is a sparse override.
type harnessOverride struct {
	c         *doc
	node      *yaml.Node
	Type      *core.HarnessType
	Model     *string
	Reasoning *string
	AuthSet   bool
	Auth      *core.HarnessAuth // nil with AuthSet means "clear inherited auth"
	authNode  *yaml.Node
	path      string
}

func (c *doc) parseHarness(n *yaml.Node, path string, full bool) *harnessOverride {
	m := c.mapping(n, path)
	if m == nil {
		return nil
	}
	defer m.finish()
	h := &harnessOverride{c: c, node: m.node, path: path}
	if full {
		m.require("type")
		m.require("model")
	}
	if v := m.get("type"); v != nil && !isNull(v) {
		if s, ok := c.str(v, m.field("type")); ok {
			if t, ok := normalizeHarnessType(s); ok {
				h.Type = &t
			} else {
				c.errf(v, m.field("type"), "%s must be one of claude, codex, claude-code (got %q)", m.field("type"), s)
			}
		}
	}
	if v := m.get("model"); v != nil && !isNull(v) {
		if s, ok := c.nonEmptyStr(v, m.field("model")); ok {
			h.Model = &s
		}
	}
	if v := m.get("reasoningLevel"); v != nil && !isNull(v) {
		if s, ok := c.str(v, m.field("reasoningLevel")); ok {
			if !reasoningRe.MatchString(s) {
				c.errf(v, m.field("reasoningLevel"), "%s %q is not a valid reasoning level (for example low, medium, high)", m.field("reasoningLevel"), s)
			} else {
				h.Reasoning = &s
			}
		}
	}
	if m.has("auth") {
		v := m.get("auth")
		h.AuthSet = true
		h.authNode = v
		if !isNull(v) {
			h.Auth = c.parseAuth(v, m.field("auth"))
			if h.Auth == nil {
				h.AuthSet = false // invalid; error already reported
			}
		}
	}
	return h
}

func (c *doc) parseAuth(n *yaml.Node, path string) *core.HarnessAuth {
	m := c.mapping(n, path)
	if m == nil {
		return nil
	}
	defer m.finish()
	src, ok := c.enum(m.require("source"), m.field("source"), "managedSecret", "workerEnvironment")
	if !ok {
		m.get("secretName")
		return nil
	}
	a := &core.HarnessAuth{Source: src}
	sn := m.get("secretName")
	switch src {
	case "managedSecret":
		if sn == nil || isNull(sn) {
			c.errf(m.node, m.field("secretName"), "%s is required when auth.source is managedSecret", m.field("secretName"))
			return nil
		}
		s, ok := c.secretName(sn, m.field("secretName"))
		if !ok {
			return nil
		}
		a.SecretName = s
	case "workerEnvironment":
		if sn != nil && !isNull(sn) {
			c.errf(sn, m.field("secretName"), "%s is not allowed when auth.source is workerEnvironment", m.field("secretName"))
			return nil
		}
	}
	return a
}

func (c *doc) secretName(n *yaml.Node, path string) (string, bool) {
	s, ok := c.nonEmptyStr(n, path)
	if !ok {
		return "", false
	}
	if !secretNameRe.MatchString(s) {
		c.errf(n, path, "%s %q is not a valid secret name (letters, digits and _, not starting with a digit)", path, s)
		return "", false
	}
	return s, true
}

func (c *doc) secretList(n *yaml.Node, path string) ([]string, bool) {
	list, ok := c.uniqueStrList(n, path)
	if !ok {
		return list, false
	}
	items := resolve(n).Content
	for i, s := range list {
		if !secretNameRe.MatchString(s) {
			c.errf(items[i], indexPath(path, i), "%q is not a valid secret name (letters, digits and _, not starting with a digit)", s)
			ok = false
		}
	}
	return list, ok
}

// parseMCPServers decodes an mcpServers map. Each entry selects exactly one
// transport: managedId, command (with optional args/env) or url (with optional headers).
func (c *doc) parseMCPServers(n *yaml.Node, path string) (map[string]core.MCPServer, bool) {
	m := c.mapping(n, path)
	if m == nil {
		return nil, false
	}
	out := map[string]core.MCPServer{}
	ok := true
	for _, name := range m.order {
		v := m.get(name)
		f := m.field(name)
		if !mcpServerNameRe.MatchString(name) {
			c.errf(m.keyNode(name), f, "MCP server name %q must be 1-64 letters, digits, '.', '_' or '-'", name)
			ok = false
			continue
		}
		if name == ReservedMCPServerName {
			c.errf(m.keyNode(name), f, "MCP server name %q is reserved for the orchestration server", name)
			ok = false
			continue
		}
		srv, good := c.parseMCPServer(v, f)
		if !good {
			ok = false
			continue
		}
		out[name] = srv
	}
	return out, ok
}

func (c *doc) parseMCPServer(n *yaml.Node, path string) (core.MCPServer, bool) {
	var s core.MCPServer
	m := c.mapping(n, path)
	if m == nil {
		return s, false
	}
	defer m.finish()
	ok := true
	transports := 0
	for _, k := range []string{"managedId", "command", "url"} {
		if m.present(k) {
			transports++
		}
	}
	if transports != 1 {
		c.errf(m.node, path, "%s must set exactly one of managedId, command or url", path)
		ok = false
	}
	if v := m.get("managedId"); v != nil && !isNull(v) {
		s.ManagedID, _ = c.nonEmptyStr(v, m.field("managedId"))
		if strings.HasPrefix(s.ManagedID, core.BuiltinMCPPrefix) {
			if _, known := core.BuiltinMCPProvider(s.ManagedID); !known {
				c.errf(v, m.field("managedId"), "%s: unknown built-in MCP server %q (known: builtin/%s)", m.field("managedId"), s.ManagedID, strings.Join(core.BuiltinMCPProviders, ", builtin/"))
				ok = false
			}
		}
	}
	if v := m.get("allowWrite"); v != nil && !isNull(v) {
		if b, good := c.boolean(v, m.field("allowWrite")); good {
			s.AllowWrite = b
		} else {
			ok = false
		}
		if !strings.HasPrefix(s.ManagedID, core.BuiltinMCPPrefix) {
			c.errf(v, m.field("allowWrite"), "%s is only valid with a built-in managedId (builtin/<provider>)", m.field("allowWrite"))
			ok = false
		}
	}
	if v := m.get("command"); v != nil && !isNull(v) {
		s.Command, _ = c.nonEmptyStr(v, m.field("command"))
	}
	if v := m.get("url"); v != nil && !isNull(v) {
		if raw, good := c.nonEmptyStr(v, m.field("url")); good {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				c.errf(v, m.field("url"), "%s must be an absolute http or https URL", m.field("url"))
				ok = false
			} else {
				s.URL = raw
			}
		}
	}
	if v := m.get("args"); v != nil && !isNull(v) {
		if s.Command == "" && !m.present("command") {
			c.errf(v, m.field("args"), "%s is only valid with command", m.field("args"))
			ok = false
		}
		items := c.seq(v, m.field("args"))
		for i, it := range items {
			if a, good := c.str(it, indexPath(m.field("args"), i)); good {
				s.Args = append(s.Args, a)
			} else {
				ok = false
			}
		}
	}
	if v := m.get("env"); v != nil && !isNull(v) {
		if !m.present("command") {
			c.errf(v, m.field("env"), "%s is only valid with command", m.field("env"))
			ok = false
		}
		env, good := c.strMap(v, m.field("env"))
		ok = ok && good
		s.Env = env
	}
	if v := m.get("headers"); v != nil && !isNull(v) {
		if !m.present("url") {
			c.errf(v, m.field("headers"), "%s is only valid with url", m.field("headers"))
			ok = false
		}
		h, good := c.strMap(v, m.field("headers"))
		ok = ok && good
		s.Headers = h
	}
	return s, ok
}

// parseNetworkPolicy decodes a networkPolicy object; null yields nil.
func (c *doc) parseNetworkPolicy(n *yaml.Node, path string) (*core.NetworkPolicy, bool) {
	if isNull(n) {
		return nil, true
	}
	m := c.mapping(n, path)
	if m == nil {
		return nil, false
	}
	defer m.finish()
	p := &core.NetworkPolicy{}
	ok := true
	if v := m.get("extends"); v != nil && !isNull(v) {
		var refs []string
		var nodes []*yaml.Node
		if rv := resolve(v); rv.Kind == yaml.ScalarNode {
			if s, good := c.nonEmptyStr(v, m.field("extends")); good {
				refs, nodes = []string{s}, []*yaml.Node{v}
			} else {
				ok = false
			}
		} else {
			list, good := c.uniqueStrList(v, m.field("extends"))
			ok = ok && good
			refs, nodes = list, resolve(v).Content
		}
		for i, ref := range refs {
			if _, _, err := networkpolicy.Resolve(ref); err != nil {
				c.errf(nodes[i], m.field("extends"), "%v", err)
				ok = false
				continue
			}
			p.Extends = append(p.Extends, ref)
		}
	}
	hosts := func(key string) []string {
		v := m.get(key)
		if v == nil || isNull(v) {
			return nil
		}
		list, good := c.strList(v, m.field(key))
		if !good {
			ok = false
			return nil
		}
		items := resolve(v).Content
		out := make([]string, 0, len(list))
		seen := map[string]bool{}
		for i, h := range list {
			norm, err := networkpolicy.NormalizeHost(h)
			if err != nil {
				c.errf(items[i], indexPath(m.field(key), i), "%v", err)
				ok = false
				continue
			}
			if !seen[norm] {
				seen[norm] = true
				out = append(out, norm)
			}
		}
		return out
	}
	p.AllowedHosts = hosts("allowedHosts")
	p.DeniedHosts = hosts("deniedHosts")
	return p, ok
}

// execSettings holds execution settings shared by agentDefaults, agents and
// automations. Pointer/flag pairs distinguish "absent" from "explicitly cleared".
type execSettings struct {
	c           *doc
	node        *yaml.Node
	Model       *string
	modelNode   *yaml.Node
	Harness     *harnessOverride
	Machine     *string
	machineNode *yaml.Node
	EnvSet      bool
	Env         string
	SecretsSet  bool
	Secrets     []string
	MCPSet      bool
	MCP         map[string]core.MCPServer
	mcpNode     *yaml.Node
	WorkerSet   bool
	Worker      string
	Idle        *int
}

// parseExec consumes the execution-setting keys of m. fullHarness requires a
// complete harness object; idle enables idleTimeoutMinutes.
func (c *doc) parseExec(m *mapping, fullHarness, idle bool) execSettings {
	e := execSettings{c: c, node: m.node}
	if m.present("model") && m.present("harness") {
		c.errf(m.keyNode("harness"), m.field("harness"), "set at most one of model or harness")
	}
	if v := m.get("model"); v != nil && !isNull(v) {
		if s, ok := c.nonEmptyStr(v, m.field("model")); ok && c.checkModelHarness(v, m.field("model"), s) {
			e.Model, e.modelNode = &s, v
		}
	}
	if v := m.get("harness"); v != nil && !isNull(v) {
		e.Harness = c.parseHarness(v, m.field("harness"), fullHarness)
	}
	if v := m.get("machine"); v != nil && !isNull(v) {
		if s, ok := c.nonEmptyStr(v, m.field("machine")); ok {
			e.Machine, e.machineNode = &s, v
		}
	}
	if m.has("environmentId") {
		v := m.get("environmentId")
		e.EnvSet = true
		if !isNull(v) {
			e.Env, _ = c.str(v, m.field("environmentId"))
		}
	}
	if v := m.get("secrets"); v != nil && !isNull(v) {
		if list, ok := c.secretList(v, m.field("secrets")); ok {
			e.SecretsSet, e.Secrets = true, list
		}
	}
	if v := m.get("mcpServers"); v != nil && !isNull(v) {
		if srv, ok := c.parseMCPServers(v, m.field("mcpServers")); ok {
			e.MCPSet, e.MCP, e.mcpNode = true, srv, v
		}
	}
	if m.has("workerHost") {
		v := m.get("workerHost")
		e.WorkerSet = true
		if !isNull(v) {
			e.Worker, _ = c.str(v, m.field("workerHost"))
			e.Worker = strings.TrimSpace(e.Worker)
		}
	}
	if idle {
		if v := m.get("idleTimeoutMinutes"); v != nil && !isNull(v) {
			if i, ok := c.intRange(v, m.field("idleTimeoutMinutes"), 0, 60); ok {
				e.Idle = &i
			}
		}
	}
	return e
}

// checkModelHarness reports an error when the shorthand model does not select
// a harness. A Claude model name selects claude, an OpenAI model name selects
// codex.
func (c *doc) checkModelHarness(n *yaml.Node, path, model string) bool {
	if harness.HarnessForModel(model) != "" {
		return true
	}
	c.errf(n, path, "%s %q is not a Claude or an OpenAI model name, so it selects no harness; set harness with a type (claude or codex) and a model", path, model)
	return false
}

// withModel applies the shorthand `model: X` on top of base. The model name
// selects the harness. When the harness does not change, the other settings
// of base stay. Otherwise the result has only the type and the model.
func withModel(base core.HarnessConfig, model string) core.HarnessConfig {
	t := harness.HarnessForModel(model)
	if t == "" {
		t = base.Type
	}
	if t != base.Type {
		return core.HarnessConfig{Type: t, Model: model}
	}
	out := base
	out.Model = model
	if out.Auth != nil {
		a := *out.Auth
		out.Auth = &a
	}
	return out
}

// applyHarness applies a model shorthand or sparse harness override on top of
// base. It reports errors through the override's document.
func applyHarness(base core.HarnessConfig, model *string, h *harnessOverride) core.HarnessConfig {
	if model != nil {
		return withModel(base, *model)
	}
	if h == nil {
		return base
	}
	out := base
	if h.Type != nil && *h.Type != base.Type {
		// Changing harness type: inherited type-specific settings do not carry over.
		out = core.HarnessConfig{Type: *h.Type}
		if h.Model == nil {
			h.c.errf(h.node, h.path+".model", "%s.model is required when changing the harness type from %s to %s", h.path, base.Type, *h.Type)
			out.Model = base.Model
		}
	}
	if h.Model != nil {
		out.Model = *h.Model
	}
	if h.Reasoning != nil {
		out.ReasoningLevel = *h.Reasoning
	}
	if h.AuthSet {
		out.Auth = h.Auth
	}
	if out.Auth != nil {
		a := *out.Auth
		out.Auth = &a
	}
	return out
}

func toHarness(h *harnessOverride, model *string) (core.HarnessConfig, bool) {
	if model != nil {
		return core.HarnessConfig{Type: harness.HarnessForModel(*model), Model: *model}, true
	}
	if h == nil || h.Type == nil || h.Model == nil {
		return core.HarnessConfig{}, false
	}
	out := core.HarnessConfig{Type: *h.Type, Model: *h.Model}
	if h.Reasoning != nil {
		out.ReasoningLevel = *h.Reasoning
	}
	out.Auth = h.Auth
	return out, true
}

func dedupe(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func copyMCP(m map[string]core.MCPServer) map[string]core.MCPServer {
	if m == nil {
		return nil
	}
	out := make(map[string]core.MCPServer, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
