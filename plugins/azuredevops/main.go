// Command openfactory-plugin-azuredevops is the Open Factory Azure DevOps
// plugin: the Azure Repos code forge (pull requests, threads, statuses,
// work item comments, definition trees) and the service-hook receiver.
//
// Install the binary as openfactory-plugin-azuredevops in the server's plugin
// directory (OF_PLUGIN_DIR). Service hooks are served at
// POST /plugins/azuredevops/webhook.
//
// All settings come from integration records of provider "azure_devops"
// (config org, base_url, hook_username, hook_password_ref,
// factory_identities, service_identities). The factory of a record
// is the record's own factory field.
package main

import (
	"context"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
	"github.com/openfactory-org/openfactory/plugins/azuredevops/internal/azuredevops"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	host := &sdk.DeferredHost{}
	sdk.Serve(sdk.Plugin{
		Name:           "azuredevops",
		Version:        version,
		Host:           host,
		ForgeProviders: []string{azuredevops.Provider},
		Forge:          forgeFactory,
		Ingress:        newWebhook(host),
	})
}

func newWebhook(host *sdk.DeferredHost) http.Handler {
	return azuredevops.NewWebhookHandler(azuredevops.WebhookOptions{
		Store: sdk.Store(host), Secrets: sdk.Secrets(host), Bus: sdk.Bus(),
		FactoryRepos: host.FactoryRepos,
		Logger:       sdk.Logger(host),
	})
}

// forgeFactory builds an Azure DevOps client for one integration (or user token).
func forgeFactory(_ context.Context, t sdk.ForgeTarget) (core.Forge, error) {
	return azuredevops.New(azuredevops.Config{Organization: t.Config["org"], BaseURL: t.Config["base_url"], Token: t.Token})
}
