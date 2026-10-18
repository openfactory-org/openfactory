// Package integrations holds what the work-source integrations (slack, teams,
// linear, jira) share: account linking between external identities and Open
// Factories users, membership checks ("who can start work") and per-factory
// integration settings.
package integrations

import (
	"context"
	"errors"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ProviderLabel returns a display name for a provider.
func ProviderLabel(p string) string {
	switch p {
	case "slack":
		return "Slack"
	case "teams", "microsoft_teams":
		return "Microsoft Teams"
	case "linear":
		return "Linear"
	case "jira":
		return "Jira"
	case "github":
		return "GitHub"
	}
	return p
}

// Member resolves the Open Factory user linked to an external account and checks that the
// user belongs to teamID. It returns ("", nil) when the account is not linked to a team member.
func Member(ctx context.Context, store core.Store, teamID, provider, externalID string) (*core.User, error) {
	if store == nil || externalID == "" {
		return nil, nil
	}
	x, err := store.FindExternalIdentity(ctx, provider, externalID)
	if errors.Is(err, core.ErrNotFound) || (err == nil && x.UserID == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := store.GetUser(ctx, x.UserID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.TeamID != teamID {
		return nil, nil
	}
	return u, nil
}

// Setting keys of factory integration settings (core.Store.GetFactoryIntegrations).
const (
	SettingAutoRespondToThreadReplies = "autoRespondToThreadReplies"
)

// FactorySetting returns a factory's integration setting for provider (definition `type`, e.g.
// "slack", "microsoft-teams"), or "" when unset.
func FactorySetting(ctx context.Context, store core.Store, factoryID, provider, key string) string {
	if store == nil || factoryID == "" {
		return ""
	}
	m, err := store.GetFactoryIntegrations(ctx, factoryID)
	if err != nil {
		return ""
	}
	for _, p := range []string{provider, strings.ReplaceAll(provider, "-", "_"), strings.ReplaceAll(provider, "_", "-")} {
		if s, ok := m[p]; ok {
			return s[key]
		}
	}
	return ""
}

// AutoRespondToThreadReplies reports whether plain thread replies continue work (default true).
func AutoRespondToThreadReplies(ctx context.Context, store core.Store, factoryID, provider string) bool {
	v := strings.ToLower(strings.TrimSpace(FactorySetting(ctx, store, factoryID, provider, SettingAutoRespondToThreadReplies)))
	return v != "false" && v != "0" && v != "no"
}

// OpenInboxItem returns the newest unresolved inbox item of a work item (approvals and
// questions first), used to resolve chat button presses and replies.
func OpenInboxItem(ctx context.Context, store core.Store, workItemID string) (*core.InboxItem, error) {
	w, err := store.GetWorkItem(ctx, workItemID)
	if err != nil {
		return nil, err
	}
	items, err := store.ListInboxItems(ctx, w.FactoryID, false)
	if err != nil {
		return nil, err
	}
	var best *core.InboxItem
	for i := range items {
		it := &items[i]
		if it.WorkItemID != workItemID || it.Resolved {
			continue
		}
		pref := it.Kind == core.InboxApproval || it.Kind == core.InboxQuestion
		if best == nil {
			best = it
			continue
		}
		bestPref := best.Kind == core.InboxApproval || best.Kind == core.InboxQuestion
		if (pref && !bestPref) || (pref == bestPref && it.CreatedAt.After(best.CreatedAt)) {
			best = it
		}
	}
	if best == nil {
		return nil, core.ErrNotFound
	}
	return best, nil
}
