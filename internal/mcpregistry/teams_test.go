package mcpregistry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

type fakeTeams struct {
	mu    sync.Mutex
	posts []string
	err   error
}

func (f *fakeTeams) PostMessage(_ context.Context, in core.Integration, channel, root, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.posts = append(f.posts, fmt.Sprintf("%s|%s|%s|%s", in.ID, channel, root, text))
	return nil
}

func TestBuiltinTeams(t *testing.T) {
	f := newFixture(t)
	teams := &fakeTeams{}
	f.svc.SetTeamsPoster(teams)
	// The Teams record has no secret: the bot signs in with the app
	// credentials of the Teams handler.
	_ = f.store.UpsertIntegration(ctx, &core.Integration{ID: "int_teams", TeamID: "team1", Provider: "microsoft_teams",
		Config: map[string]string{"channel_ids": "19:a@thread.tacv2, 19:b@thread.tacv2"}})
	if err := f.store.CreateFactory(ctx, &core.Factory{ID: "fac1", TeamID: "team1", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	// factory.yaml writes the integration type as microsoft-teams.
	if err := f.store.SetFactoryIntegrations(ctx, "fac1", []string{"microsoft-teams"}, nil); err != nil {
		t.Fatal(err)
	}
	connect := func(run *core.Run) *mcp.ClientSession {
		t.Helper()
		srv, err := f.svc.Resolve(ctx, run, "builtin/teams")
		if err != nil {
			t.Fatal(err)
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: headerRT{h: f.authHeaders(run.ID)}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		return sess
	}
	toolNames := func(s *mcp.ClientSession) map[string]bool {
		tools, err := s.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, tl := range tools.Tools {
			out[tl.Name] = true
		}
		return out
	}

	ro := connect(f.run(map[string]core.MCPServer{"teams": {ManagedID: "builtin/teams"}}))
	if n := toolNames(ro); !n["teams_list_channels"] || n["teams_post_message"] {
		t.Fatalf("read-only tools: %v", n)
	}
	res, err := ro.CallTool(ctx, &mcp.CallToolParams{Name: "teams_list_channels"})
	if err != nil || res.IsError {
		t.Fatalf("list channels: %+v %v", res, err)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "19:a@thread.tacv2") || !strings.Contains(txt, "19:b@thread.tacv2") || !strings.Contains(txt, `"limited": true`) {
		t.Fatalf("channels: %s", txt)
	}

	rw := connect(f.run(map[string]core.MCPServer{"teams": {ManagedID: "builtin/teams", AllowWrite: true}}))
	if n := toolNames(rw); !n["teams_post_message"] {
		t.Fatalf("write tools: %v", n)
	}
	res, err = rw.CallTool(ctx, &mcp.CallToolParams{Name: "teams_post_message", Arguments: map[string]any{"channel": "19:a@thread.tacv2", "text": "Deployed", "thread": "1700"}})
	if err != nil || res.IsError {
		t.Fatalf("post: %+v %v", res, err)
	}
	if fmt.Sprint(teams.posts) != "[int_teams|19:a@thread.tacv2|1700|Deployed]" {
		t.Fatalf("posts = %v", teams.posts)
	}
	// Missing text is a tool error, and nothing is posted.
	res, err = rw.CallTool(ctx, &mcp.CallToolParams{Name: "teams_post_message", Arguments: map[string]any{"channel": "19:a@thread.tacv2", "text": " "}})
	if err != nil || !res.IsError || len(teams.posts) != 1 {
		t.Fatalf("empty text: %+v %v %v", res, err, teams.posts)
	}
	// A refusal of the poster is a tool error.
	teams.err = fmt.Errorf("%w: channel is not allowed", core.ErrForbidden)
	res, err = rw.CallTool(ctx, &mcp.CallToolParams{Name: "teams_post_message", Arguments: map[string]any{"channel": "19:z@thread.tacv2", "text": "x"}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not allowed") {
		t.Fatalf("refused post: %+v %v", res, err)
	}

	// A factory that does not attach Teams gets no server.
	if err := f.store.SetFactoryIntegrations(ctx, "fac1", []string{"slack"}, nil); err != nil {
		t.Fatal(err)
	}
	run := f.run(map[string]core.MCPServer{"teams": {ManagedID: "builtin/teams"}})
	srv, err := f.svc.Resolve(ctx, run, "builtin/teams")
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	if sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: headerRT{h: f.authHeaders(run.ID)}}}, nil); err == nil {
		sess.Close()
		t.Fatal("teams served without the integration")
	}
}

func TestBuiltinTeamsProviderNames(t *testing.T) {
	if p, ok := core.BuiltinMCPProvider("builtin/teams"); !ok || p != "teams" {
		t.Fatalf("builtin/teams = %q %v", p, ok)
	}
	for _, a := range []string{"teams", "microsoft-teams", "microsoft_teams", "Microsoft_Teams"} {
		if !core.SameIntegrationProvider(a, "microsoft_teams") {
			t.Errorf("%q is not Microsoft Teams", a)
		}
	}
	if core.SameIntegrationProvider("slack", "teams") || core.BuiltinIntegrationProvider("teams") != "microsoft_teams" || core.BuiltinIntegrationProvider("slack") != "slack" {
		t.Fatal("provider mapping")
	}
}
