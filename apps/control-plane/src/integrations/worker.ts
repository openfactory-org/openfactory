// Worker-side wiring of the integrations: the IntegrationStore over TeamDO
// RPC (internal ops of src/do/team/integrations.ts) and the secret reader over
// TeamRpc.secretsResolve. The rest (publish to IngressDO, inbox resolve,
// account linker) is supplied by the caller.
import { OfError, doName } from "@openfactory/core";
import type { ExternalIdentity, Integration, User } from "@openfactory/core";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import type { IntegrationDeps, IntegrationStore, SecretReader } from "./types";

const META: CallMeta = { requestId: "integrations" };

function unwrap<T>(out: unknown): T {
  if (typeof out === "object" && out !== null && "$error" in out) {
    const e = (out as { $error: { code: string; detail: string } }).$error;
    throw new OfError(e.code as never, e.detail);
  }
  return out as T;
}

/** The store of one team over TeamDO.api. */
export function rpcStore(env: Env, teamId = "default"): IntegrationStore {
  const stub = () => env.TEAM.getByName(doName.team(teamId));
  const call = async <T>(op: string, params: Record<string, string> = {}, body?: unknown): Promise<T> =>
    unwrap<T>(await stub().api(META, op, params, body));
  return {
    listIntegrations: (_team) => call<Integration[]>("integrations.list"),
    getIntegration: (id) => call<Integration | null>("integrations.get", { id }),
    setIntegrationStatus: async (id, status, reason) => (await call<{ changed: boolean }>("integrationsSetStatus", {}, { id, status, reason })).changed,
    findExternalIdentity: (provider, external_id) => call<ExternalIdentity | null>("identity.find", { provider, external_id }),
    getUser: (id) => call<User | null>("identity.user", { id }),
    getUserByEmail: (email) => call<User | null>("identity.userByEmail", { email }),
    linkExternalIdentity: async (x) => (await call<{ linked: boolean }>("identity.link", {}, x)).linked,
    upsertExternalIdentity: async (x) => {
      await call("identity.upsert", {}, x);
    },
    getFactoryIntegrations: (factory_id) => call<Record<string, Record<string, string>>>("factorySettings", { factory_id }),
    kvGet: (key) => call<string | null>("kv.get", { key }),
    kvSet: async (key, value) => {
      await call("kv.set", { key, value });
    },
  };
}

/** Team secrets by name, decrypted by TeamDO (team scope). */
export function rpcSecrets(env: Env, teamId = "default"): SecretReader {
  return {
    async get(_team, name) {
      const out = await env.TEAM.getByName(doName.team(teamId)).secretsResolve(META, { factoryId: "", names: [name] });
      const v = out[name];
      if (v === undefined) throw new OfError("resource_not_found", `secret ${name} not found`);
      return v;
    },
  };
}

/** Dependencies for the handlers of the default team. `rest` supplies publish and the optional parts. */
export function integrationDeps(env: Env, rest: Pick<IntegrationDeps, "publish"> & Partial<IntegrationDeps>, teamId = "default"): IntegrationDeps {
  return { store: rpcStore(env, teamId), secrets: rpcSecrets(env, teamId), ...rest };
}
