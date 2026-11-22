// Extension points of the control plane (D2: the enterprise build imports
// @openfactory/control-plane and adds to these registries before export).
//
// Server routes outside /api/v1 (/runner/v1, /mcp, /oauth, /webhooks,
// /integrations, /plugins, ...) register here by path prefix. Their owner
// modules must be imported by src/index.ts (ask WS4 to add the import).
import type { Env } from "./contracts/env";

export type ServerRouteHandler = (
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string,
) => Promise<Response | null>;

interface ServerRoute {
  prefix: string;
  handler: ServerRouteHandler;
}

const serverRoutes: ServerRoute[] = [];

/** Registers a handler for paths that start with prefix. Longer prefixes win. Return null to pass. */
export function registerServerRoute(prefix: string, handler: ServerRouteHandler): void {
  serverRoutes.push({ prefix, handler });
  serverRoutes.sort((a, b) => b.prefix.length - a.prefix.length);
}

export function matchServerRoutes(path: string): ServerRouteHandler[] {
  return serverRoutes.filter((r) => path === r.prefix || path.startsWith(r.prefix)).map((r) => r.handler);
}

export type CronHook = (env: Env, ctx: ExecutionContext) => Promise<void>;
const cronHooks: CronHook[] = [];

/** Registers work for the 1/min Cron safety net. */
export function registerCronHook(h: CronHook): void {
  cronHooks.push(h);
}

export function cronHooksList(): readonly CronHook[] {
  return cronHooks;
}

// ---------- plugin webhooks (/plugins/{name}/webhook, WS12) ----------

import { DEFAULT_TEAM, doName } from "@openfactory/core";
import { OfError } from "@openfactory/core";
import type { CallMeta } from "./contracts/do-messages";
import {
  forwardPluginWebhook,
  loadPlugins,
  pluginWebhookName,
  PluginHost,
  type PluginHostBackend,
} from "./plugins/index";
import { b64url, sha256 } from "./lib/bytes";
import { opResult } from "./http/handler";

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

/** Partition of a plugin event: hash of plugin name and source (03 §1 IngressDO name). */
async function pluginPartition(name: string, source: Record<string, unknown>): Promise<string> {
  const key = `plugin:${name}:${JSON.stringify(source, Object.keys(source).sort())}`;
  return b64url(await sha256(key)).slice(0, 22);
}

registerServerRoute("/plugins/", async (request, env, _ctx, url) => {
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
        await env.INGRESS.getByName(doName.ingress(teamId, partition)).enqueue(partition, d.event, d.dedupeKey);
      }
    },
  });
});
