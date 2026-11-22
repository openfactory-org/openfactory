// The GitLab plugin Worker: the GitLab code forge (merge requests, notes,
// commit statuses, definition trees, review threads) and the GitLab webhook
// receiver. The control plane binds it as env.PLUGIN_GITLAB and calls the
// public methods of `Plugin` with typed RPC.
//
// All settings come from integration records of provider "gitlab" (config
// base_url, web_url, token_type, user_token_type, org, bot_username,
// factory_bots, service_accounts, webhook_secret_ref). The control plane
// resolves the token and passes it in ForgeTarget.token.
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
import { FORGE_NAME, GitLabClient } from "./client";
import { PROVIDER, handleWebhook } from "./webhook";

export interface Env {
  PLUGIN_VERSION?: string;
}

/** The JSON Schema of an integration record of provider "gitlab". */
export const CONFIG_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    org: { type: "string", description: "Top-level group the record covers." },
    base_url: { type: "string", description: "API root, for example https://gitlab.example.com/api/v4." },
    web_url: { type: "string", description: "Git host root. Derived from base_url when empty." },
    token_type: { type: "string", enum: ["private", "oauth"], description: "Header style of the integration token." },
    user_token_type: { type: "string", enum: ["private", "oauth"], description: "Header style of personal tokens." },
    bot_username: { type: "string", description: "Team-wide bot account." },
    factory_bots: { type: "string", description: "Per-factory bot accounts: username=alias,username2=alias2." },
    service_accounts: { type: "string", description: "Other service accounts whose activity is ignored." },
    webhook_secret_ref: { type: "string", description: "Name of the secret that holds the X-Gitlab-Token value." },
  },
});

/** Builds the GitLab client of one integration (or user token). */
export function clientFor(t: ForgeTarget, fetchFn?: typeof fetch): GitLabClient {
  const tokenType = t.userToken ? t.config["user_token_type"] : t.config["token_type"];
  return new GitLabClient({
    ...(t.config["base_url"] ? { baseUrl: t.config["base_url"] } : {}),
    ...(t.config["web_url"] ? { webUrl: t.config["web_url"] } : {}),
    token: t.token,
    ...(tokenType ? { tokenType } : {}),
    ...(fetchFn ? { fetch: fetchFn } : {}),
  });
}

export class Plugin extends ForgePluginWorker<Env> implements ForgePlugin, IngressPlugin {
  protected readonly forge: ForgeSpec = {
    name: FORGE_NAME,
    label: "gitlab",
    client: (t) => clientFor(t),
    unsupported: ["commentWorkItem", "setWorkItemState", "attachPullRequestToWorkItem"],
  };

  async info(): Promise<PluginInfo> {
    return pluginInfo(
      {
        name: "gitlab",
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
    return new Response("openfactory-plugin-gitlab: call this Worker through a service binding", { status: 404 });
  },
};
