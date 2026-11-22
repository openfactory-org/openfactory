// The server route of plugin webhooks: /plugins/{name}/webhook. The plugin
// Worker verifies and maps the delivery; the events go to IngressDO.
import { DEFAULT_TEAM, OfError, doName } from "@openfactory/core";
import type { ServerRouteHandler } from "../builder";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { opResult } from "../http/handler";
import { b64url, sha256 } from "../lib/bytes";
import { forwardPluginWebhook, loadPlugins, pluginWebhookName, PluginHost, type PluginHostBackend } from "./index";

/** The path prefix of the plugin webhooks. */
export const PLUGIN_ROUTE_PREFIX = "/plugins/";

const SYSTEM_META: CallMeta = { requestId: "plugin-host" };

async function teamOp<T>(env: Env, op: string, body?: unknown, params: Record<string, string> = {}): Promise<T> {
  const res = opResult(await env.TEAM.getByName(doName.team(DEFAULT_TEAM)).api(SYSTEM_META, op, params, body));
  return (await res.json()) as T;
}

/** PluginHostBackend over TeamDO internal ops (single team core). */
export function pluginHostBackend(env: Env): PluginHostBackend {
  return {
    listTeams: async () => {
      const t = await teamOp<{ id: string; name: string } | null>(env, "teamGet");
      return t ? [{ id: t.id, name: t.name }] : [];
    },
    listIntegrations: (teamId) =>
      teamId && teamId !== DEFAULT_TEAM ? Promise.resolve([]) : teamOp(env, "integrations.list"),
    getSecret: async (_teamId, factoryId, name) => {
      const r = await teamOp<{ found: boolean; value: string }>(env, "secretsGet", { factory_id: factoryId, name });
      if (!r.found) throw new OfError("resource_not_found", `secret ${name} not found`);
      return r.value;
    },
    listFactories: () => teamOp(env, "factories.list"),
    getFactory: async (factoryId) => {
      const f = await teamOp<{ id: string; teamId: string; alias: string; name: string }>(env, "factories.get", {
        factory_id: factoryId,
      });
      return { id: f.id, teamId: f.teamId, alias: f.alias, name: f.name };
    },
    getFactoryRepos: async (factoryId) =>
      (await teamOp<{ repositories: never[] }>(env, "factories.get", { factory_id: factoryId })).repositories,
  };
}

/** Partition of a plugin event: hash of plugin name and source (IngressDO name). */
async function pluginPartition(name: string, source: Record<string, unknown>): Promise<string> {
  const key = `plugin:${name}:${JSON.stringify(source, Object.keys(source).sort())}`;
  return b64url(await sha256(key)).slice(0, 22);
}

/** Forwards /plugins/{name}/webhook to the plugin; other /plugins/ paths pass (null). */
export const pluginWebhookRoute: ServerRouteHandler = async (request, env, _ctx, url) => {
  const name = pluginWebhookName(url.pathname);
  if (!name) return null;
  const backend = pluginHostBackend(env);
  const registry = await loadPlugins(env);
  return forwardPluginWebhook(name, request, {
    registry,
    host: (pluginName) => {
      const p = registry.get(pluginName);
      return new PluginHost(backend, { plugin: p?.info ?? { name: pluginName } });
    },
    deliver: async (deliveries) => {
      for (const d of deliveries) {
        const teamId = d.event.team_id || DEFAULT_TEAM;
        const partition = await pluginPartition(name, d.event.source ?? {});
        await env.INGRESS.getByName(doName.ingress(teamId, partition)).enqueueEvent(partition, d.event, d.dedupeKey);
      }
    },
  });
};
