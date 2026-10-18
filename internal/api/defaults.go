package api

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
)

// DefaultAgentNames are the optional default agents a new factory can include
// besides the foreman (which is always created).
var DefaultAgentNames = []string{"triage", "spec", "implement", "review"}

// DefaultDefinitionRequest describes the factory a default definition is generated for.
type DefaultDefinitionRequest struct {
	Name         string                 `json:"name"`
	Alias        string                 `json:"alias,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Repositories []core.RepoRef         `json:"repositories"`
	Integrations []core.IntegrationDecl `json:"integrations,omitempty"`
	Agents       []string               `json:"agents"`            // subset of DefaultAgentNames
	Model        string                 `json:"model,omitempty"`   // agentDefaults.model (default: the default claude model)
	Harness      *core.HarnessConfig    `json:"harness,omitempty"` // alternative to Model
}

type defaultAgent struct {
	Type         core.AgentType
	Description  string
	Instructions string
}

var defaultAgents = map[string]defaultAgent{
	"foreman": {core.AgentForeman, "Coordinates each work item and communicates with people",
		`You are the foreman of this factory. You receive every work item, decide which
stages it needs, and coordinate the other agents with the run_agents tool.

- Start by understanding the request and its source (issue, pull request, chat thread, webhook).
- Use the triage agent to scope unclear requests, the spec agent to write a plan for
  non-trivial changes, the implement agent to make changes, and the review agent to check them.
- Ask a human with ask_human when a decision is required; request approval of specs with
  request_approval before implementation starts.
- Keep the work item's stage current with set_stage and post short progress updates.
- Link every pull request you open with link_pull_request and hand the result back where the
  work started. Complete the work item when nothing remains.`},
	"triage": {core.AgentTriage, "Scopes incoming requests and gathers context",
		`Triage the request. Identify the affected repositories, components and owners,
reproduce the problem when possible, and report the scope, risks, open questions and a
recommended next stage. Do not modify code.`},
	"spec": {core.AgentSpec, "Writes implementation plans for approval",
		`Write a concise technical specification for the requested change: goals, non-goals,
the proposed approach, affected files, test plan and rollout notes. Highlight decisions that
need a human. Do not implement the change.`},
	"implement": {core.AgentImplement, "Implements changes and opens pull requests",
		`Implement the requested change following the approved specification and the
repository's conventions. Add or update tests, run the relevant checks, push a branch and
open a pull request that explains what changed and how it was validated.`},
	"review": {core.AgentReview, "Reviews pull requests produced by the factory",
		`Review the pull request against the specification and the repository's standards.
Check correctness, tests, security and maintainability. Request changes with specific,
actionable comments; never approve your own edits.`},
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,60}$`)

// ValidAlias reports whether s is a valid foreman alias (≤60 letters, digits, spaces, ., _, -).
func ValidAlias(s string) bool { return aliasRe.MatchString(s) && strings.TrimSpace(s) != "" }

// AliasFromName derives a default alias from a factory name.
func AliasFromName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	a := strings.Trim(b.String(), "-")
	if len(a) > 60 {
		a = a[:60]
	}
	if a == "" {
		a = "factory"
	}
	return a
}

// GenerateDefaultDefinition builds a valid v1 definition tree with a
// foreman plus the selected default agents.
func GenerateDefaultDefinition(req DefaultDefinitionRequest) map[string]string {
	files := map[string]string{}
	root := yamlMap()
	yamlSet(root, "schemaVersion", "v1")
	yamlSet(root, "name", req.Name)
	if req.Alias != "" {
		yamlSet(root, "alias", req.Alias)
	}
	if req.Description != "" {
		yamlSet(root, "description", req.Description)
	}
	var forges []string
	repos := make([]any, 0, len(req.Repositories))
	for _, r := range req.Repositories {
		forge := strings.ToUpper(r.CodeForge)
		if forge == "" {
			forge = "GITHUB"
		}
		if !slices.Contains(forges, forge) {
			forges = append(forges, forge)
		}
		m := yamlMap()
		yamlSet(m, "codeForge", forge)
		yamlSet(m, "owner", r.Owner)
		yamlSet(m, "name", r.Name)
		repos = append(repos, m)
	}
	if len(forges) > 0 {
		yamlSet(root, "codeForges", forges)
	}
	yamlSet(root, "repositories", repos)
	if len(req.Integrations) > 0 {
		var ints []any
		for _, in := range req.Integrations {
			m := yamlMap()
			yamlSet(m, "type", in.Type)
			if len(in.Settings) > 0 {
				sm := yamlMap()
				keys := make([]string, 0, len(in.Settings))
				for k := range in.Settings {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				for _, k := range keys {
					yamlSet(sm, k, scalarValue(in.Settings[k]))
				}
				yamlSet(m, in.Type, sm)
			}
			ints = append(ints, m)
		}
		yamlSet(root, "integrations", ints)
	}
	defs := yamlMap()
	if req.Harness != nil && req.Harness.Type != "" {
		h := yamlMap()
		yamlSet(h, "type", string(req.Harness.Type))
		model := req.Harness.Model
		if model == "" {
			model = harness.DefaultModel(req.Harness.Type)
		}
		yamlSet(h, "model", model)
		if req.Harness.ReasoningLevel != "" {
			yamlSet(h, "reasoningLevel", req.Harness.ReasoningLevel)
		}
		if req.Harness.Auth != nil && req.Harness.Auth.Source != "" {
			a := yamlMap()
			yamlSet(a, "source", req.Harness.Auth.Source)
			if req.Harness.Auth.SecretName != "" {
				yamlSet(a, "secretName", req.Harness.Auth.SecretName)
			}
			yamlSet(h, "auth", a)
		}
		yamlSet(defs, "harness", h)
	} else {
		model := req.Model
		if model == "" {
			model = harness.DefaultModel(core.HarnessClaude)
		}
		yamlSet(defs, "model", model)
	}
	yamlSet(root, "agentDefaults", defs)
	files["factory.yaml"] = encodeYAMLNode(root)

	names := append([]string{"foreman"}, req.Agents...)
	for _, n := range names {
		a, ok := defaultAgents[n]
		if !ok {
			continue
		}
		files["agents/"+n+"/agent.md"] = fmt.Sprintf("---\ndescription: %s\nagentType: %s\n---\n\n%s\n", a.Description, a.Type, a.Instructions)
	}
	return files
}

func scalarValue(s string) any {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	return s
}

// ---------- yaml.Node helpers (comment-preserving edits) ----------

func yamlMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

func toNode(v any) *yaml.Node {
	if n, ok := v.(*yaml.Node); ok {
		return n
	}
	switch t := v.(type) {
	case []any:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range t {
			seq.Content = append(seq.Content, toNode(e))
		}
		return seq
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(v)}
	}
	return &n
}

// yamlSet sets (or replaces) key in mapping node m. A nil value removes the key.
func yamlSet(m *yaml.Node, key string, v any) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if v == nil {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				return
			}
			m.Content[i+1] = toNode(v)
			return
		}
	}
	if v == nil {
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		toNode(v))
}

func encodeYAMLNode(n *yaml.Node) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(n)
	_ = enc.Close()
	return buf.String()
}

// editYAMLDocument parses doc, applies set to its root mapping and re-encodes it,
// preserving comments and key order.
func editYAMLDocument(doc string, set map[string]any) (string, error) {
	var root yaml.Node
	if strings.TrimSpace(doc) != "" {
		if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
			return "", errInvalid("cannot parse YAML: %v", err)
		}
	}
	var m *yaml.Node
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 && root.Content[0].Kind == yaml.MappingNode {
		m = root.Content[0]
	} else if root.Kind == 0 {
		m = yamlMap()
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{m}}
	} else {
		return "", errInvalid("YAML document root must be a mapping")
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		yamlSet(m, k, set[k])
	}
	return encodeYAMLNode(&root), nil
}
