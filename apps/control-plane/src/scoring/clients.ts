// The default implementations of the scoring ports over the Durable Objects:
// runs and messages from WorkDO (and compacted messages from R2), the engine
// through WorkDO.runsCreate / runsCancel / api, the store through TeamDO.
// Everything here is plumbing. The logic is in service.ts and benchmark.ts.
//
// Calls that WorkDO does not serve yet are listed in the report of WS13:
// WorkDO.api ops "listRunChildren" (exists), "getRunStartCommits",
// "createInboxItem", "followUp"; DispatchPlan.startingRepoRefs / agentPins;
// CreateRunInput.run.startingRepoRefs.
import {
  DEFAULT_TEAM,
  OfError,
  doName,
  hashToken,
  isOfError,
  newToken,
  toOfError,
  workDOName,
  type CompiledDefinition,
  type Run,
  type RunMessage,
  type Score,
  type Scorer,
  type WorkItem,
} from "@openfactory/core";
import type { CallMeta, Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { readCompactedMessages } from "../compaction/compact";
import { applyOverrides, judgeConfig, resolveAgent, withFactoryWide, type FactoryRuntimeLike } from "./overrides";
import type {
  DispatchRequest,
  EnginePort,
  FactoryInfo,
  InboxItemInput,
  JudgeRunRequest,
  RunsPort,
  ScoringStore,
} from "./types";

/** The principal of calls that the service makes by itself (queue consumer, timers). */
export function systemPrincipal(teamId: string): Principal {
  return { kind: "service_account", id: "system:scoring", teamId, role: "admin", factoryRoles: {} };
}

export function systemMeta(teamId: string, requestId = `scoring-${crypto.randomUUID()}`): CallMeta {
  return { requestId, principal: systemPrincipal(teamId) };
}

/** Throws the OfError of a `$error` result of a DO api() call, else returns the body. */
export function unwrapOp<T = unknown>(out: unknown): T {
  if (typeof out === "object" && out !== null) {
    const e = (out as { $error?: { code: string; detail: string; reason?: string; extra?: Record<string, unknown> } }).$error;
    if (e && typeof e === "object") {
      throw new OfError(e.code as never, e.detail, { ...(e.reason ? { reason: e.reason } : {}), ...(e.extra ? { extra: e.extra } : {}) });
    }
    const r = out as { $status?: number; body?: unknown };
    if (typeof r.$status === "number") return r.body as T;
  }
  return out as T;
}

const notFoundToNull = async <T>(p: Promise<T>): Promise<T | null> => {
  try {
    return await p;
  } catch (e) {
    if (isOfError(toOfError(e).error) && toOfError(e).error.code === "resource_not_found") return null;
    throw e;
  }
};

// ---------- runs ----------

/** Messages of a run: compacted ones from R2, then the rows that WorkDO still holds. */
export async function listAllMessages(
  env: Env,
  work: { api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> },
  meta: CallMeta,
  runId: string,
): Promise<Pick<RunMessage, "role" | "content" | "seq">[]> {
  const compacted = await readCompactedMessages(env.DATA, runId);
  const after = compacted.length > 0 ? compacted[compacted.length - 1]!.seq : 0;
  const out = unwrapOp<{ messages: RunMessage[] }>(await work.api(meta, "listRunMessages", { runId, after: String(after) }, undefined));
  return [...compacted, ...(out.messages ?? [])];
}

export function workRunsPort(env: Env, teamId: string): RunsPort {
  const meta = systemMeta(teamId);
  const stubOf = (id: string) => env.WORK.getByName(workDOName(teamId, id));
  return {
    async getRun(runId) {
      return notFoundToNull(stubOf(runId).getRun(runId) as Promise<Run>);
    },
    async getWorkItem(id) {
      return notFoundToNull(Promise.resolve(stubOf(id).api(meta, "getWorkItem", { id }, undefined)).then((o) => unwrapOp<WorkItem>(o)));
    },
    async listChildren(runId) {
      const out = unwrapOp<{ runs: Run[] }>(await stubOf(runId).api(meta, "listRunChildren", { runId }, undefined));
      return out.runs ?? [];
    },
    async listMessages(runId) {
      return listAllMessages(env, stubOf(runId), meta, runId);
    },
    async loadStartCommits(runId) {
      try {
        const out = unwrapOp<{ start_commits: { codeForge: string; owner: string; repo: string; ref: string; resolved?: boolean }[] }>(
          await stubOf(runId).api(meta, "getRunStartCommits", { runId }, undefined),
        );
        return (out.start_commits ?? []).map((s) => ({ ...s }));
      } catch (e) {
        if (toOfError(e).error.code === "feature_not_available") return [];
        throw e;
      }
    },
    async listOpenInbox(factoryId) {
      const idx = env.INDEX.getByName(doName.index(teamId));
      const out: { id: string; run_id?: string; work_item_id?: string }[] = [];
      let cursor = "";
      for (let page = 0; page < 20; page++) {
        const r = (await idx.list(meta, "inbox", { factory_id: factoryId, resolved: "false", limit: "200", ...(cursor ? { cursor } : {}) })) as {
          items: { id: string; run_id?: string; work_item_id?: string }[];
          page_info: { has_next_page: boolean; next_cursor?: string };
        };
        out.push(...r.items);
        if (!r.page_info.has_next_page) break;
        cursor = r.page_info.next_cursor ?? "";
      }
      return out;
    },
  };
}

// ---------- engine ----------

export function workEngine(env: Env, teamId: string): EnginePort {
  const meta = systemMeta(teamId);
  const team = () => env.TEAM.getByName(doName.team(teamId));
  const runtime = async (factoryId: string): Promise<FactoryRuntimeLike> => {
    // TeamDO.configSnapshot: { agents, runtime: { machines, secrets, mcp_servers, ... }, ... }
    const s = (await team().configSnapshot(factoryId)) as { agents: FactoryRuntimeLike["agents"]; runtime?: Partial<FactoryRuntimeLike> };
    return { agents: s.agents, machines: s.runtime?.machines ?? null, secrets: s.runtime?.secrets ?? null, mcp_servers: s.runtime?.mcp_servers ?? null };
  };
  return {
    async dispatch(req: DispatchRequest, key?: string) {
      const rt = await runtime(req.factoryId);
      const agent = resolveAgent(rt, req.agentName);
      let cfg = applyOverrides(agent, req.overrides, rt.machines);
      const pin = req.agentPins?.[cfg.name];
      if (pin) cfg = applyOverrides(cfg, pin, rt.machines);
      cfg = withFactoryWide(cfg, rt);
      // A key gives the same work item (and WorkDO) to a retry of the same dispatch.
      const token = key ? await hashToken("w", "dispatch", key) : newToken("w");
      const work = env.WORK.getByName(doName.work(teamId, token));
      const plan = {
        factoryId: req.factoryId,
        config: cfg,
        prompt: req.prompt,
        actor: req.triggeredBy || "api",
        title: req.title ?? "",
        source: req.source,
        ...(req.metadata ? { metadata: req.metadata } : {}),
        ...(req.triggeredBy ? { triggeredBy: req.triggeredBy } : {}),
        ...(req.startingRepoRefs ? { startingRepoRefs: req.startingRepoRefs } : {}),
        ...(req.agentPins ? { agentPins: req.agentPins } : {}),
        ...(key ? { ledger: { actionKey: key, target: "dispatch" } } : {}),
      };
      const out = (await work.runsCreate(meta, { dispatch: plan } as never)) as { run: Run; workItemId?: string };
      return out.workItemId ? { runId: out.run.id, workItemId: out.workItemId } : { runId: out.run.id };
    },
    async startJudge(req: JudgeRunRequest, runId: string) {
      const rt = await runtime(req.factoryId);
      const config = await judgeConfig(rt, req);
      const work = env.WORK.getByName(workDOName(teamId, runId));
      const out = (await work.runsCreate(meta, {
        run: {
          factoryId: req.factoryId,
          config,
          prompt: req.prompt,
          name: config.name,
          triggeredBy: req.triggeredBy || "scorer",
          ...(req.metadata ? { metadata: req.metadata } : {}),
          actor: "scoring",
          runId,
          ...(req.startingRepoRefs.length > 0 ? { startingRepoRefs: req.startingRepoRefs } : {}),
        },
      } as never)) as { run: Run };
      return { runId: out.run.id };
    },
    async cancel(runId, actor) {
      const work = env.WORK.getByName(workDOName(teamId, runId));
      await work.runsCancel({ ...meta, principal: { ...systemPrincipal(teamId), userId: actor } }, runId, true);
    },
    async followUp(runId, message, actor) {
      const work = env.WORK.getByName(workDOName(teamId, runId));
      await work.runsMessage(meta, runId, { message, actor });
    },
    async resolveInbox(inboxId, resolution, actor) {
      const work = env.WORK.getByName(workDOName(teamId, inboxId));
      unwrapOp(await work.api({ ...meta, principal: { ...systemPrincipal(teamId), userId: actor } }, "resolveInbox", { id: inboxId }, { resolution }));
    },
    async applyDefinition(factoryId, files, actor, message) {
      const out = unwrapOp<{ id?: string; version?: { id: string } }>(
        await team().definitionApply({ ...meta, principal: { ...systemPrincipal(teamId), userId: actor } }, factoryId, { files, author: actor, message }),
      );
      return { id: out.id ?? out.version?.id ?? "" };
    },
    async checkDefinition(factoryId, files) {
      const out = unwrapOp<{ errors?: { file: string; message: string }[] }>(
        await team().api(meta, "scoring.checkDefinition", {}, { factoryId, files }),
      );
      const errors = out.errors ?? [];
      return { valid: errors.length === 0, errors };
    },
    async createInboxItem(item: InboxItemInput) {
      if (item.work_item_id) {
        const work = env.WORK.getByName(workDOName(teamId, item.work_item_id));
        unwrapOp(await work.api(meta, "createInboxItem", {}, item));
        return;
      }
      // A team-level item is owned by IndexDO; TeamDO sends it through its outbox.
      unwrapOp(await team().api(meta, "scoring.inboxCreate", {}, item));
    },
  };
}

// ---------- store (TeamDO) ----------

/** Methods of the ScoringStore that TeamDO serves as the op "scoring.store". */
export const STORE_METHODS = [
  "getFactory",
  "activeDefinition",
  "listScorers",
  "getScore",
  "putScore",
  "listFailedScores",
  "markScoresImproved",
  "kvGet",
  "kvSet",
  "kvClaim",
  "listUsers",
] as const;
export type StoreMethod = (typeof STORE_METHODS)[number];

/** The ScoringStore of a Worker or a DO other than TeamDO: every call is one TeamDO op. */
export function remoteStore(env: Env, teamId: string): ScoringStore {
  const meta = systemMeta(teamId);
  const call = async <T>(method: StoreMethod, ...args: unknown[]): Promise<T> =>
    unwrapOp<T>(await env.TEAM.getByName(doName.team(teamId)).api(meta, "scoring.store", {}, { method, args }));
  return {
    getFactory: (id) => call<FactoryInfo>("getFactory", id),
    activeDefinition: (id) => call<{ definition: CompiledDefinition; version: { versionId: string; files: Record<string, string> } } | null>("activeDefinition", id),
    listScorers: (id) => call<Scorer[]>("listScorers", id),
    getScore: (s, r) => call<Score | null>("getScore", s, r),
    putScore: (sc) => call<Score>("putScore", sc),
    listFailedScores: (id) => call<{ score: Score; scorer: Scorer }[]>("listFailedScores", id),
    markScoresImproved: (ids, run) => call<void>("markScoresImproved", ids, run),
    kvGet: (k) => call<string>("kvGet", k),
    kvSet: (k, v) => call<void>("kvSet", k, v),
    kvClaim: (k, v) => call<boolean>("kvClaim", k, v),
    listUsers: () => call<{ id: string; email: string; role: string }[]>("listUsers"),
  };
}

export { DEFAULT_TEAM };
