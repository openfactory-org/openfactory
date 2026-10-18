// Package links builds dashboard URLs. It is the single source of truth for
// the paths the dashboard (web/, served by internal/webui) routes, so links emitted by the API, the Factory MCP
// server, scoring and chat/forge messages always resolve to a dashboard page.
//
// Every function takes the externally reachable base URL (OF_PUBLIC_URL); a
// trailing slash is ignored and an empty base yields a root-relative path.
package links

import (
	"net/url"
	"strings"
)

func join(base string, parts ...string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(p)
	}
	return b.String()
}

func esc(s string) string { return url.PathEscape(s) }

// Factory is a factory's dashboard: /f/{fid}.
func Factory(base, factoryID string) string { return join(base, "f", esc(factoryID)) }

// FactorySettings is a factory's settings page: /f/{fid}/settings.
func FactorySettings(base, factoryID string) string {
	return join(base, "f", esc(factoryID), "settings")
}

// FactoryDefinition is a factory's definition page: /f/{fid}/definition.
func FactoryDefinition(base, factoryID string) string {
	return join(base, "f", esc(factoryID), "definition")
}

// FactoryWebhook is a custom webhook's page: /f/{fid}/webhooks/{id}.
func FactoryWebhook(base, factoryID, webhookID string) string {
	return join(base, "f", esc(factoryID), "webhooks", esc(webhookID))
}

// WorkItem is a work item (task): /f/{fid}/work/{wid}.
func WorkItem(base, factoryID, workItemID string) string {
	return join(base, "f", esc(factoryID), "work", esc(workItemID))
}

// Run is a run transcript: /runs/{id}.
func Run(base, runID string) string { return join(base, "runs", esc(runID)) }

// Inbox is the team inbox: /inbox.
func Inbox(base string) string { return join(base, "inbox") }

// Integrations is the team integrations page: /integrations. When provider
// is set, it is passed as ?provider= (and connection as ?connection=) so the
// page can preselect it.
func Integrations(base, provider, connection string) string {
	u := join(base, "integrations")
	q := url.Values{}
	if provider != "" {
		q.Set("provider", provider)
	}
	if connection != "" {
		q.Set("connection", connection)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}
