package definition

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

var codeForgeValues = []string{"GITHUB", "GITLAB", "AZURE_DEVOPS"}

// factorySpec is the parsed factory.yaml.
type factorySpec struct {
	c              *doc
	def            *core.CompiledDefinition
	networkPolicy  *core.NetworkPolicy
	mcp            map[string]core.MCPServer
	defaults       execSettings
	defaultHarness core.HarnessConfig
	harnessOK      bool
	scorerDefaults *scorerDefaultsSpec
	repoSubs       []repoSubstitution
}

type scorerDefaultsSpec struct {
	machine     string
	machineNode *yaml.Node
	secrets     []string
	mcp         map[string]core.MCPServer
	model       *string
	harness     *harnessOverride
}

type repoSubstitution struct {
	node           *yaml.Node
	source, target core.RepoRef
}

const factoryFile = "factory.yaml"

func parseFactory(d *diag, content string) *factorySpec {
	c := &doc{d: d, file: factoryFile}
	root := c.parse(content)
	if root == nil {
		return nil
	}
	m := c.mapping(root, "")
	if m == nil {
		return nil
	}
	defer m.finish()
	f := &factorySpec{c: c, def: &core.CompiledDefinition{GithubOnboardingNotices: true, FactoryConfigChecks: "main"}}
	def := f.def

	if v := m.require("schemaVersion"); v != nil {
		if s, ok := c.str(v, "schemaVersion"); ok && s != SchemaVersion {
			c.errf(v, "schemaVersion", "schemaVersion must be %s (got %q)", SchemaVersion, s)
		}
	}
	def.SchemaVersion = SchemaVersion
	if v := m.require("name"); v != nil {
		def.Name, _ = c.nonEmptyStr(v, "name")
	}
	if v := m.get("description"); v != nil && !isNull(v) {
		def.Description, _ = c.str(v, "description")
	}
	if v := m.get("alias"); v != nil && !isNull(v) {
		if s, ok := c.str(v, "alias"); ok {
			if !aliasRe.MatchString(s) || strings.TrimSpace(s) == "" {
				c.errf(v, "alias", "alias must be 1-60 letters, numbers, spaces, '.', '_' or '-' (got %q)", s)
			} else {
				def.Alias = s
			}
		}
	}
	if v := m.get("credentialStrategy"); v != nil && !isNull(v) {
		def.CredentialStrategy, _ = c.enum(v, "credentialStrategy", "EXECUTOR", "CREATOR")
	}
	if v := m.get("maxConcurrentRuns"); v != nil && !isNull(v) {
		if i, ok := c.integer(v, "maxConcurrentRuns"); ok {
			if i < 1 {
				c.errf(v, "maxConcurrentRuns", "maxConcurrentRuns must be 1 or more (got %d)", i)
			} else {
				def.MaxConcurrentRuns = i
			}
		}
	}
	if v := m.get("approvers"); v != nil && !isNull(v) {
		if list, ok := c.uniqueStrList(v, "approvers"); ok {
			if len(list) == 0 {
				c.errf(v, "approvers", "approvers must list at least one user email or service account name")
			} else {
				def.Approvers = list
			}
		}
	}
	if v := m.get("restrictFollowupsToStarter"); v != nil && !isNull(v) {
		def.RestrictFollowupsToStarter, _ = c.boolean(v, "restrictFollowupsToStarter")
	}
	if v := m.get("requireLinkedActor"); v != nil && !isNull(v) {
		def.RequireLinkedActor, _ = c.boolean(v, "requireLinkedActor")
	}
	if v := m.get("allowAgentMerge"); v != nil && !isNull(v) {
		def.AllowAgentMerge, _ = c.boolean(v, "allowAgentMerge")
	}
	if v := m.get("spendLimit"); v != nil && !isNull(v) {
		if sm := c.mapping(v, "spendLimit"); sm != nil {
			lim := &core.SpendLimit{}
			for _, k := range []string{"daily", "monthly"} {
				n := sm.get(k)
				if n == nil || isNull(n) {
					continue
				}
				f, ok := c.number(n, "spendLimit."+k)
				if !ok {
					continue
				}
				if f <= 0 {
					c.errf(n, "spendLimit."+k, "spendLimit.%s must be more than 0 US dollars (got %v)", k, f)
					continue
				}
				if k == "daily" {
					lim.DailyUSD = f
				} else {
					lim.MonthlyUSD = f
				}
			}
			sm.finish()
			if lim.DailyUSD > 0 || lim.MonthlyUSD > 0 {
				def.SpendLimit = lim
			}
		}
	}
	if v := m.get("automations"); v != nil && !isNull(v) {
		if am := c.mapping(v, "automations"); am != nil {
			if g := am.get("githubOnboardingNotices"); g != nil && !isNull(g) {
				if b, ok := c.boolean(g, "automations.githubOnboardingNotices"); ok {
					def.GithubOnboardingNotices = b
				}
			}
			am.finish()
		}
	}
	if v := m.get("github"); v != nil && !isNull(v) {
		if gm := c.mapping(v, "github"); gm != nil {
			if g := gm.get("factoryConfigChecks"); g != nil && !isNull(g) {
				if s, ok := c.enum(g, "github.factoryConfigChecks", "main", "all"); ok {
					def.FactoryConfigChecks = s
				}
			}
			gm.finish()
		}
	}

	f.parseRepositories(m)

	if v := m.get("secrets"); v != nil && !isNull(v) {
		def.Secrets, _ = c.secretList(v, "secrets")
	}
	if m.has("networkPolicy") {
		f.networkPolicy, _ = c.parseNetworkPolicy(m.get("networkPolicy"), "networkPolicy")
	}
	if v := m.get("mcpServers"); v != nil && !isNull(v) {
		f.mcp, _ = c.parseMCPServers(v, "mcpServers")
		def.MCPServers = copyMCP(f.mcp)
	}
	f.parseCloudProviders(m)
	f.parseIntegrations(m)
	if v := m.get("scorerDefaults"); v != nil && !isNull(v) {
		f.parseScorerDefaults(v)
	}
	if v := m.get("selfImprovement"); v != nil && !isNull(v) {
		def.SelfImprovement = parseSelfImprovement(c, v)
	}

	if v := m.require("agentDefaults"); v != nil {
		if am := c.mapping(v, "agentDefaults"); am != nil {
			f.defaults = c.parseExec(am, true, false)
			am.finish()
			switch {
			case f.defaults.Model == nil && f.defaults.Harness == nil && !am.present("model") && !am.present("harness"):
				c.errf(am.node, "agentDefaults", "agentDefaults must set exactly one of model or harness")
			default:
				if h, ok := toHarness(f.defaults.Harness, f.defaults.Model); ok {
					f.defaultHarness, f.harnessOK = h, true
				}
			}
			if f.defaults.WorkerSet && f.defaults.Worker == "" {
				// An empty value at the defaults level simply selects the workspace default.
				f.defaults.WorkerSet = false
			}
			if f.harnessOK && f.defaultHarness.Auth != nil && f.defaultHarness.Auth.Source == "workerEnvironment" && f.defaults.Worker == "" {
				c.errf(f.defaults.Harness.authNode, "agentDefaults.harness.auth.source", "auth.source workerEnvironment requires agentDefaults.workerHost to select a self-hosted worker")
			}
			if f.defaults.SecretsSet && f.defaults.Secrets == nil {
				f.defaults.Secrets = []string{}
			}
		}
	}
	if f.mcp != nil && f.defaults.MCPSet {
		for name := range f.defaults.MCP {
			if _, dup := f.mcp[name]; dup {
				c.errf(f.defaults.mcpNode, "agentDefaults.mcpServers."+name, "MCP server %q is already granted factory-wide by mcpServers", name)
			}
		}
	}
	return f
}

func (f *factorySpec) parseRepositories(m *mapping) {
	c, def := f.c, f.def
	var forges []string
	forgesSet := false
	if v := m.get("codeForges"); v != nil && !isNull(v) {
		list, ok := c.uniqueStrList(v, "codeForges")
		if ok {
			items := resolve(v).Content
			for i, s := range list {
				if !contains(codeForgeValues, s) {
					c.errf(items[i], indexPath("codeForges", i), "codeForges values must be one of %s (got %q)", strings.Join(codeForgeValues, ", "), s)
					ok = false
				}
			}
		}
		if ok {
			forges, forgesSet = list, true
			def.CodeForges = list
		}
	}
	v := m.require("repositories")
	if v == nil {
		def.Repositories = []core.RepoRef{}
		return
	}
	items := c.seq(v, "repositories")
	def.Repositories = []core.RepoRef{}
	if forgesSet && len(forges) == 0 && len(items) > 0 {
		c.errf(v, "repositories", "repositories must be empty when codeForges is an empty list")
	}
	seen := map[string]bool{}
	for i, it := range items {
		p := indexPath("repositories", i)
		rm := c.mapping(it, p)
		if rm == nil {
			continue
		}
		r := core.RepoRef{}
		if cv := rm.get("codeForge"); cv != nil && !isNull(cv) {
			r.CodeForge, _ = c.enum(cv, p+".codeForge", codeForgeValues...)
			if r.CodeForge != "" && forgesSet && !contains(forges, r.CodeForge) {
				c.errf(cv, p+".codeForge", "%s %s is not listed in codeForges", p+".codeForge", r.CodeForge)
			}
		} else if len(forges) > 1 {
			c.errf(rm.node, p+".codeForge", "%s.codeForge is required when codeForges lists more than one code host", p)
		}
		if r.CodeForge == "" {
			if len(forges) == 1 {
				r.CodeForge = forges[0]
			} else {
				r.CodeForge = "GITHUB"
			}
		}
		if ov := rm.require("owner"); ov != nil {
			if s, ok := c.nonEmptyStr(ov, p+".owner"); ok {
				if !validOwner(r.CodeForge, s) {
					c.errf(ov, p+".owner", "%s %q is not a valid owner for %s", p+".owner", s, r.CodeForge)
				}
				r.Owner = s
			}
		}
		if nv := rm.require("name"); nv != nil {
			if s, ok := c.nonEmptyStr(nv, p+".name"); ok {
				if strings.ContainsAny(s, "/ \t") {
					c.errf(nv, p+".name", "%s %q must be a single repository name", p+".name", s)
				}
				r.Name = s
			}
		}
		rm.finish()
		key := strings.ToLower(r.CodeForge + ":" + r.Owner + "/" + r.Name)
		if r.Owner != "" && r.Name != "" {
			if seen[key] {
				c.errf(rm.node, p, "repository %s/%s is listed more than once", r.Owner, r.Name)
			}
			seen[key] = true
		}
		def.Repositories = append(def.Repositories, r)
	}

	if sv := m.get("benchmarkRepoSubstitutions"); sv != nil && !isNull(sv) {
		for i, it := range c.seq(sv, "benchmarkRepoSubstitutions") {
			p := indexPath("benchmarkRepoSubstitutions", i)
			sm := c.mapping(it, p)
			if sm == nil {
				continue
			}
			sub := repoSubstitution{node: sm.node}
			if n := sm.require("source"); n != nil {
				sub.source = f.parseRepoTriple(n, p+".source")
			}
			if n := sm.require("target"); n != nil {
				sub.target = f.parseRepoTriple(n, p+".target")
			}
			sm.finish()
			if sub.source.Owner != "" && sub.target.Owner != "" {
				if !f.ownsRepo(sub.source) {
					c.errf(sm.node, p+".source", "%s.source %s must be one of the factory's repositories", p, sub.source.FullName())
				}
				if f.ownsRepo(sub.target) {
					c.errf(sm.node, p+".target", "%s.target %s must not be one of the factory's repositories", p, sub.target.FullName())
				}
			}
			f.repoSubs = append(f.repoSubs, sub)
			f.def.BenchmarkRepoSubstitutions = append(f.def.BenchmarkRepoSubstitutions, core.RepoSubstitution{Source: sub.source, Target: sub.target})
		}
	}
}

func (f *factorySpec) parseRepoTriple(n *yaml.Node, p string) core.RepoRef {
	c := f.c
	m := c.mapping(n, p)
	if m == nil {
		return core.RepoRef{}
	}
	defer m.finish()
	r := core.RepoRef{}
	if v := m.require("codeForge"); v != nil {
		if s, ok := c.enum(v, p+".codeForge", codeForgeValues...); ok {
			if s != "GITHUB" {
				c.errf(v, p+".codeForge", "%s.codeForge must be GITHUB for benchmark repository substitutions", p)
			}
			r.CodeForge = s
		}
	}
	if v := m.require("owner"); v != nil {
		r.Owner, _ = c.nonEmptyStr(v, p+".owner")
	}
	if v := m.require("name"); v != nil {
		r.Name, _ = c.nonEmptyStr(v, p+".name")
	}
	return r
}

func (f *factorySpec) ownsRepo(r core.RepoRef) bool {
	for _, x := range f.def.Repositories {
		if strings.EqualFold(x.CodeForge, r.CodeForge) && strings.EqualFold(x.Owner, r.Owner) && strings.EqualFold(x.Name, r.Name) {
			return true
		}
	}
	return false
}

func validOwner(forge, s string) bool {
	if strings.ContainsAny(s, " \t") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return false
	}
	switch forge {
	case "GITLAB", "AZURE_DEVOPS":
		return true // GitLab groups/subgroups; Azure DevOps organization/project
	default:
		return !strings.Contains(s, "/")
	}
}

func (f *factorySpec) parseCloudProviders(m *mapping) {
	c := f.c
	key := "cloudProviders"
	v := m.get(key)
	if v == nil || isNull(v) {
		return
	}
	cm := c.mapping(v, key)
	if cm == nil {
		return
	}
	defer cm.finish()
	cp := &core.CloudProviders{}
	if g := cm.get("gcp"); g != nil && !isNull(g) {
		p := key + ".gcp"
		if gm := c.mapping(g, p); gm != nil {
			gcp := &struct {
				ProjectNumber                        string `json:"projectNumber"`
				WorkloadIdentityFederationPoolID     string `json:"workloadIdentityFederationPoolId"`
				WorkloadIdentityFederationProviderID string `json:"workloadIdentityFederationProviderId"`
				ServiceAccountEmail                  string `json:"serviceAccountEmail,omitempty"`
			}{}
			if n := gm.require("projectNumber"); n != nil {
				if s, ok := c.nonEmptyStr(n, p+".projectNumber"); ok {
					if strings.Trim(s, "0123456789") != "" {
						c.errf(n, p+".projectNumber", "%s.projectNumber must contain only digits", p)
					}
					gcp.ProjectNumber = s
				}
			}
			if n := gm.require("workloadIdentityFederationPoolId"); n != nil {
				gcp.WorkloadIdentityFederationPoolID, _ = c.nonEmptyStr(n, p+".workloadIdentityFederationPoolId")
			}
			if n := gm.require("workloadIdentityFederationProviderId"); n != nil {
				gcp.WorkloadIdentityFederationProviderID, _ = c.nonEmptyStr(n, p+".workloadIdentityFederationProviderId")
			}
			if n := gm.get("serviceAccountEmail"); n != nil && !isNull(n) {
				if s, ok := c.str(n, p+".serviceAccountEmail"); ok {
					if !emailRe.MatchString(s) {
						c.errf(n, p+".serviceAccountEmail", "%s.serviceAccountEmail %q is not an email address", p, s)
					}
					gcp.ServiceAccountEmail = s
				}
			}
			gm.finish()
			cp.GCP = gcp
		}
	}
	if a := cm.get("aws"); a != nil && !isNull(a) {
		p := key + ".aws"
		if am := c.mapping(a, p); am != nil {
			aws := &struct {
				RoleARN string `json:"roleArn"`
			}{}
			if n := am.require("roleArn"); n != nil {
				if s, ok := c.nonEmptyStr(n, p+".roleArn"); ok {
					if !strings.HasPrefix(s, "arn:") || strings.Count(s, ":") < 5 {
						c.errf(n, p+".roleArn", "%s.roleArn %q is not an IAM role ARN", p, s)
					}
					aws.RoleARN = s
				}
			}
			am.finish()
			cp.AWS = aws
		}
	}
	if n := cm.get("audiences"); n != nil && !isNull(n) {
		if list, ok := c.uniqueStrList(n, key+".audiences"); ok {
			for _, a := range list {
				if strings.TrimSpace(a) == "" {
					c.errf(n, key+".audiences", "%s.audiences must not contain an empty audience", key)
				}
			}
			cp.Audiences = list
		}
	}
	f.def.CloudProviders = cp
}

var integrationTypes = []string{"slack", "microsoft-teams", "linear", "jira"}

func (f *factorySpec) parseIntegrations(m *mapping) {
	c, def := f.c, f.def
	if !m.has("integrations") {
		return
	}
	v := m.get("integrations")
	def.IntegrationsSet = true
	def.Integrations = []core.IntegrationDecl{}
	if isNull(v) {
		return
	}
	seen := map[string]*yaml.Node{}
	for i, it := range c.seq(v, "integrations") {
		p := indexPath("integrations", i)
		im := c.mapping(it, p)
		if im == nil {
			continue
		}
		typ, ok := c.enum(im.require("type"), p+".type", integrationTypes...)
		decl := core.IntegrationDecl{Type: typ}
		for _, t := range integrationTypes {
			sv := im.get(t)
			if sv == nil || isNull(sv) {
				continue
			}
			if t != typ {
				c.errf(im.keyNode(t), p+"."+t, "%s.%s settings are only valid on an integration of type %s", p, t, t)
				continue
			}
			decl.Settings = parseIntegrationSettings(c, t, sv, p+"."+t)
		}
		im.finish()
		if !ok {
			continue
		}
		if seen[typ] != nil {
			c.errf(im.node, p+".type", "integration %s is declared more than once", typ)
			continue
		}
		seen[typ] = im.node
		def.Integrations = append(def.Integrations, decl)
	}
	if seen["linear"] != nil && seen["jira"] != nil {
		c.errf(seen["jira"], "integrations", "linear and jira are mutually exclusive; declare at most one issue tracker")
	}
}

func parseIntegrationSettings(c *doc, typ string, n *yaml.Node, p string) map[string]string {
	m := c.mapping(n, p)
	if m == nil {
		return nil
	}
	defer m.finish()
	out := map[string]string{}
	switch typ {
	case "slack", "microsoft-teams":
		if v := m.get("autoRespondToThreadReplies"); v != nil && !isNull(v) {
			if b, ok := c.boolean(v, p+".autoRespondToThreadReplies"); ok {
				if b {
					out["autoRespondToThreadReplies"] = "true"
				} else {
					out["autoRespondToThreadReplies"] = "false"
				}
			}
		}
	case "linear":
		// Compatibility field: accepted but ignored for discovery and routing.
		if v := m.get("teamIds"); v != nil && !isNull(v) {
			c.strList(v, p+".teamIds")
		}
	case "jira":
		if v := m.get("projectKeys"); v != nil && !isNull(v) {
			c.strList(v, p+".projectKeys")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (f *factorySpec) parseScorerDefaults(n *yaml.Node) {
	c := f.c
	m := c.mapping(n, "scorerDefaults")
	if m == nil {
		return
	}
	defer m.finish()
	sd := &scorerDefaultsSpec{}
	if v := m.get("machine"); v != nil && !isNull(v) {
		sd.machine, _ = c.nonEmptyStr(v, "scorerDefaults.machine")
		sd.machineNode = v
	}
	if v := m.get("secrets"); v != nil && !isNull(v) {
		sd.secrets, _ = c.secretList(v, "scorerDefaults.secrets")
	}
	if v := m.get("mcpServers"); v != nil && !isNull(v) {
		sd.mcp, _ = c.parseMCPServers(v, "scorerDefaults.mcpServers")
	}
	if v := m.get("model"); v != nil && !isNull(v) {
		if s, ok := c.nonEmptyStr(v, "scorerDefaults.model"); ok && c.checkModelHarness(v, "scorerDefaults.model", s) {
			sd.model = &s
		}
	}
	if v := m.get("harness"); v != nil && !isNull(v) {
		sd.harness = c.parseHarness(v, "scorerDefaults.harness", false)
	}
	f.scorerDefaults = sd
	f.def.ScorerDefaults = &core.AgentOverrides{Machine: sd.machine, Secrets: sd.secrets, MCPServers: sd.mcp}
	if sd.model != nil {
		f.def.ScorerDefaults.Model = *sd.model
	}
}

func parseSelfImprovement(c *doc, n *yaml.Node) *core.SelfImprovementConfig {
	m := c.mapping(n, "selfImprovement")
	if m == nil {
		return nil
	}
	defer m.finish()
	si := &core.SelfImprovementConfig{ReviewerType: "admins"}
	if v := m.get("failedRunThreshold"); v != nil && !isNull(v) {
		si.FailedRunThreshold, _ = c.intRange(v, "selfImprovement.failedRunThreshold", 1, 50)
	}
	if v := m.get("reviewerType"); v != nil && !isNull(v) {
		if s, ok := c.enum(v, "selfImprovement.reviewerType", "admins", "team", "custom", "none"); ok {
			si.ReviewerType = s
		}
	}
	ev := m.get("reviewerEmails")
	hasEmails := ev != nil && !isNull(ev)
	switch {
	case si.ReviewerType == "custom" && !hasEmails:
		c.errf(m.node, "selfImprovement.reviewerEmails", "selfImprovement.reviewerEmails is required when reviewerType is custom")
	case si.ReviewerType != "custom" && hasEmails:
		c.errf(ev, "selfImprovement.reviewerEmails", "selfImprovement.reviewerEmails is only valid when reviewerType is custom")
	case hasEmails:
		list, ok := c.uniqueStrList(ev, "selfImprovement.reviewerEmails")
		if ok && len(list) == 0 {
			c.errf(ev, "selfImprovement.reviewerEmails", "selfImprovement.reviewerEmails must list at least one email")
		}
		for i, e := range list {
			if !emailRe.MatchString(e) {
				c.errf(resolve(ev).Content[i], indexPath("selfImprovement.reviewerEmails", i), "%q is not an email address", e)
			}
		}
		si.ReviewerEmails = list
	}
	if v := m.get("analysisModel"); v != nil && !isNull(v) {
		si.AnalysisModel, _ = c.nonEmptyStr(v, "selfImprovement.analysisModel")
	}
	return si
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
