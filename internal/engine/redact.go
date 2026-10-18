package engine

import (
	"context"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/redact"
)

// RedactFor returns a function that removes the secret values of a run from
// text. The orchestration server uses it for the text that an agent posts
// with post_update, ask_human and request_approval. It rebuilds the secret
// values of the agent, the harness credentials and the MCP servers of the
// run. It never returns nil.
func (x *Planner) RedactFor(ctx context.Context, r *core.Run) func(string) string {
	red := redact.New()
	if env, err := x.runEnv(ctx, r); err == nil {
		red.Add(secretValues(r, env, nil, nil)...)
	}
	if auth, err := x.harnessAuth(ctx, r); err == nil {
		red.Add(secretValues(r, nil, auth, nil)...)
	}
	if mcp, err := x.mcpServers(ctx, r); err == nil {
		red.Add(secretValues(r, nil, nil, mcp)...)
	}
	return red.String
}

// secretValues lists the values that the run must not show in its output.
func secretValues(r *core.Run, env, auth map[string]string, mcp map[string]core.MCPServer) []string {
	var out []string
	for _, n := range r.Config.Secrets {
		out = append(out, env[n])
	}
	for k, v := range auth {
		// A base URL is not a secret, and it would match ordinary text.
		if !strings.HasSuffix(strings.ToUpper(k), "_BASE_URL") {
			out = append(out, v)
		}
	}
	for _, s := range mcp {
		for _, v := range s.Headers {
			out = append(out, headerSecrets(v)...)
		}
		for _, v := range s.Env {
			out = append(out, v)
		}
	}
	return out
}

// headerSecrets returns a header value and, for a value with an
// authentication scheme such as "Bearer <token>", the token alone.
func headerSecrets(v string) []string {
	out := []string{v}
	if scheme, rest, ok := strings.Cut(v, " "); ok && rest != "" {
		switch strings.ToLower(scheme) {
		case "bearer", "basic", "token":
			out = append(out, strings.TrimSpace(rest))
		}
	}
	return out
}
