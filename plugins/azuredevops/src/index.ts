// The Azure DevOps plugin Worker: the Azure Repos code forge (pull requests,
// threads, statuses, work item comments and states, definition trees) and the
// service-hook receiver. The control plane binds it as env.PLUGIN_AZUREDEVOPS
// and calls the public methods of `Plugin` with typed RPC.
//
// All settings come from integration records of provider "azure_devops" (config
// org, base_url, hook_username, hook_password_ref, factory_identities,
// service_identities). The control plane resolves the token and passes it in
// ForgeTarget.token.
//
// Worker vars: PLUGIN_VERSION (optional version string).

import type {
  ForgePlugin,
  ForgeTarget,
  HostApi,
  IngressPlugin,
  PluginInfo,
  WebhookResult,
} from "@openfactory/plugin-api";
import { rpc } from "@openfactory/plugin-api";
import { ForgePluginWorker, pluginInfo, type ForgeSpec } from "@openfactory/plugin-api/worker";
import { AzureDevOpsClient, FORGE_NAME } from "./client";
import { PROVIDER, handleWebhook } from "./webhook";

export interface Env {
  PLUGIN_VERSION?: string;
}

/** The JSON Schema of an integration record of provider "azure_devops". */
export const CONFIG_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    org: { type: "string", description: "Organization name." },
    base_url: { type: "string", description: "Azure DevOps Services root. Default https://dev.azure.com." },
    hook_username: { type: "string", description: "User name of the service hook basic authentication." },
    hook_password_ref: { type: "string", description: "Name of the secret that holds the service hook password." },
    factory_identities: {
      type: "string",
      description: "Factory identities: identityGUID=alias,identityGUID2=alias2.",
    },
    service_identities: { type: "string", description: "Other identity GUIDs whose activity is ignored." },
  },
  required: ["org"],
});

/** Builds the Azure DevOps client of one integration (or user token). */
export function clientFor(t: ForgeTarget, fetchFn?: typeof fetch): AzureDevOpsClient {
  return new AzureDevOpsClient({
    organization: t.config["org"] ?? "",
    ...(t.config["base_url"] ? { baseUrl: t.config["base_url"] } : {}),
    token: t.token,
    ...(fetchFn ? { fetch: fetchFn } : {}),
  });
}

export class Plugin extends ForgePluginWorker<Env> implements ForgePlugin, IngressPlugin {
  protected readonly forge: ForgeSpec = {
    name: FORGE_NAME,
    label: "azuredevops",
    client: (t) => clientFor(t),
    unsupported: ["commentIssue"],
  };

  async info(): Promise<PluginInfo> {
    return pluginInfo(
      {
        name: "azuredevops",
        roles: ["forge", "ingress"],
        forgeProviders: [PROVIDER],
        integrationProviders: [PROVIDER],
        configSchema: CONFIG_SCHEMA,
      },
      this.env.PLUGIN_VERSION,
    );
  }

  handleWebhook(req: Request, host: HostApi): Promise<WebhookResult> {
    return rpc(() => handleWebhook(req, host));
  }
}

export default {
  fetch(): Response {
    return new Response("openfactory-plugin-azuredevops: call this Worker through a service binding", { status: 404 });
  },
};
