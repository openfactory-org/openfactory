// Worker-side adapters that read integration records and secrets from the
// team's TeamDO, and build a forge Resolver from the environment.
// TeamDO ops used: integrationsForProvider and integrationsSetStatus (WS10b,
// src/do/team/integrations.ts), secretsGet (WS4, src/do/team/secrets.ts),
// userForgeTokenGet (this workstream, src/do/team/schedules.ts).
import { doName, type Integration } from "@openfactory/core";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { Resolver, type IntegrationSource, type SecretSource, type UserTokenSource } from "./registry";

const meta = (requestId = "forge"): CallMeta => ({ requestId });

async function teamOp(env: Env, teamId: string, op: string, params: Record<string, string>, body?: unknown): Promise<unknown> {
  const out = await env.TEAM.getByName(doName.team(teamId)).api(meta(), op, params, body);
  if (typeof out === "object" && out !== null && "$error" in out) {
    const e = (out as { $error: { code: string; detail: string } }).$error;
    throw new Error(`TeamDO ${op}: ${e.code}: ${e.detail}`);
  }
  if (typeof out === "object" && out !== null && "$status" in out) return (out as { body?: unknown }).body;
  return out;
}

export function teamIntegrations(env: Env): IntegrationSource {
  return {
    async list(teamId, provider) {
      const out = (await teamOp(env, teamId, "integrationsForProvider", provider ? { "?provider": provider } : {})) as { items?: Integration[] } | undefined;
      return out?.items ?? [];
    },
    async setStatus(id, status, reason) {
      // The core has one team; integration IDs are unique inside it.
      const out = (await teamOp(env, "default", "integrationsSetStatus", {}, { id, status, reason })) as { changed?: boolean } | boolean | undefined;
      return typeof out === "boolean" ? out : !!out?.changed;
    },
  };
}

export function teamSecrets(env: Env): SecretSource {
  return {
    async get(teamId, factoryId, name) {
      const out = (await teamOp(env, teamId, "secretsGet", {}, { factory_id: factoryId, name })) as { found?: boolean; value?: string } | undefined;
      return out?.found ? (out.value ?? "") : undefined;
    },
  };
}

export function teamUserTokens(env: Env): UserTokenSource {
  return {
    async get(teamId, userId, codeForge) {
      const out = (await teamOp(env, teamId, "userForgeTokenGet", {}, { user_id: userId, forge: codeForge })) as { token?: string } | undefined;
      return out?.token || undefined;
    },
  };
}

let cached: { env: Env; resolver: Resolver } | undefined;

/** The forge Resolver of this isolate for env (clients and tokens cached per isolate). */
export function resolverFor(env: Env): Resolver {
  if (cached && cached.env === env) return cached.resolver;
  const resolver = new Resolver({
    integrations: teamIntegrations(env),
    secrets: teamSecrets(env),
    userTokens: teamUserTokens(env),
    plugins: { gitlab: env.PLUGIN_GITLAB, azuredevops: env.PLUGIN_AZUREDEVOPS },
  });
  cached = { env, resolver };
  return resolver;
}
