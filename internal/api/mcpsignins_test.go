package api

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mcpauth"
)

type fakeSignIns struct{ grants map[string]*mcpauth.Grant }

func (f *fakeSignIns) ListGrants(_ context.Context, userID string) ([]mcpauth.Grant, error) {
	var out []mcpauth.Grant
	for _, g := range f.grants {
		if g.UserID == userID {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (f *fakeSignIns) ListTeamGrants(_ context.Context, teamID string) ([]mcpauth.Grant, error) {
	var out []mcpauth.Grant
	for _, g := range f.grants {
		if g.TeamID == teamID {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (f *fakeSignIns) GetGrant(_ context.Context, id string) (*mcpauth.Grant, error) {
	g, ok := f.grants[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return g, nil
}

func (f *fakeSignIns) Revoke(_ context.Context, id string) error {
	delete(f.grants, id)
	return nil
}

func TestMCPSignIns(t *testing.T) {
	fake := &fakeSignIns{grants: map[string]*mcpauth.Grant{}}
	e := newTestEnv(t, func(o *Options) { o.MCPSignIns = fake })
	now := time.Now().UTC()
	add := func(id, user, team string) {
		fake.grants[id] = &mcpauth.Grant{ID: id, UserID: user, TeamID: team, ClientName: "agent", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	add("g_viewer", e.viewer.User.ID, e.team.ID)
	add("g_editor", e.editor.User.ID, e.team.ID)
	add("g_other", e.other.User.ID, e.other.User.TeamID)

	if l := e.must(e.do("GET", "/mcp-sign-ins", e.viewer.Key, nil), 200); len(list(l.Body, "items")) != 1 {
		t.Fatalf("own sign-ins: %s", l.Raw)
	}
	e.must(e.do("GET", "/mcp-sign-ins?scope=team", e.viewer.Key, nil), 403)
	if l := e.must(e.do("GET", "/mcp-sign-ins?scope=team", e.admin.Key, nil), 200); len(list(l.Body, "items")) != 2 {
		t.Fatalf("team sign-ins: %s", l.Raw)
	}
	e.must(e.do("DELETE", "/mcp-sign-ins/g_editor", e.viewer.Key, nil), 403)
	e.must(e.do("DELETE", "/mcp-sign-ins/g_other", e.admin.Key, nil), 404)
	e.must(e.do("DELETE", "/mcp-sign-ins/g_viewer", e.viewer.Key, nil), 204)
	e.must(e.do("DELETE", "/mcp-sign-ins/g_editor", e.admin.Key, nil), 204)
	if len(fake.grants) != 1 {
		t.Fatalf("left: %v", fake.grants)
	}
}
