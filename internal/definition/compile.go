// Package definition parses, validates and compiles factory definition trees
// (factory.yaml, agents, automations, machines, benchmarks, scorers,
// webhooks and skills), resolves each agent's effective configuration,
// publishes the v1 JSON Schemas, and diffs compiled definitions.
package definition

import (
	"path"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.DefinitionCompiler = Compile

// tree is a definition's files classified by resource kind.
type tree struct {
	factory     *string
	agents      map[string]string // name -> file path
	agentSkills map[string]map[string]*skillDir
	skills      map[string]*skillDir
	automations map[string]string
	machines    map[string]string
	webhooks    map[string]string
	scorers     map[string]string
	benchmarks  map[string]*benchDir
	files       map[string]string // normalized path -> content
}

type skillDir struct {
	dir      string
	skillMD  string // path of SKILL.md ("" if missing)
	supports map[string]string
}

type benchDir struct {
	suite string            // path of suite.yaml ("" if missing)
	tasks map[string]string // slug -> path
}

// NormalizePath cleans a definition path to its canonical slash-separated
// relative form. It returns "" for paths that escape the tree.
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." || p == "" {
		return ""
	}
	return p
}

func classify(d *diag, files map[string]string) *tree {
	t := &tree{
		agents: map[string]string{}, agentSkills: map[string]map[string]*skillDir{}, skills: map[string]*skillDir{},
		automations: map[string]string{}, machines: map[string]string{},
		webhooks: map[string]string{}, scorers: map[string]string{}, benchmarks: map[string]*benchDir{},
		files: map[string]string{},
	}
	paths := make([]string, 0, len(files))
	for p, content := range files {
		np := NormalizePath(p)
		if np == "" {
			d.add(p, 0, "", "invalid path")
			continue
		}
		if _, dup := t.files[np]; dup {
			d.add(np, 0, "", "path is listed more than once")
			continue
		}
		t.files[np] = content
		paths = append(paths, np)
	}
	sort.Strings(paths)

	checkName := func(p, kind, name string) bool {
		if !resourceNameRe.MatchString(name) {
			d.add(p, 0, "", "%s name %q must be 1-64 letters, digits, '.', '_' or '-' and start with a letter or digit", kind, name)
			return false
		}
		return true
	}
	yamlFile := func(p, kind, dir, file string, into map[string]string) {
		switch {
		case strings.HasSuffix(file, ".yaml"):
			name := strings.TrimSuffix(file, ".yaml")
			if checkName(p, kind, name) {
				into[name] = p
			}
		case strings.HasSuffix(file, ".yml"):
			d.add(p, 0, "", "%s files must use the .yaml extension (%s/<name>.yaml)", kind, dir)
		}
	}
	addSkillFile := func(m map[string]*skillDir, dir, name, rel, p string) {
		sd := m[name]
		if sd == nil {
			sd = &skillDir{dir: dir, supports: map[string]string{}}
			m[name] = sd
		}
		if rel == "SKILL.md" {
			sd.skillMD = p
		} else {
			sd.supports[rel] = p
		}
	}

	for _, p := range paths {
		seg := strings.Split(p, "/")
		switch {
		case p == factoryFile:
			c := t.files[p]
			t.factory = &c
		case p == "factory.yml":
			d.add(p, 0, "", "the root document must be named factory.yaml")
		case seg[0] == "agents" && len(seg) == 3 && seg[2] == "agent.md":
			if checkName(p, "agent", seg[1]) {
				t.agents[seg[1]] = p
			}
		case seg[0] == "agents" && len(seg) == 2 && strings.HasSuffix(seg[1], ".md"):
			d.add(p, 0, "", "agent files must be agents/<name>/agent.md")
		case seg[0] == "agents" && len(seg) >= 5 && seg[2] == "skills":
			if checkName(p, "skill", seg[3]) {
				if t.agentSkills[seg[1]] == nil {
					t.agentSkills[seg[1]] = map[string]*skillDir{}
				}
				addSkillFile(t.agentSkills[seg[1]], strings.Join(seg[:4], "/"), seg[3], strings.Join(seg[4:], "/"), p)
			}
		case seg[0] == "skills" && len(seg) >= 3:
			if checkName(p, "skill", seg[1]) {
				addSkillFile(t.skills, strings.Join(seg[:2], "/"), seg[1], strings.Join(seg[2:], "/"), p)
			}
		case seg[0] == "automations" && len(seg) == 3 && seg[2] == "automation.md":
			if checkName(p, "automation", seg[1]) {
				t.automations[seg[1]] = p
			}
		case seg[0] == "automations" && len(seg) == 2 && strings.HasSuffix(seg[1], ".md"):
			d.add(p, 0, "", "automation files must be automations/<name>/automation.md")
		case seg[0] == "machines" && len(seg) == 2:
			yamlFile(p, "machine", "machines", seg[1], t.machines)
		case seg[0] == "webhooks" && len(seg) == 2:
			yamlFile(p, "webhook", "webhooks", seg[1], t.webhooks)
		case seg[0] == "scorers" && len(seg) == 3 && seg[2] == "scorer.md":
			if checkName(p, "scorer", seg[1]) {
				t.scorers[seg[1]] = p
			}
		case seg[0] == "scorers" && len(seg) == 2 && strings.HasSuffix(seg[1], ".md"):
			d.add(p, 0, "", "scorer files must be scorers/<slug>/scorer.md")
		case seg[0] == "benchmarks" && len(seg) == 3 && seg[2] == "suite.yaml":
			if checkName(p, "benchmark suite", seg[1]) {
				t.bench(seg[1]).suite = p
			}
		case seg[0] == "benchmarks" && len(seg) == 4 && seg[2] == "tasks":
			if checkName(p, "benchmark suite", seg[1]) {
				tasks := map[string]string{}
				yamlFile(p, "benchmark task", "benchmarks/<suite>/tasks", seg[3], tasks)
				for k, v := range tasks {
					t.bench(seg[1]).tasks[k] = v
				}
			}
		}
	}
	for agent, skills := range t.agentSkills {
		if _, ok := t.agents[agent]; !ok {
			for _, sd := range skills {
				d.add(sd.dir, 0, "", "skills directory belongs to agent %q, which has no agents/%s/agent.md", agent, agent)
			}
		}
	}
	for slug, b := range t.benchmarks {
		if b.suite == "" {
			d.add("benchmarks/"+slug, 0, "", "benchmark directory has task files but no suite.yaml")
		}
	}
	return t
}

func (t *tree) bench(slug string) *benchDir {
	b := t.benchmarks[slug]
	if b == nil {
		b = &benchDir{tasks: map[string]string{}}
		t.benchmarks[slug] = b
	}
	return b
}

// compiler holds the state of one Compile call.
type compiler struct {
	d       *diag
	t       *tree
	f       *factorySpec
	def     *core.CompiledDefinition
	agents  map[string]*core.AgentConfig
	foreman string
}

// Compile validates and compiles a definition tree (path -> content). On any
// validation error it returns a nil definition and the errors sorted by file
// and line. Agent, automation and factory IDs are left empty.
func Compile(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
	return compile(files)
}

func compile(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
	d := &diag{}
	t := classify(d, files)
	if t.factory == nil {
		d.add(factoryFile, 0, "", "factory.yaml is required at the root of the definition")
		return nil, d.sorted()
	}
	f := parseFactory(d, *t.factory)
	if f == nil {
		return nil, d.sorted()
	}
	c := &compiler{d: d, t: t, f: f, def: f.def, agents: map[string]*core.AgentConfig{}}
	def := c.def
	def.Machines = map[string]core.MachineSpec{}
	def.Agents = []core.AgentConfig{}
	def.Automations = []core.Automation{}
	def.Scorers = []core.Scorer{}
	def.Webhooks = []core.Webhook{}
	def.Benchmarks = []core.BenchmarkSuite{}
	def.Skills = []core.Skill{}

	for _, name := range sortedNames(t.machines) {
		p := t.machines[name]
		if spec, ok := parseMachine(d, p, name, t.files[p]); ok {
			def.Machines[name] = spec
		}
	}
	if len(t.webhooks) > MaxWebhooks {
		d.add("webhooks", 0, "", "a factory can define at most %d webhooks (found %d)", MaxWebhooks, len(t.webhooks))
	}
	for _, name := range sortedNames(t.webhooks) {
		p := t.webhooks[name]
		if w, ok := parseWebhook(d, p, name, t.files[p]); ok {
			def.Webhooks = append(def.Webhooks, w)
		}
	}
	c.checkDefaultMachine()
	c.checkScorerDefaults()

	c.compileAgents()
	c.compileAutomations()
	c.compileSkills()
	c.compileScorers()
	c.compileBenchmarks()

	if len(d.errs) > 0 {
		return nil, d.sorted()
	}
	return def, nil
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// machineRef validates a machine reference and returns its spec.
func (c *compiler) machineRef(dc *doc, name string, field string, line int) *core.MachineSpec {
	if _, declared := c.t.machines[name]; !declared {
		c.d.add(dc.file, line, field, "%s %q does not match any machines/<name>.yaml file", field, name)
		return nil
	}
	spec, ok := c.def.Machines[name]
	if !ok {
		return nil // machine file itself is invalid; reported there
	}
	s := spec
	return &s
}

func (c *compiler) checkDefaultMachine() {
	e := c.f.defaults
	if e.Machine == nil {
		return
	}
	spec := c.machineRef(c.f.c, *e.Machine, "agentDefaults.machine", c.f.c.line(e.machineNode))
	if spec != nil && spec.OS != "linux" {
		c.f.c.errf(e.machineNode, "agentDefaults.machine", "agentDefaults.machine must be a Linux machine because it supplies the factory-managed environment (%s is %s)", *e.Machine, spec.OS)
	}
}

func (c *compiler) checkScorerDefaults() {
	sd := c.f.scorerDefaults
	if sd != nil && sd.machine != "" {
		c.machineRef(c.f.c, sd.machine, "scorerDefaults.machine", c.f.c.line(sd.machineNode))
	}
	// The resolved harness of scorerDefaults is the harness of the judge of
	// the built-in Correctness Scorer of benchmarks.
	if h, ok := c.scorerBaseHarness(); ok {
		if c.def.ScorerDefaults == nil {
			c.def.ScorerDefaults = &core.AgentOverrides{}
		}
		c.def.ScorerDefaults.Harness = &h
	}
}
