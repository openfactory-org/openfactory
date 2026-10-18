package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/integrations/jira"
	"github.com/openfactory-org/openfactory/internal/integrations/linear"
	"github.com/openfactory-org/openfactory/internal/integrations/slack"
	"github.com/openfactory-org/openfactory/internal/integrations/teams"
)

// nameResolvers sends each name lookup of the preflight to the resolver of
// its provider. A provider without a resolver gives an error, and the
// preflight then reports a warning with that reason.
type nameResolvers map[string]core.NameResolver

func (m nameResolvers) ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, scope map[string][]string) (map[string][]string, error) {
	r := m[strings.ToLower(provider)]
	if r == nil {
		return nil, fmt.Errorf("this server cannot list %s objects", provider)
	}
	return r.ResolveNames(ctx, teamID, provider, kind, names, scope)
}

// newNameResolvers returns the name resolvers of the integrations that can
// list their objects: Slack (channels, users), Linear (teams, projects,
// states, issues), Jira (projects, statuses) and Teams (channels, users).
// The Teams resolver reads with the bot of the Teams handler.
func newNameResolvers(st core.Store, secrets core.SecretStore, teamsH *teams.Handler, log *slog.Logger) nameResolvers {
	teamsR := teamsH.NameResolver()
	return nameResolvers{
		slack.Provider:  slack.NewNameResolver(slack.NameResolverOptions{Store: st, Secrets: secrets, Logger: log}),
		linear.Provider: linear.NewNameResolver(linear.NameResolverOptions{Store: st, Secrets: secrets, Logger: log}),
		jira.Provider:   jira.NewNameResolver(jira.NameResolverOptions{Store: st, Secrets: secrets, Logger: log}),
		// Teams triggers are stored with the provider "microsoft_teams".
		teams.Provider:            teamsR,
		teams.IntegrationProvider: teamsR,
	}
}
