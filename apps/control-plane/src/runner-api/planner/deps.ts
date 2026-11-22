// The planner's view of the platform (PlannerDeps), its inputs, and the
// implementation over the DO bindings of the Worker.
import {
  doName,
  isOfError,
  newId,
  OfError,
  partitionOf,
  type Environment as CoreEnvironment,
  type Skill,
} from "@openfactory/core";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import type { Run, WorkItem } from "../../engine/types";
import { resolverFor } from "../../forges/env";
import type { Forge, ForgeResolver } from "../../forges/forge";
import { isOpError, unwrapOp } from "../../lib/rpc";
import type { McpServer } from "./mcp";
import type { RepoAtCommit, RepoRef, RepoStart } from "./repos";

/** Inputs that the run's WorkDO gives the planner. */
export interface PlannerState {
  workItem?: WorkItem;
  repoRefs: RepoAtCommit[];
  startCommits?: RepoStart[];
}

export interface Snapshot {
  factory: { id: string; name?: string };
  runtime: {
    repositories?: RepoRef[] | null;
    skills?: Skill[];
    credential_strategy?: string;
    cloud_providers?: {
      gcp?: {
        projectNumber?: string;
        workloadIdentityFederationPoolId?: string;
        workloadIdentityFederationProviderId?: string;
        serviceAccountEmail?: string;
      };
      audiences?: string[];
    };
  };
  agents: { name: string; type: string; description?: string; spawnable_by?: string[] | null }[];
  integrations?: Record<string, unknown>;
}

/** The fields of an environment that the planner reads. */
export type Environment = Pick<CoreEnvironment, "id" | "repos" | "setup_commands" | "env_vars">;

/** The planner's view of the platform. Tests replace it. */
export interface PlannerDeps {
  snapshot(factoryId: string): Promise<Snapshot>;
  state(run: Run): Promise<PlannerState>;
  recordStartCommits(run: Run, list: RepoStart[]): Promise<void>;
  environment(id: string): Promise<Environment | undefined>;
  /** A secret value; factoryId "" reads the team secret. undefined when missing. */
  secret(factoryId: string, name: string): Promise<string | undefined>;
  /** Secrets of one scope with their types. */
  listSecrets(factoryId: string): Promise<{ name: string; type: string; factory_id?: string }[]>;
  resolveManagedMcp(run: Run, managedId: string): Promise<McpServer>;
  forges: ForgeResolver & {
    forgeForUser?(
      l: Parameters<ForgeResolver["forge"]>[0],
      userId: string,
    ): Promise<{ forge: Forge; userToken: boolean }>;
  };
  sandboxUrl: string;
  now(): number;
}

/** The agent configuration of a run with the fields the planner reads. */
export type Cfg = Run["config"] & {
  harness: Run["config"]["harness"] & { auth?: { source?: string; secretName?: string }; reasoningLevel?: string };
  mcp_servers?: Record<string, McpServer>;
  skills?: Skill[];
};

export const cfgOf = (r: Run): Cfg => r.config as Cfg;

/** wrap: an error with a context prefix; an OfError keeps its code and reason. */
export function wrap(e: unknown, ctx: string): Error {
  if (isOfError(e)) return new OfError(e.code, `${ctx}: ${e.detail}`, e.reason ? { reason: e.reason } : undefined);
  return new Error(`${ctx}: ${e instanceof Error ? e.message : String(e)}`);
}

/** The run's environment, undefined without one; a missing environment is resource_not_found. */
export async function environmentOf(d: PlannerDeps, r: Run): Promise<Environment | undefined> {
  const id = r.config.environment_id;
  if (!id) return undefined;
  const e = await d.environment(id);
  if (!e) throw new OfError("resource_not_found", `environment ${JSON.stringify(id)}: environment ${id} not found`);
  return e;
}

// ---------- platform deps ----------

const internalMeta = (team: string, run: Run): CallMeta => {
  const principal: Principal = {
    kind: "run",
    id: run.id,
    runId: run.id,
    teamId: team,
    role: "admin",
    factoryRoles: {},
  };
  return { requestId: newId("req"), principal };
};

/** PlannerDeps over the DO bindings of env. */
export function platformDeps(env: Env, run: Run): PlannerDeps {
  const team = env.TEAM.getByName(doName.team(run.team_id));
  const work = () => env.WORK.getByName(doName.work(run.team_id, partitionOf(run.id)));
  const meta = internalMeta(run.team_id, run);
  const secretCache = new Map<string, Promise<string | undefined>>();
  const listCache = new Map<string, Promise<{ name: string; type: string; factory_id?: string }[]>>();
  return {
    snapshot: async (fid) => (await team.configSnapshot(fid)) as Snapshot,
    state: async (r) => (await work().plannerState(r.id)) as PlannerState,
    recordStartCommits: async (r, list) => {
      await work().recordStartCommits(r.id, list);
    },
    environment: async (id) => {
      const out: unknown = await team.api(meta, "getEnvironment", { id }, null);
      if (isOpError(out) && out.$error.code === "resource_not_found") return undefined;
      return unwrapOp<Environment>(out, "getEnvironment");
    },
    secret: (fid, name) => {
      const k = `${fid}\u0000${name}`;
      let p = secretCache.get(k);
      if (!p) {
        p = (async () => {
          const v = unwrapOp<{ found?: boolean; value?: string }>(
            await team.api(meta, "secretsGet", {}, { factory_id: fid, name }),
            "secretsGet",
          );
          return v?.found ? (v.value ?? "") : undefined;
        })();
        secretCache.set(k, p);
      }
      return p;
    },
    listSecrets: (fid) => {
      let p = listCache.get(fid);
      if (!p) {
        p = (async () =>
          unwrapOp<{ items?: { name: string; type: string; factory_id?: string }[] }>(
            await team.api(meta, "listSecrets", fid ? { "?factory_id": fid } : {}, null),
            "listSecrets",
          )?.items ?? [])();
        listCache.set(fid, p);
      }
      return p;
    },
    resolveManagedMcp: async (r, managedId) =>
      unwrapOp<McpServer>(
        await team.api(
          meta,
          "mcp.resolve",
          {},
          { run: { id: r.id, mcpServers: cfgOf(r).mcp_servers ?? {} }, managedId },
        ),
        "mcp.resolve",
      ),
    forges: resolverFor(env) as unknown as PlannerDeps["forges"],
    sandboxUrl: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, ""),
    now: () => Date.now(),
  };
}
