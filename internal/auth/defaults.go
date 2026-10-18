package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ---------- RoleAuthorizer ----------

// RoleAuthorizer is the free-core Authorizer: user may read and operate,
// editor may also write, admin may do everything. For a request about a
// factory, the role override of the user for that factory replaces the
// team-wide role. A resource owned by another team is always forbidden.
type RoleAuthorizer struct{}

var _ Authorizer = RoleAuthorizer{}

// Allow implements Authorizer.
func (RoleAuthorizer) Allow(_ context.Context, p *Principal, a Action, res Resource) error {
	if p == nil || p.User == nil {
		return fmt.Errorf("%w: authentication required", core.ErrUnauthorized)
	}
	if res.TeamID != "" && res.TeamID != p.TeamID {
		return fmt.Errorf("%w: resource belongs to another team", core.ErrForbidden)
	}
	need, ok := categoryRole[a.Category()]
	if !ok {
		return fmt.Errorf("%w: unknown action %q", core.ErrForbidden, a)
	}
	have := p.RoleFor(res.FactoryID)
	if RoleRank(have) < RoleRank(need) {
		if res.FactoryID != "" {
			return fmt.Errorf("%w: %s requires the %s role; your role for this factory is %s", core.ErrForbidden, a, need, have)
		}
		return fmt.Errorf("%w: %s requires the %s role; your role is %s", core.ErrForbidden, a, need, have)
	}
	return nil
}

var categoryRole = map[Action]core.Role{
	ActionRead:    core.RoleUser,
	ActionOperate: core.RoleUser,
	ActionWrite:   core.RoleEditor,
	ActionAdmin:   core.RoleAdmin,
}

// RoleRank orders the built-in roles (0 for unknown roles).
func RoleRank(r core.Role) int { return core.RoleRank(r) }

// ValidRole reports whether r is a built-in role.
func ValidRole(r core.Role) bool { return RoleRank(r) > 0 }

// ---------- SlogAuditSink ----------

// SlogAuditSink writes audit events to a structured logger. It is the
// free-core sink: no export, no retention.
type SlogAuditSink struct {
	Logger *slog.Logger // nil => slog.Default()
}

var _ AuditSink = SlogAuditSink{}

// Record implements AuditSink.
func (s SlogAuditSink) Record(ctx context.Context, e AuditEvent) {
	l := s.Logger
	if l == nil {
		l = slog.Default()
	}
	attrs := []any{
		"audit", true, "action", e.Action, "outcome", e.Outcome,
	}
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, k, v)
		}
	}
	add("actor_id", e.ActorID)
	add("user_id", e.UserID)
	add("team_id", e.TeamID)
	add("provider", e.Provider)
	add("remote_addr", e.RemoteAddr)
	add("user_agent", e.UserAgent)
	add("reason", e.Reason)
	if e.Resource != nil {
		add("resource_type", e.Resource.Type)
		add("resource_id", e.Resource.ID)
	}
	for k, v := range e.Detail {
		add("detail."+k, v)
	}
	level := slog.LevelInfo
	if e.Outcome == OutcomeFailure {
		level = slog.LevelWarn
	}
	l.Log(ctx, level, "audit: "+e.Action, attrs...)
}

// ---------- SingleTeamResolver ----------

// SingleTeamResolver is the free-core TenantResolver: one team per install.
// It returns the first team in the store, creating it when the store has none
// and Create is true.
type SingleTeamResolver struct {
	Store    core.Store
	TeamName string // name of the created team (default "Default")
	Create   bool
	Now      func() time.Time

	mu sync.Mutex
}

var _ TenantResolver = (*SingleTeamResolver)(nil)

// ResolveTeam implements TenantResolver.
func (t *SingleTeamResolver) ResolveTeam(ctx context.Context, _ *http.Request, _ Identity) (*core.Team, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	teams, err := t.Store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	if len(teams) > 0 {
		team := teams[0]
		return &team, nil
	}
	if !t.Create {
		return nil, fmt.Errorf("%w: no team exists on this install", core.ErrNotFound)
	}
	name := t.TeamName
	if name == "" {
		name = "Default"
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	team := &core.Team{ID: core.NewID("team"), Name: name, CreatedAt: now().UTC()}
	if err := t.Store.CreateTeam(ctx, team); err != nil {
		return nil, err
	}
	return team, nil
}
