// Command openfactory-plugin-gitlab is the Open Factory GitLab plugin: the
// GitLab code forge (merge requests, notes, commit statuses, definition
// trees) and the GitLab webhook receiver.
//
// Install the binary as openfactory-plugin-gitlab in the server's plugin
// directory (OF_PLUGIN_DIR). Webhooks are served at POST /plugins/gitlab/webhook.
//
// All settings come from integration records of provider "gitlab"
// (config base_url, web_url, token_type, user_token_type, group,
// bot_username, factory_bots, service_accounts, webhook_secret_ref).
package main

import (
	"context"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
	"github.com/openfactory-org/openfactory/plugins/gitlab/internal/gitlab"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	host := &sdk.DeferredHost{}
	sdk.Serve(sdk.Plugin{
		Name:           "gitlab",
		Version:        version,
		Host:           host,
		ForgeProviders: []string{gitlab.Provider},
		Forge:          forgeFactory,
		Ingress:        newWebhook(host),
	})
}

func newWebhook(host *sdk.DeferredHost) http.Handler {
	return gitlab.NewWebhookHandler(gitlab.WebhookOptions{
		Store: sdk.Store(host), Secrets: sdk.Secrets(host), Bus: sdk.Bus(), Logger: sdk.Logger(host),
	})
}

// forgeFactory builds a GitLab client for one integration (or user token).
func forgeFactory(_ context.Context, t sdk.ForgeTarget) (core.Forge, error) {
	tokenType := t.Config["token_type"]
	if t.UserToken {
		tokenType = t.Config["user_token_type"]
	}
	return gitlab.New(gitlab.Config{BaseURL: t.Config["base_url"], WebURL: t.Config["web_url"], Token: t.Token, TokenType: tokenType})
}
