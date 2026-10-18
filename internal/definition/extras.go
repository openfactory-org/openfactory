package definition

import (
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// parseSkill parses a skill directory. scope is "factory" or the agent name.
func (c *compiler) parseSkill(name string, sd *skillDir, scope string) (core.Skill, bool) {
	s := core.Skill{Name: name, Path: sd.skillMD, Scope: scope}
	if sd.skillMD == "" {
		c.d.add(sd.dir, 0, "", "skill directory %s has no SKILL.md", sd.dir)
		return s, false
	}
	before := len(c.d.errs)
	content := c.t.files[sd.skillMD]
	s.Content = content
	dc, m, _, ok := parseMarkdown(c.d, sd.skillMD, content, true)
	if !ok || m == nil {
		return s, false
	}
	// SKILL.md frontmatter may carry additional keys (license, metadata, ...);
	// only name and description are interpreted here.
	for _, k := range m.order {
		m.get(k)
	}
	if v := m.require("name"); v != nil {
		if n, ok := dc.nonEmptyStr(v, "name"); ok {
			switch {
			case len(n) > 64 || !skillNameRe.MatchString(n):
				dc.errf(v, "name", "skill name %q must be at most 64 lowercase letters, digits and single hyphens", n)
			case n != name:
				dc.errf(v, "name", "skill name %q must match its directory name %q", n, name)
			}
			s.Name = n
		}
	}
	if v := m.require("description"); v != nil {
		if desc, ok := dc.nonEmptyStr(v, "description"); ok {
			if len(desc) > 1024 {
				dc.errf(v, "description", "skill description must be at most 1024 characters")
			}
			s.Description = desc
		}
	}
	if len(sd.supports) > 0 {
		s.Files = map[string]string{}
		for rel, p := range sd.supports {
			s.Files[rel] = c.t.files[p]
		}
	}
	return s, len(c.d.errs) == before
}

func (c *compiler) compileSkills() {
	for _, name := range sortedNames(c.t.skills) {
		if s, ok := c.parseSkill(name, c.t.skills[name], "factory"); ok {
			c.def.Skills = append(c.def.Skills, s)
		}
	}
}

// agentSkills parses an agent's own skills.
func (c *compiler) agentSkills(agent string) []core.Skill {
	dirs := c.t.agentSkills[agent]
	var out []core.Skill
	for _, name := range sortedNames(dirs) {
		if s, ok := c.parseSkill(name, dirs[name], agent); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---- scorers ----

func (c *compiler) compileScorers() {
	names := map[string]string{}
	for _, slug := range sortedNames(c.t.scorers) {
		p := c.t.scorers[slug]
		s, ok := c.parseScorer(p, slug, c.t.files[p])
		if !ok {
			continue
		}
		if prev, dup := names[s.Name]; dup {
			c.d.add(p, 0, "name", "scorer name %q is already used by %s", s.Name, prev)
			continue
		}
		names[s.Name] = p
		c.def.Scorers = append(c.def.Scorers, s)
	}
}

func (c *compiler) parseScorer(file, slug, content string) (core.Scorer, bool) {
	before := len(c.d.errs)
	s := core.Scorer{Slug: slug, SamplingRate: 10}
	dc, m, body, ok := parseMarkdown(c.d, file, content, true)
	if !ok || m == nil {
		return s, false
	}
	defer m.finish()
	s.Rubric = strings.TrimSpace(body)
	if s.Rubric == "" {
		dc.errf(nil, "", "the scorer's Markdown body (its rubric) must not be empty")
	}
	if v := m.require("name"); v != nil {
		s.Name, _ = dc.nonEmptyStr(v, "name")
	}
	if v := m.get("description"); v != nil && !isNull(v) {
		s.Description, _ = dc.str(v, "description")
	}
	if v := m.get("output"); v != nil && !isNull(v) {
		dc.enum(v, "output", "classification")
	}
	passingOK := false
	if v := m.require("passingScore"); v != nil {
		if f, ok := dc.number(v, "passingScore"); ok {
			if f < 0 || f > 1 {
				dc.errf(v, "passingScore", "passingScore must be between 0 and 1")
			} else {
				s.PassingScore, passingOK = f, true
			}
		}
	}
	if v := m.get("samplingRate"); v != nil && !isNull(v) {
		if f, ok := dc.number(v, "samplingRate"); ok {
			if f < 0 || f > 100 {
				dc.errf(v, "samplingRate", "samplingRate must be between 0 and 100")
			} else {
				s.SamplingRate = roundHundredths(resolve(v).Value, f)
			}
		}
	}
	var harness *harnessOverride
	if v := m.get("harness"); v != nil && !isNull(v) {
		harness = dc.parseHarness(v, "harness", false)
	}
	if harness == nil {
		if v := m.require("model"); v != nil {
			if mv, ok := dc.nonEmptyStr(v, "model"); ok && dc.checkModelHarness(v, "model", mv) {
				s.Model = mv
			}
		}
	} else if v := m.get("model"); v != nil && !isNull(v) {
		s.Model, _ = dc.nonEmptyStr(v, "model")
	}
	if v := m.get("selfImprovement"); v != nil && !isNull(v) {
		s.SelfImprovement, _ = dc.boolean(v, "selfImprovement")
	}
	if v := m.require("agents"); v != nil {
		items := dc.seq(v, "agents")
		if isSeq(v) && len(items) == 0 {
			dc.errf(v, "agents", "agents must list at least one agent")
		}
		seen := map[string]bool{}
		for i, it := range items {
			p := indexPath("agents", i)
			var sa core.ScorerAgent
			var nameNode *yaml.Node
			if r := resolve(it); r.Kind == yaml.ScalarNode {
				n, ok := dc.nonEmptyStr(it, p)
				if !ok {
					continue
				}
				sa.Name, nameNode = n, it
			} else {
				am := dc.mapping(it, p)
				if am == nil {
					continue
				}
				if nv := am.require("name"); nv != nil {
					sa.Name, _ = dc.nonEmptyStr(nv, p+".name")
					nameNode = nv
				}
				if dv := am.get("includeDescendants"); dv != nil && !isNull(dv) {
					sa.IncludeDescendants, _ = dc.boolean(dv, p+".includeDescendants")
				}
				am.finish()
			}
			if sa.Name == "" {
				continue
			}
			if _, ok := c.t.agents[sa.Name]; !ok {
				dc.errf(nameNode, p, "scorer agent %q does not match any agents/<name>/agent.md", sa.Name)
			}
			if seen[sa.Name] {
				dc.errf(nameNode, p, "agent %q is listed more than once", sa.Name)
			}
			seen[sa.Name] = true
			s.Agents = append(s.Agents, sa)
		}
	}
	if v := m.require("labels"); v != nil {
		items := dc.seq(v, "labels")
		if isSeq(v) && (len(items) < 1 || len(items) > 20) {
			dc.errf(v, "labels", "labels must list 1 to 20 classifications (found %d)", len(items))
		}
		seen := map[string]bool{}
		for i, it := range items {
			p := indexPath("labels", i)
			lm := dc.mapping(it, p)
			if lm == nil {
				continue
			}
			l := core.ScorerLabel{}
			if lv := lm.require("value"); lv != nil {
				if val, ok := dc.nonEmptyStr(lv, p+".value"); ok {
					if seen[val] {
						dc.errf(lv, p+".value", "label value %q is used more than once", val)
					}
					seen[val] = true
					l.Value = val
				}
			}
			if dv := lm.get("description"); dv != nil && !isNull(dv) {
				l.Description, _ = dc.str(dv, p+".description")
			}
			if sv := lm.require("score"); sv != nil {
				if f, ok := dc.number(sv, p+".score"); ok {
					if f < 0 || f > 1 {
						dc.errf(sv, p+".score", "%s.score must be between 0 and 1", p)
					}
					l.Score = f
				}
			}
			lm.finish()
			s.Labels = append(s.Labels, l)
		}
		if passingOK && len(s.Labels) > 0 {
			pass, fail := false, false
			for _, l := range s.Labels {
				if l.Score >= s.PassingScore {
					pass = true
				} else {
					fail = true
				}
			}
			if !pass {
				dc.errf(v, "labels", "at least one label must meet passingScore %v", s.PassingScore)
			}
			if !fail {
				dc.errf(v, "labels", "at least one label must fall below passingScore %v", s.PassingScore)
			}
		}
	}
	// Scorer execution settings override scorerDefaults for this scorer.
	if v := m.get("machine"); v != nil && !isNull(v) {
		if r, ok := dc.nonEmptyStr(v, "machine"); ok {
			c.machineRef(dc, r, "machine", dc.line(v))
			s.Machine = r
		}
	}
	if v := m.get("secrets"); v != nil && !isNull(v) {
		if list, ok := dc.secretList(v, "secrets"); ok {
			s.Secrets = list
		}
	}
	if v := m.get("mcpServers"); v != nil && !isNull(v) {
		if srv, ok := dc.parseMCPServers(v, "mcpServers"); ok {
			s.MCPServers = srv
		}
	}
	c.resolveScorerExec(&s, harness)
	return s, len(c.d.errs) == before
}

// scorerBaseHarness returns the harness of the judge before the fields of one
// scorer apply: scorerDefaults.harness and scorerDefaults.model over the
// harness of agentDefaults.
func (c *compiler) scorerBaseHarness() (core.HarnessConfig, bool) {
	base, ok := c.f.defaultHarness, c.f.harnessOK
	sd := c.f.scorerDefaults
	if sd == nil {
		return base, ok
	}
	if sd.harness != nil {
		if !ok {
			h, full := toHarness(sd.harness, nil)
			if !full {
				return base, false
			}
			base, ok = h, true
		} else {
			base = applyHarness(base, nil, sd.harness)
		}
	}
	if sd.model != nil && ok {
		base = withModel(base, *sd.model)
	}
	return base, ok
}

// resolveScorerExec sets the effective execution settings of the judge run of
// a scorer: its harness (with the scorer model), machine, secrets and MCP
// servers. A scorer value replaces the value of scorerDefaults; secrets and
// MCP servers add to the lists of scorerDefaults.
func (c *compiler) resolveScorerExec(s *core.Scorer, harness *harnessOverride) {
	base, ok := c.scorerBaseHarness()
	if harness != nil {
		if ok {
			base = applyHarness(base, nil, harness)
		} else if h, full := toHarness(harness, nil); full {
			base, ok = h, true
		}
	}
	if ok {
		switch {
		case s.Model != "" && harness == nil:
			// The shorthand model selects the harness of the judge.
			base = withModel(base, s.Model)
		case s.Model != "":
			base.Model = s.Model
		}
		h := base
		s.Harness = &h
		if s.Model == "" {
			s.Model = h.Model
		}
	}
	sd := c.f.scorerDefaults
	if sd == nil {
		return
	}
	if s.Machine == "" {
		s.Machine = sd.machine
	}
	if len(sd.secrets) > 0 {
		s.Secrets = dedupe(sd.secrets, s.Secrets)
	}
	if len(sd.mcp) > 0 {
		merged := map[string]core.MCPServer{}
		for k, v := range sd.mcp {
			merged[k] = v
		}
		for k, v := range s.MCPServers {
			merged[k] = v
		}
		s.MCPServers = merged
	}
}

// roundHundredths rounds a decimal literal to two decimal places, half away
// from zero, using exact decimal arithmetic (so 12.345 rounds to 12.35).
func roundHundredths(literal string, fallback float64) float64 {
	r, ok := new(big.Rat).SetString(literal)
	if !ok {
		return math.Round(fallback*100) / 100
	}
	r.Mul(r, big.NewRat(100, 1))
	num, den := new(big.Int).Set(r.Num()), r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half away from zero: compare 2*|remainder| with the denominator.
	m.Abs(m).Mul(m, big.NewInt(2))
	if m.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	f, _ := new(big.Rat).SetFrac(q, big.NewInt(100)).Float64()
	return f
}

// ---- benchmarks ----

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c *compiler) compileBenchmarks() {
	names := map[string]string{}
	for _, slug := range sortedNames(c.t.benchmarks) {
		b := c.t.benchmarks[slug]
		if b.suite == "" {
			continue
		}
		suite, ok := c.parseSuite(slug, b)
		if !ok {
			continue
		}
		if prev, dup := names[suite.Name]; dup {
			c.d.add(b.suite, 0, "name", "benchmark suite name %q is already used by %s", suite.Name, prev)
			continue
		}
		names[suite.Name] = b.suite
		c.def.Benchmarks = append(c.def.Benchmarks, suite)
	}
}

func (c *compiler) parseSuite(slug string, b *benchDir) (core.BenchmarkSuite, bool) {
	before := len(c.d.errs)
	s := core.BenchmarkSuite{Slug: slug}
	dc := &doc{d: c.d, file: b.suite}
	root := dc.parse(c.t.files[b.suite])
	if root == nil {
		return s, false
	}
	m := dc.mapping(root, "")
	if m == nil {
		return s, false
	}
	defer m.finish()
	if v := m.require("name"); v != nil {
		s.Name, _ = dc.nonEmptyStr(v, "name")
	}
	if v := m.get("description"); v != nil && !isNull(v) {
		s.Description, _ = dc.str(v, "description")
	}
	if v := m.require("agent"); v != nil {
		if a, ok := dc.nonEmptyStr(v, "agent"); ok {
			if _, exists := c.t.agents[a]; !exists {
				dc.errf(v, "agent", "agent %q does not match any agents/<name>/agent.md", a)
			}
			s.Agent = a
		}
	}
	if v := m.get("configurations"); v != nil && !isNull(v) {
		items := dc.seq(v, "configurations")
		if isSeq(v) && (len(items) < 1 || len(items) > 6) {
			dc.errf(v, "configurations", "configurations must list 1 to 6 configurations (found %d)", len(items))
		}
		seen := map[string]bool{}
		for i, it := range items {
			p := indexPath("configurations", i)
			cfg, ok := c.parseBenchConfig(dc, it, p)
			if !ok {
				continue
			}
			if seen[cfg.Name] {
				dc.errf(it, p+".name", "configuration name %q is used more than once", cfg.Name)
			}
			seen[cfg.Name] = true
			s.Configurations = append(s.Configurations, cfg)
		}
	}
	listed := map[string]bool{}
	if v := m.get("tasks"); v != nil && !isNull(v) {
		list, _ := dc.uniqueStrList(v, "tasks")
		items := resolve(v).Content
		for i, slug := range list {
			listed[slug] = true
			p, exists := b.tasks[slug]
			if !exists {
				dc.errf(items[i], indexPath("tasks", i), "task %q has no file benchmarks/%s/tasks/%s.yaml", slug, s.Slug, slug)
				continue
			}
			if task, ok := c.parseTask(p, slug); ok {
				s.Tasks = append(s.Tasks, task)
			}
		}
	}
	var unlisted []string
	for slug := range b.tasks {
		if !listed[slug] {
			unlisted = append(unlisted, slug)
		}
	}
	sort.Strings(unlisted)
	for _, slug := range unlisted {
		c.d.add(b.tasks[slug], 0, "", "task file is not listed in benchmarks/%s/suite.yaml tasks", s.Slug)
	}
	return s, len(c.d.errs) == before
}

func (c *compiler) parseBenchConfig(dc *doc, n *yaml.Node, p string) (core.BenchmarkConfiguration, bool) {
	var cfg core.BenchmarkConfiguration
	m := dc.mapping(n, p)
	if m == nil {
		return cfg, false
	}
	defer m.finish()
	if v := m.require("name"); v != nil {
		cfg.Name, _ = dc.nonEmptyStr(v, p+".name")
	}
	if v := m.get("machine"); v != nil && !isNull(v) {
		if r, ok := dc.nonEmptyStr(v, p+".machine"); ok {
			c.machineRef(dc, r, p+".machine", dc.line(v))
			cfg.Machine = r
		}
	}
	if v := m.get("agents"); v != nil && !isNull(v) {
		seen := map[string]bool{}
		for i, it := range dc.seq(v, p+".agents") {
			ap := indexPath(p+".agents", i)
			am := dc.mapping(it, ap)
			if am == nil {
				continue
			}
			pin := core.BenchmarkAgentPin{}
			if av := am.require("agent"); av != nil {
				if name, ok := dc.nonEmptyStr(av, ap+".agent"); ok {
					if _, exists := c.t.agents[name]; !exists {
						dc.errf(av, ap+".agent", "agent %q does not match any agents/<name>/agent.md", name)
					}
					if seen[name] {
						dc.errf(av, ap+".agent", "agent %q is listed more than once in this configuration", name)
					}
					seen[name] = true
					pin.Agent = name
				}
			}
			if am.present("model") == am.present("harness") {
				dc.errf(am.node, ap, "%s must set exactly one of model or harness", ap)
			}
			e := dc.parseExec(&mapping{c: dc, node: am.node, path: ap, order: nil, vals: pick(am, "model", "harness"), keys: am.keys, used: map[string]bool{}}, true, false)
			if e.Model != nil {
				pin.Model = *e.Model
			}
			if h, ok := toHarness(e.Harness, nil); ok && e.Harness != nil {
				pin.Harness = &h
				if a := c.agents[pin.Agent]; a != nil && h.Auth != nil && h.Auth.Source == "workerEnvironment" && a.WorkerHost == "" {
					dc.errf(e.Harness.authNode, ap+".harness.auth.source", "auth.source workerEnvironment requires agent %q to use a self-hosted workerHost", pin.Agent)
				}
			}
			am.finish()
			cfg.Agents = append(cfg.Agents, pin)
		}
	}
	return cfg, true
}

// pick consumes keys from m and returns them as a value map for a sub-mapping.
func pick(m *mapping, keys ...string) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	for _, k := range keys {
		if v := m.get(k); v != nil {
			out[k] = v
		}
	}
	return out
}

func (c *compiler) parseTask(file, slug string) (core.BenchmarkTask, bool) {
	before := len(c.d.errs)
	t := core.BenchmarkTask{Slug: slug}
	dc := &doc{d: c.d, file: file}
	root := dc.parse(c.t.files[file])
	if root == nil {
		return t, false
	}
	m := dc.mapping(root, "")
	if m == nil {
		return t, false
	}
	defer m.finish()
	if v := m.require("title"); v != nil {
		t.Title, _ = dc.nonEmptyStr(v, "title")
	}
	if v := m.require("prompt"); v != nil {
		t.Prompt, _ = dc.nonEmptyStr(v, "prompt")
	}
	if v := m.require("successCriteria"); v != nil {
		t.SuccessCriteria, _ = dc.nonEmptyStr(v, "successCriteria")
	}
	if v := m.get("sourceRunId"); v != nil && !isNull(v) {
		t.SourceRunID, _ = dc.nonEmptyStr(v, "sourceRunId")
	}
	if v := m.get("tags"); v != nil && !isNull(v) {
		t.Tags, _ = dc.strList(v, "tags")
	}
	if v := m.get("trackerSeed"); v != nil && !isNull(v) {
		t.TrackerSeed = toValue(v)
	}
	if v := m.get("linearSeed"); v != nil && !isNull(v) {
		if t.TrackerSeed != nil {
			dc.errf(v, "linearSeed", "set trackerSeed or linearSeed, not both (linearSeed is the older name)")
		} else {
			t.TrackerSeed = toValue(v)
		}
	}
	if v := m.get("startingRepoRefs"); v != nil && !isNull(v) {
		for i, it := range dc.seq(v, "startingRepoRefs") {
			if ref, ok := parseRepoAtCommit(dc, it, indexPath("startingRepoRefs", i)); ok {
				t.StartingRepoRefs = append(t.StartingRepoRefs, ref)
			}
		}
	}
	return t, len(c.d.errs) == before
}

// ParseRepoAtCommit parses "github.com:OWNER/REPO@SHA" or "gitlab.com:OWNER/REPO@SHA".
func ParseRepoAtCommit(s string) (core.RepoAtCommit, bool) {
	host, rest, ok := strings.Cut(s, ":")
	if !ok {
		return core.RepoAtCommit{}, false
	}
	var forge string
	switch host {
	case "github.com":
		forge = "GITHUB"
	case "gitlab.com":
		forge = "GITLAB"
	default:
		return core.RepoAtCommit{}, false
	}
	repoPath, sha, ok := strings.Cut(rest, "@")
	if !ok || !shaRe.MatchString(sha) {
		return core.RepoAtCommit{}, false
	}
	i := strings.LastIndex(repoPath, "/")
	if i <= 0 || i == len(repoPath)-1 {
		return core.RepoAtCommit{}, false
	}
	owner, repo := repoPath[:i], repoPath[i+1:]
	if forge == "GITHUB" && strings.Contains(owner, "/") {
		return core.RepoAtCommit{}, false
	}
	return core.RepoAtCommit{CodeForge: forge, Owner: owner, Repo: repo, Ref: sha}, true
}

func parseRepoAtCommit(dc *doc, n *yaml.Node, p string) (core.RepoAtCommit, bool) {
	if r := resolve(n); r != nil && r.Kind == yaml.ScalarNode {
		s, ok := dc.nonEmptyStr(n, p)
		if !ok {
			return core.RepoAtCommit{}, false
		}
		ref, ok := ParseRepoAtCommit(s)
		if !ok {
			dc.errf(n, p, "%s %q must have the form github.com:OWNER/REPO@COMMIT_SHA or gitlab.com:OWNER/REPO@COMMIT_SHA with a full 40-character commit SHA", p, s)
		}
		return ref, ok
	}
	m := dc.mapping(n, p)
	if m == nil {
		return core.RepoAtCommit{}, false
	}
	defer m.finish()
	var ref core.RepoAtCommit
	ok := true
	if v := m.require("codeForge"); v != nil {
		ref.CodeForge, _ = dc.enum(v, p+".codeForge", "GITHUB", "GITLAB")
	} else {
		ok = false
	}
	if v := m.require("owner"); v != nil {
		ref.Owner, _ = dc.nonEmptyStr(v, p+".owner")
	} else {
		ok = false
	}
	if v := m.require("repo"); v != nil {
		ref.Repo, _ = dc.nonEmptyStr(v, p+".repo")
	} else {
		ok = false
	}
	if v := m.require("ref"); v != nil {
		if s, good := dc.nonEmptyStr(v, p+".ref"); good {
			if !shaRe.MatchString(s) {
				dc.errf(v, p+".ref", "%s.ref must be a full 40-character lowercase commit SHA, not a branch or tag", p)
				ok = false
			}
			ref.Ref = s
		}
	} else {
		ok = false
	}
	return ref, ok
}
