// Worker-side adapters that read integration records and secrets from the
// team's TeamDO, and build a forge Resolver from the environment.
// TeamDO ops used: integrationsForProvider and integrationsSetStatus
// (src/do/team/integrations.ts), secretsGet (src/do/team/secrets.ts),
// userForgeTokenGet (src/do/team/user-forge-tokens.ts).
import { doName, type Integration } from "@openfactory/core";
import type { ForgePlugin } from "@openfactory/plugin-api";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { unwrapOp } from "../lib/rpc";
import { Resolver, type IntegrationSource, type SecretSource, type UserTokenSource } from "./registry";

const meta = (requestId = "forge"): CallMeta => ({ requestId });

async function teamOp(
  env: Env,
  teamId: string,
  op: string,
  params: Record<string, string>,
  body?: unknown,
): Promise<unknown> {
  return unwrapOp(await env.TEAM.getByName(doName.team(teamId)).api(meta(), op, params, body), `TeamDO ${op}`);
}

export function teamIntegrations(env: Env): IntegrationSource {
  return {
    async list(teamId, provider) {
      const out = (await teamOp(env, teamId, "integrationsForProvider", provider ? { "?provider": provider } : {})) as
        { items?: Integration[] } | undefined;
      return out?.items ?? [];
    },
    async setStatus(id, status, reason) {
      // The core has one team; integration IDs are unique inside it.
      const out = (await teamOp(env, "default", "integrationsSetStatus", {}, { id, status, reason })) as
        { changed?: boolean } | boolean | undefined;
      return typeof out === "boolean" ? out : !!out?.changed;
    },
  };
}

export function teamSecrets(env: Env): SecretSource {
  return {
    async get(teamId, factoryId, name) {
      const out = (await teamOp(env, teamId, "secretsGet", {}, { factory_id: factoryId, name })) as
        { found?: boolean; value?: string } | undefined;
      return out?.found ? (out.value ?? "") : undefined;
    },
  };
}

export function teamUserTokens(env: Env): UserTokenSource {
  return {
    async get(teamId, userId, codeForge) {
      const out = (await teamOp(env, teamId, "userForgeTokenGet", {}, { user_id: userId, forge: codeForge })) as
        { token?: string } | undefined;
      return out?.token || undefined;
    },
  };
}

/**
 * Forge plugins by name: every PLUGIN_<NAME> service binding of env (the
 * bundled gitlab and azuredevops, and any plugin an extended build binds).
 * The name is the binding suffix in lower case.
 */
export function forgePluginBindings(env: Env): Record<string, ForgePlugin> {
  const out: Record<string, ForgePlugin> = {};
  for (const [k, v] of Object.entries(env)) {
    if (k.startsWith("PLUGIN_") && v !== null && (typeof v === "object" || typeof v === "function")) {
      out[k.slice("PLUGIN_".length).toLowerCase()] = v as ForgePlugin;
    }
  }
  return out;
}

let cached: { env: Env; resolver: Resolver } | undefined;

/** The forge Resolver of this isolate for env (clients and tokens cached per isolate). */
export function resolverFor(env: Env): Resolver {
  if (cached && cached.env === env) return cached.resolver;
  const resolver = new Resolver({
    integrations: teamIntegrations(env),
    secrets: teamSecrets(env),
    userTokens: teamUserTokens(env),
    plugins: forgePluginBindings(env),
  });
  cached = { env, resolver };
  return resolver;
}
