package teams

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// NameResolver implements core.NameResolver for Microsoft Teams: it maps
// channel names to channel IDs (channel_ids) and user names or e-mail
// addresses to the Microsoft Entra object IDs of the users (user_ids). It
// lists the channels and the members of each team that the team has
// connected, with Microsoft Graph and the bot of the record. It keeps the
// lists in memory and uses the rules of integrations.NameLists (cache time,
// one extra read for a missing name, back-off after an error, stale lists).
//
// The bot needs the resource permissions ChannelSettings.Read.Group (channels)
// and TeamMember.Read.Group (users) in the team, and the record needs
// tenant_id.
type NameResolver struct {
	h     *Handler
	lists integrations.NameLists
}

var _ core.NameResolver = (*NameResolver)(nil)

// NewNameResolver creates the resolver. It uses the credentials, the Graph
// endpoint and the HTTP client of the handler.
func NewNameResolver(h *Handler, now func() time.Time) *NameResolver {
	r := &NameResolver{h: h}
	r.lists.Now = now
	return r
}

// ResolveNames implements core.NameResolver. kind is channels or users. A
// channel name ignores a leading "#". A user is a display name or an e-mail
// address. The scope key team_ids limits the lookup to those Microsoft Graph
// teams. Without it, the result holds the objects of all connected teams, so a
// name that exists in two teams gives two IDs. An error means that the names
// cannot be resolved now.
func (r *NameResolver) ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, scope map[string][]string) (map[string][]string, error) {
	if !strings.EqualFold(provider, Provider) && !strings.EqualFold(provider, IntegrationProvider) {
		return nil, fmt.Errorf("the Teams resolver cannot list %s objects", provider)
	}
	if kind != "channels" && kind != "users" {
		return nil, fmt.Errorf("the Teams resolver cannot resolve %q", kind)
	}
	if r.h.opts.Store == nil {
		return nil, errors.New("no store configured")
	}
	all, err := forgeutil.TeamIntegrations(ctx, r.h.opts.Store, teamID, IntegrationProvider)
	if err != nil {
		return nil, fmt.Errorf("could not read the Teams integrations: %w", err)
	}
	var ints []core.Integration
	for _, in := range all {
		if in.Active() && in.Config["team_id"] != "" && inScope(scope["team_ids"], in.Config["team_id"]) {
			ints = append(ints, in)
		}
	}
	if len(ints) == 0 {
		return nil, errors.New("the team has no active Teams integration for these Microsoft teams")
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = strings.ToLower(strings.TrimSpace(n))
		if kind == "channels" {
			keys[i] = strings.TrimSpace(strings.TrimPrefix(keys[i], "#"))
		}
	}
	out := make(map[string][]string, len(names))
	var firstErr error
	ok := 0
	for _, in := range ints {
		byName, err := r.lookup(ctx, in, kind, keys)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		for i, n := range names {
			for _, id := range byName[keys[i]] {
				integrations.AddID(out, n, id)
			}
		}
	}
	if ok == 0 {
		return nil, firstErr
	}
	integrations.SortIDs(out)
	return out, nil
}

func inScope(teams []string, id string) bool {
	if len(teams) == 0 {
		return true
	}
	for _, t := range teams {
		if strings.EqualFold(t, id) {
			return true
		}
	}
	return false
}

func (r *NameResolver) lookup(ctx context.Context, in core.Integration, kind string, keys []string) (map[string][]string, error) {
	graphTeam := in.Config["team_id"]
	return r.lists.Lookup(ctx, in.ID+"/"+kind+"/"+graphTeam, keys, func(ctx context.Context) (map[string][]string, error) {
		c, err := r.h.graphClient(ctx, &in, "")
		if err != nil {
			return nil, err
		}
		base := "/teams/" + graphTeam
		out := map[string][]string{}
		if kind == "channels" {
			err = r.h.graphPages(ctx, c, base+"/channels?$select=id,displayName", func(items []map[string]any) bool {
				for _, ch := range items {
					integrations.AddName(out, str(ch, "displayName"), str(ch, "id"))
				}
				return true
			})
			return out, describeGraphError("list the channels", "ChannelSettings.Read.Group", err)
		}
		err = r.h.graphPages(ctx, c, base+"/members?$top=999", func(items []map[string]any) bool {
			for _, m := range items {
				id := str(m, "userId")
				integrations.AddName(out, str(m, "displayName"), id)
				integrations.AddName(out, str(m, "email"), id)
			}
			return true
		})
		return out, describeGraphError("list the members", "TeamMember.Read.Group", err)
	})
}

// NameResolver returns the name resolver of the handler.
func (h *Handler) NameResolver() *NameResolver { return NewNameResolver(h, h.opts.Now) }
