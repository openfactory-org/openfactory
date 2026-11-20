package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
)

func TestEnvironmentValidation(t *testing.T) {
	e := newTestEnv(t)
	e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "web"}), 201)
	cases := []struct {
		name   string
		body   map[string]any
		status int
		detail string // substring of the detail
	}{
		{"https repo", map[string]any{"name": "a", "repos": []string{"https://github.com/acme/web"}}, 201, ""},
		{"https repo with .git", map[string]any{"name": "b", "repos": []string{"https://gitlab.example.com/acme/platform/billing.git"}}, 201, ""},
		{"forge repo", map[string]any{"name": "c", "repos": []string{"GITLAB:acme/platform/billing", "acme/api"}}, 201, ""},
		{"repo without owner", map[string]any{"name": "x", "repos": []string{"nope"}}, 400, "repo"},
		{"url without path", map[string]any{"name": "x", "repos": []string{"https://github.com/acme"}}, 400, "repo"},
		{"ftp url", map[string]any{"name": "x", "repos": []string{"ftp://example.com/acme/web"}}, 400, "repo"},
		{"image", map[string]any{"name": "d", "image": "registry.example.com:5000/team/tools:1.2"}, 201, ""},
		{"image with digest", map[string]any{"name": "e", "image": "node@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, 201, ""},
		{"image with space", map[string]any{"name": "x", "image": "node 22"}, 400, "image"},
		{"image uppercase path", map[string]any{"name": "x", "image": "Node:22"}, 400, "image"},
		{"image bad tag", map[string]any{"name": "x", "image": "node:"}, 400, "image"},
		{"env var", map[string]any{"name": "f", "env_vars": map[string]string{"NODE_ENV": "test", "_x1": "y"}}, 201, ""},
		{"env var with dash", map[string]any{"name": "x", "env_vars": map[string]string{"NODE-ENV": "test"}}, 400, "NODE-ENV"},
		{"env var with digit first", map[string]any{"name": "x", "env_vars": map[string]string{"1A": "b"}}, 400, "1A"},
		{"reserved env var", map[string]any{"name": "x", "env_vars": map[string]string{"OF_METADATA_TEAM": "b"}}, 400, "reserved"},
		{"reserved env var lowercase", map[string]any{"name": "x", "env_vars": map[string]string{"of_run_id": "b"}}, 400, "reserved"},
		{"duplicate name", map[string]any{"name": "web"}, 409, `an environment named "web" already exists`},
		{"duplicate name other case", map[string]any{"name": " WEB "}, 409, "already exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.do("POST", "/environments", e.editor.Key, c.body)
			if r.Status != c.status || !strings.Contains(str(r.Body, "detail"), c.detail) {
				t.Fatalf("status %d, body %s; want %d with %q", r.Status, r.Raw, c.status, c.detail)
			}
		})
	}

	// PATCH validates the same fields and refuses a taken name.
	other := e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "other"}), 201)
	id := str(other.Body, "id")
	e.must(e.do("PATCH", "/environments/"+id, e.editor.Key, map[string]any{"image": "not an image"}), 400)
	e.must(e.do("PATCH", "/environments/"+id, e.editor.Key, map[string]any{"env_vars": map[string]string{"OF_X": "1"}}), 400)
	e.must(e.do("PATCH", "/environments/"+id, e.editor.Key, map[string]any{"name": "web"}), 409)
	e.must(e.do("PATCH", "/environments/"+id, e.editor.Key, map[string]any{"name": "other", "repos": []string{"https://github.com/acme/web"}}), 200)
}

func TestDeleteReferencedEnvironmentAndMCPServer(t *testing.T) {
	var envID string
	e := newTestEnv(t, func(o *Options) {
		o.Compile = func(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
			def, errs := apitest.Compile(files)
			if def != nil && envID != "" {
				for i := range def.Agents {
					if def.Agents[i].Type == core.AgentForeman {
						def.Agents[i].EnvironmentID = envID
						def.Agents[i].MCPServers = map[string]core.MCPServer{"docs": {ManagedID: "docs"}}
					}
				}
			}
			return def, errs
		}
	})
	env := e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "qa-env"}), 201)
	envID = str(env.Body, "id")
	free := e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "free"}), 201)
	mcp := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "docs", "command": "docs-mcp"}), 201)
	e.createFactory("qa-1")

	r := e.must(e.do("DELETE", "/environments/"+envID, e.editor.Key, nil), 409)
	if d := str(r.Body, "detail"); d != "Environment qa-env is used by agent foreman of factory qa-1. Remove the reference first." {
		t.Fatalf("detail = %q", d)
	}
	e.must(e.do("GET", "/environments/"+envID, e.viewer.Key, nil), 200)
	r = e.must(e.do("DELETE", "/mcp-servers/"+str(mcp.Body, "id"), e.editor.Key, nil), 409)
	if d := str(r.Body, "detail"); d != "MCP server docs is used by agent foreman of factory qa-1. Remove the reference first." {
		t.Fatalf("detail = %q", d)
	}
	e.must(e.do("DELETE", "/environments/"+str(free.Body, "id"), e.editor.Key, nil), 204)

	// Without the reference the delete succeeds.
	envID = ""
	e.must(e.do("DELETE", "/environments/"+str(env.Body, "id"), e.editor.Key, nil), 204)
	e.must(e.do("DELETE", "/mcp-servers/docs", e.editor.Key, nil), 204)
}

func TestResourceUses(t *testing.T) {
	envOverride, other := "env_1", "env_2"
	def := &core.CompiledDefinition{
		MCPServers: map[string]core.MCPServer{"wiki": {ManagedID: "mcp_wiki"}},
		Agents: []core.AgentConfig{
			{Name: "foreman", EnvironmentID: "env_1", MCPServers: map[string]core.MCPServer{"wiki": {ManagedID: "mcp_wiki"}}},
			{Name: "coder", MCPServers: map[string]core.MCPServer{"wiki": {ManagedID: "mcp_wiki"}, "docs": {ManagedID: "docs"}}},
		},
		Automations: []core.Automation{
			{Name: "nightly", Overrides: &core.AgentOverrides{EnvironmentID: &envOverride}},
			{Name: "weekly", Overrides: &core.AgentOverrides{EnvironmentID: &other, MCPServers: map[string]core.MCPServer{"d": {ManagedID: "mcp_docs"}}}},
		},
		Scorers:        []core.Scorer{{Slug: "quality", MCPServers: map[string]core.MCPServer{"d": {ManagedID: "docs"}}}},
		ScorerDefaults: &core.AgentOverrides{EnvironmentID: &envOverride},
	}
	cases := []struct {
		name string
		got  []string
		want string
	}{
		{"environment", environmentUses(def, "env_1"), "agent foreman, automation nightly, scorerDefaults"},
		{"unused environment", environmentUses(def, "env_9"), ""},
		{"factory-wide server once", mcpServerUses(def, "mcp_wiki", "wiki"), "the factory-wide mcpServers"},
		{"server by name or id", mcpServerUses(def, "mcp_docs", "docs"), "agent coder, automation weekly, scorer quality"},
		{"unused server", mcpServerUses(def, "mcp_x", "x"), ""},
	}
	for _, c := range cases {
		if got := strings.Join(c.got, ", "); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	if err := inUseError("Environment", "x", nil); err != nil {
		t.Fatalf("no uses: %v", err)
	}
	many := []string{"a", "b", "c", "d", "e", "f", "g"}
	var ae *Error
	err := inUseError("Environment", "x", many)
	if want := "Environment x is used by a, b, c, d, e and 2 more. Remove the references first."; !errors.As(err, &ae) || ae.Detail != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
