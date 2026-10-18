package core

import (
	"context"
	"net/url"
	"strings"
)

// IntegrationWorkspaceKey returns the config key of a chat or tracker
// record that names the one workspace, team or site that the record serves.
// It plays the role of config.org of a forge record:
//
//   - slack: team_id, the Slack workspace ID (T…).
//   - microsoft_teams: team_id, the Microsoft Graph team ID.
//   - linear: organization_id, the Linear workspace ID.
//   - jira: site, the host name of the Jira site (acme.atlassian.net), or
//     the cloud ID of the site for a record that uses the OAuth API address.
//
// It returns "" for the other providers.
func IntegrationWorkspaceKey(provider string) string {
	switch {
	case SameIntegrationProvider(provider, "slack"), SameIntegrationProvider(provider, "microsoft_teams"):
		return "team_id"
	case SameIntegrationProvider(provider, "linear"):
		return "organization_id"
	case SameIntegrationProvider(provider, "jira"):
		return "site"
	}
	return ""
}

// NormalizeWorkspace returns the form of a workspace value that the
// selection rule compares (the case is ignored there). For Jira it is the
// host name: "https://Acme.atlassian.net/browse/X-1" gives
// "acme.atlassian.net", and an OAuth API address
// "https://api.atlassian.com/ex/jira/<cloud id>/..." gives the cloud ID.
func NormalizeWorkspace(provider, v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !SameIntegrationProvider(provider, "jira") {
		return v
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	// An OAuth API address names the site by its cloud ID:
	// https://api.atlassian.com/ex/jira/<cloud id>/...
	if host == "api.atlassian.com" {
		if rest, ok := strings.CutPrefix(u.Path, "/ex/jira/"); ok {
			id, _, _ := strings.Cut(rest, "/")
			return strings.ToLower(id)
		}
	}
	return host
}

// IntegrationWorkspace returns the normalized workspace of a chat or
// tracker record, or "" when it has none.
func IntegrationWorkspace(in Integration) string {
	k := IntegrationWorkspaceKey(in.Provider)
	if k == "" {
		return ""
	}
	return NormalizeWorkspace(in.Provider, in.Config[k])
}

type factoryCtxKey struct{}

// WithFactory returns a context that carries the factory of an action. A
// tracker write-back has no factory argument; the notify router puts the
// factory of the work item in the context, and the tracker selects the
// integration record of that factory.
func WithFactory(ctx context.Context, factoryID string) context.Context {
	if factoryID == "" {
		return ctx
	}
	return context.WithValue(ctx, factoryCtxKey{}, factoryID)
}

// FactoryFrom returns the factory that WithFactory put in ctx, or "".
func FactoryFrom(ctx context.Context) string {
	v, _ := ctx.Value(factoryCtxKey{}).(string)
	return v
}
