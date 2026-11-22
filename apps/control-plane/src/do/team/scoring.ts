// TeamDO module: scoring (WS13). Scores and markers of the scoring service,
// scorer and benchmark suite lists, the benchmark plan, and the review of
// proposed definition changes. Migration range 0400-0499.
//
// Needs one line in src/do/team/index.ts: add `scoring` to MODULES.
import {
  OfError,
  newId,
  type BenchmarkConfiguration,
  type BenchmarkSuite,
  type HarnessConfig,
  type RepoSubstitution,
  type Score,
  type Scorer,
} from "@openfactory/core";
import { actorId, requireRole } from "../../auth/roles";
import { bodyObject, optString, str, type JsonObject } from "../../lib/body";
import { dest } from "../../lib/outbox/dest";
import { listResponse, msOf } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import {
  correctnessScorerFor as _unused,
  findSuite,
  validateConfigurations,
  type BenchmarkScorer,
} from "../../scoring/benchmark";
import { diffFiles, isProposalState } from "../../scoring/proposals";
import { scorerExec, scorerName } from "../../scoring/judge";
import { DEFAULT_CORRECTNESS_MODEL, MAX_BENCHMARK_REPETITIONS, type FactoryInfo } from "../../scoring/types";
import { STORE_METHODS, type StoreMethod } from "../../scoring/clients";
import type { BenchmarkPlan } from "../benchmark/index";
import {
  activeFiles,
  configSnapshot,
  factoryFor,
  getFactoryById,
  getVersion,
  checkDefinition,
  definitionApply,
} from "./factories";
import type { DefinitionVersion } from "./factories";
import type { OpCall, TeamKit, TeamOp } from "./index";
import { scoringSweep } from "../../scoring/sweep";

void _unused;

export const migrations: readonly Migration[] = [
  {
    version: 400,
    name: "scoring",
    sql: `
CREATE TABLE scoring_scores (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, scorer_id TEXT NOT NULL, run_id TEXT NOT NULL,
  passed INTEGER NOT NULL, improvement_run_id TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 1,
  data TEXT NOT NULL);
CREATE UNIQUE INDEX scoring_scores_scorer_run ON scoring_scores (scorer_id, run_id);
CREATE INDEX scoring_scores_factory ON scoring_scores (factory_id, passed, improvement_run_id);
CREATE TABLE scoring_kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_ms INTEGER NOT NULL);
`,
  },
];

type Snapshot = {
  factory: FactoryInfo & Record<string, unknown>;
  version_id: string;
  runtime: Record<string, unknown>;
  agents: { name: string }[];
  scorers: Scorer[];
};

function scorersOf(kit: TeamKit, factoryId: string): Scorer[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM scorers WHERE factory_id = ? ORDER BY ord", factoryId).map(
    (r) => JSON.parse(r.data) as Scorer,
  );
}

function projectScore(kit: TeamKit, s: Score & { factory_id: string }, version: number): void {
  kit.enqueue({
    dest: dest.index(kit.teamId()),
    kind: "index.project",
    msgId: `score:${s.id}:${version}`,
    payload: {
      entity: "score",
      id: s.id,
      version,
      summary: { ...s, state: s.judge_run_id && !s.label ? "pending" : "done" },
    },
  });
}

/** The ScoringStore on the local tables. TeamDO serves it as the op "scoring.store". */
export function localStore(kit: TeamKit) {
  return {
    getFactory(id: string): FactoryInfo {
      const f = getFactoryById(kit, id);
      if (!f) throw new OfError("resource_not_found", `factory ${id} not found`);
      return {
        id: f.id,
        team_id: f.team_id,
        name: f.name,
        source: f.source,
        active_version_id: f.active_version_id ?? "",
        ...(f.git_repo ? { git_repo: f.git_repo } : {}),
        ...(f.git_branch ? { git_branch: f.git_branch } : {}),
        ...(f.git_path ? { git_path: f.git_path } : {}),
      };
    },
    async activeDefinition(id: string) {
      const f = getFactoryById(kit, id);
      if (!f || !f.active_version_id) return null;
      const snap = (await configSnapshot(kit, f.id)) as Snapshot;
      const rt = snap.runtime as Record<string, unknown>;
      const definition = {
        name: f.name,
        agents: snap.agents,
        machines: rt["machines"] ?? null,
        benchmarks: rt["benchmarks"] ?? null,
        scorerDefaults: rt["scorer_defaults"],
        selfImprovement: rt["self_improvement"],
        benchmarkRepoSubstitutions: rt["benchmark_repo_substitutions"],
      };
      return { definition, version: { versionId: f.active_version_id, files: await activeFiles(kit, f) } };
    },
    listScorers: (id: string): Scorer[] => scorersOf(kit, id),
    getScore(scorerId: string, runId: string): Score | null {
      const r = first<{ data: string }>(
        kit.sql,
        "SELECT data FROM scoring_scores WHERE scorer_id = ? AND run_id = ?",
        scorerId,
        runId,
      );
      return r ? (JSON.parse(r.data) as Score) : null;
    },
    putScore(s: Score & { factory_id: string }): Score {
      return kit.tx(() => {
        const prev = first<{ id: string; version: number }>(
          kit.sql,
          "SELECT id, version FROM scoring_scores WHERE scorer_id = ? AND run_id = ?",
          s.scorer_id,
          s.run_id,
        );
        const score = { ...s, id: prev?.id ?? s.id };
        const version = (prev?.version ?? 0) + 1;
        kit.sql.exec(
          `INSERT INTO scoring_scores (id, factory_id, scorer_id, run_id, passed, improvement_run_id, created_ms, version, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(scorer_id, run_id) DO UPDATE SET passed = excluded.passed, improvement_run_id = excluded.improvement_run_id,
             created_ms = excluded.created_ms, version = excluded.version, data = excluded.data`,
          score.id,
          score.factory_id,
          score.scorer_id,
          score.run_id,
          score.passed ? 1 : 0,
          score.improvement_run_id ?? "",
          msOf(score.created_at) || Date.now(),
          version,
          JSON.stringify(score),
        );
        projectScore(kit, score, version);
        kit.timers.setEarliest("scoring.sweep", "scoring.sweep", Date.now() + 15 * 60_000);
        return score;
      });
    },
    listFailedScores(factoryId: string): { score: Score; scorer: Scorer }[] {
      const scorers = scorersOf(kit, factoryId).filter((s) => s.self_improvement);
      const out: { score: Score; scorer: Scorer }[] = [];
      for (const sc of scorers) {
        for (const r of all<{ data: string }>(
          kit.sql,
          "SELECT data FROM scoring_scores WHERE scorer_id = ? AND passed = 0 AND improvement_run_id = ''",
          sc.id,
        )) {
          out.push({ score: JSON.parse(r.data) as Score, scorer: sc });
        }
      }
      return out;
    },
    markScoresImproved(ids: string[], runId: string): void {
      kit.tx(() => {
        for (const id of ids) {
          const r = first<{ data: string; version: number; factory_id: string }>(
            kit.sql,
            "SELECT data, version, factory_id FROM scoring_scores WHERE id = ?",
            id,
          );
          if (!r) continue;
          const s = { ...(JSON.parse(r.data) as Score), improvement_run_id: runId };
          const version = r.version + 1;
          kit.sql.exec(
            "UPDATE scoring_scores SET improvement_run_id = ?, version = ?, data = ? WHERE id = ?",
            runId,
            version,
            JSON.stringify(s),
            id,
          );
          projectScore(kit, { ...s, factory_id: r.factory_id }, version);
        }
      });
    },
    kvGet: (key: string): string =>
      first<{ value: string }>(kit.sql, "SELECT value FROM scoring_kv WHERE key = ?", key)?.value ?? "",
    kvSet(key: string, value: string): void {
      kit.tx(() =>
        kit.sql.exec(
          "INSERT INTO scoring_kv (key, value, updated_ms) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_ms = excluded.updated_ms",
          key,
          value,
          Date.now(),
        ),
      );
    },
    kvClaim: (key: string, value: string): boolean =>
      kit.tx(() => {
        const cur = first<{ value: string }>(kit.sql, "SELECT value FROM scoring_kv WHERE key = ?", key);
        if (cur && cur.value !== "") return false;
        kit.sql.exec(
          "INSERT INTO scoring_kv (key, value, updated_ms) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_ms = excluded.updated_ms",
          key,
          value,
          Date.now(),
        );
        return true;
      }),
    listUsers: (): { id: string; email: string; role: string }[] =>
      all<{ id: string; email: string; role: string }>(kit.sql, "SELECT id, email, role FROM users"),
  };
}

const scoringOps: Record<string, TeamOp> = {
  /** DO-to-DO: one ScoringStore method. */
  "scoring.store": async (kit, c) => {
    const b = bodyObject(c.body);
    const method = str(b, "method") as StoreMethod;
    if (!(STORE_METHODS as readonly string[]).includes(method))
      throw new OfError("invalid_request", `unknown store method ${method}`);
    const store = localStore(kit) as unknown as Record<string, (...a: unknown[]) => unknown>;
    const out = await store[method]!(...((b["args"] as unknown[]) ?? []));
    return out === undefined ? null : out;
  },
  "scoring.checkDefinition": async (kit, c) => {
    const b = bodyObject(c.body);
    const f = getFactoryById(kit, str(b, "factoryId"));
    if (!f) throw new OfError("resource_not_found", "factory not found");
    const chk = await checkDefinition(kit, f, b["files"] as Record<string, string>);
    return { errors: chk.errors };
  },
  /** A team-level inbox item (no work item): IndexDO owns it. */
  "scoring.inboxCreate": (kit, c) => {
    const item = bodyObject(c.body);
    kit.tx(() =>
      kit.enqueue({
        dest: dest.index(kit.teamId()),
        kind: "index.inboxCreate",
        msgId: `inbox:${String(item["id"])}`,
        payload: { item },
      }),
    );
    return { ok: true };
  },
  /** Worker handlers: authorizes the caller on the factory and returns what they need. */
  scoringFactory: (kit, c) => {
    const f = factoryFor(kit, c);
    return { factory_id: f.id, team_id: f.team_id, scorers: scorersOf(kit, f.id) };
  },
  listScorers: (kit, c) => listResponse(scorersOf(kit, factoryFor(kit, c).id), ""),
  listBenchmarkSuites: async (kit, c) => {
    const f = factoryFor(kit, c);
    const def = await localStore(kit).activeDefinition(f.id);
    return listResponse((def?.definition.benchmarks ?? []) as unknown[], "");
  },
  /** POST .../benchmarks/{suite}/runs, step 1: validates the request and snapshots the inputs. */
  scoringBenchmarkPlan: async (kit, c) => {
    const f = factoryFor(kit, c);
    requireRole(c.meta.principal, f.id, "editor");
    const b = bodyObject(c.body, true);
    const reps = typeof b["repetitions"] === "number" ? (b["repetitions"] as number) : 1;
    if (reps < 0 || reps > MAX_BENCHMARK_REPETITIONS)
      throw new OfError("invalid_request", `repetitions must be between 1 and ${MAX_BENCHMARK_REPETITIONS}`);
    const active = await localStore(kit).activeDefinition(f.id);
    if (!active) throw new OfError("invalid_request", `factory ${f.id} has no active definition`);
    const def = active.definition as unknown as {
      agents: { name: string }[];
      machines: Record<string, never> | null;
      benchmarks: BenchmarkSuite[] | null;
      scorerDefaults?: {
        harness?: HarnessConfig;
        model?: string;
        machine?: string;
        secrets?: string[];
        mcp_servers?: never;
      };
      benchmarkRepoSubstitutions?: RepoSubstitution[];
    };
    const suite = findSuite(def.benchmarks, c.params["suite"] ?? "");
    if (!suite)
      throw new OfError("resource_not_found", `benchmark suite ${JSON.stringify(c.params["suite"])} not found`);
    if (!suite.tasks || suite.tasks.length === 0)
      throw new OfError("invalid_request", `benchmark suite ${JSON.stringify(suite.slug)} has no tasks`);
    let configs = (b["configurations"] as BenchmarkConfiguration[] | undefined) ?? [];
    if (configs.length === 0) configs = suite.configurations ?? [];
    if (configs.length === 0) configs = [{ name: "default" }];
    validateConfigurations(configs, def.agents, def.machines);
    const scorers: BenchmarkScorer[] = [];
    const seen = new Set<string>();
    for (const ref of (b["scorers"] as string[] | undefined) ?? []) {
      const all_ = scorersOf(kit, f.id);
      const sc = all_.find((s) => s.id === ref) ?? all_.find((s) => s.slug === ref || s.name === ref);
      if (!sc) throw new OfError("resource_not_found", `scorer ${JSON.stringify(ref)} not found`);
      if (seen.has(sc.id)) continue;
      seen.add(sc.id);
      const key = scorerName(sc) === "correctness" ? "scorer:correctness" : scorerName(sc);
      scorers.push({
        key,
        scorer_id: sc.id,
        name: sc.name,
        model: sc.model,
        rubric: sc.rubric,
        labels: sc.labels ?? [],
        passing_score: sc.passing_score,
        ...scorerExec(sc),
      });
    }
    const sd = def.scorerDefaults;
    const judge: BenchmarkPlan["correctness_judge"] = {};
    if (sd) {
      if (sd.machine) judge.machine = sd.machine;
      if (sd.secrets) judge.secrets = sd.secrets;
      if (sd.mcp_servers) judge.mcp_servers = sd.mcp_servers;
      if (sd.harness) judge.harness = { ...sd.harness };
    }
    const plan: BenchmarkPlan = {
      suite,
      configurations: configs,
      repetitions: reps === 0 ? 1 : reps,
      correctness_model: DEFAULT_CORRECTNESS_MODEL,
      correctness_judge: judge,
      ...(scorers.length > 0 ? { scorers } : {}),
      agents: def.agents.map((a) => a.name),
    };
    return { factoryId: f.id, teamId: f.team_id, plan, repoSubstitutions: def.benchmarkRepoSubstitutions ?? [] };
  },
  /** POST .../benchmarks/{suite}/tasks, step 1: the active definition for makeTaskFromRun. */
  scoringTaskContext: async (kit, c) => {
    const f = factoryFor(kit, c);
    const active = await localStore(kit).activeDefinition(f.id);
    return {
      factory: { id: f.id, source: f.source },
      definition: active ? { benchmarks: active.definition.benchmarks } : null,
      files: active?.version.files ?? {},
    };
  },
  listDefinitionProposals: async (kit, c) => {
    const f = factoryFor(kit, c);
    const showAll = c.query["state"] === "all";
    const rows = all<{ id: string; state: string }>(
      kit.sql,
      "SELECT id, state FROM definition_versions WHERE factory_id = ? ORDER BY created_ms DESC, id DESC",
      f.id,
    );
    const items: unknown[] = [];
    for (const r of rows) {
      if (!isProposalState(r.state) || (!showAll && r.state !== "proposed")) continue;
      items.push(await proposalView(kit, f.active_version_id ?? "", (await getVersion(kit, r.id))!));
    }
    return listResponse(items, "");
  },
  approveDefinitionProposal: async (kit, c) => {
    const f = factoryFor(kit, c);
    const v = await proposalOf(kit, f.id, c.params["id"] ?? "");
    if (v.state !== "proposed") throw new OfError("conflict", `proposal ${v.id} is ${v.state}`);
    if ((f.active_version_id ?? "") !== (v.base_version_id ?? "")) {
      throw new OfError(
        "conflict",
        `the active definition changed after the proposal was made (base ${v.base_version_id ?? ""}, active ${f.active_version_id ?? ""}); ask the agent to propose the change again`,
      );
    }
    const actor = actorId(c.meta.principal);
    const applied = (await definitionApply(kit, c.meta, f.id, {
      files: v.files,
      author: actor,
      message: `${v.message ?? ""} (proposed by run ${v.author_run_id ?? ""}, approved by ${actor})`,
    })) as { version: { id: string } };
    const next = {
      ...v,
      state: "approved",
      applied_version_id: applied.version.id,
      reviewed_by: actor,
      reviewed_at: new Date().toISOString(),
    };
    saveProposal(kit, next);
    // The review item is resolved by the Worker (WorkDO owns it); the result says which.
    return {
      ...(await proposalView(kit, applied.version.id, next)),
      $resolve: {
        inbox_item_id: v.inbox_item_id ?? "",
        resolution: `approved: the change is applied as version ${applied.version.id}`,
        actor,
      },
    };
  },
  rejectDefinitionProposal: async (kit, c) => {
    const f = factoryFor(kit, c);
    const v = await proposalOf(kit, f.id, c.params["id"] ?? "");
    if (v.state !== "proposed") throw new OfError("conflict", `proposal ${v.id} is ${v.state}`);
    const actor = actorId(c.meta.principal);
    const comment = (optString(bodyObject(c.body, true), "comment") ?? "").trim();
    const next = {
      ...v,
      state: "rejected",
      reviewed_by: actor,
      review_comment: comment,
      reviewed_at: new Date().toISOString(),
    };
    saveProposal(kit, next);
    const resolution = comment ? `rejected: ${comment}` : "rejected";
    return {
      ...(await proposalView(kit, f.active_version_id ?? "", next)),
      $resolve: {
        inbox_item_id: v.inbox_item_id ?? "",
        resolution,
        actor,
        author_run_id: v.author_run_id ?? "",
        proposal_id: v.id,
      },
    };
  },
};

async function proposalOf(kit: TeamKit, factoryId: string, id: string): Promise<DefinitionVersion> {
  const v = await getVersion(kit, id);
  if (!v || v.factory_id !== factoryId || !isProposalState(v.state))
    throw new OfError("resource_not_found", `proposal ${id} not found`);
  return v;
}

function saveProposal(kit: TeamKit, v: DefinitionVersion): void {
  const { files: _f, ...meta } = v;
  kit.tx(() =>
    kit.sql.exec(
      "UPDATE definition_versions SET state = ?, data = ? WHERE id = ?",
      v.state ?? "",
      JSON.stringify(meta),
      v.id,
    ),
  );
}

async function proposalView(kit: TeamKit, activeId: string, v: DefinitionVersion): Promise<JsonObject> {
  const base = v.base_version_id ? await getVersion(kit, v.base_version_id) : undefined;
  return {
    id: v.id,
    factory_id: v.factory_id,
    state: v.state,
    base_version_id: v.base_version_id ?? "",
    author_run_id: v.author_run_id ?? "",
    inbox_item_id: v.inbox_item_id ?? "",
    message: v.message ?? "",
    diff: diffFiles(base?.files, v.files),
    applied_version_id: v.applied_version_id ?? "",
    reviewed_by: v.reviewed_by ?? "",
    review_comment: v.review_comment ?? "",
    ...(v.reviewed_at ? { reviewed_at: v.reviewed_at } : {}),
    created_at: v.created_at,
    base_is_active: (v.base_version_id ?? "") === activeId,
  };
}

export const ops: Record<string, TeamOp> = scoringOps;

export function setup(kit: TeamKit): void {
  // The scheduled self-improvement check of every factory with failed scores.
  kit.onTimerAsync("scoring.sweep", async () => {
    const ids = all<{ factory_id: string }>(
      kit.sql,
      "SELECT DISTINCT factory_id FROM scoring_scores WHERE passed = 0 AND improvement_run_id = ''",
    ).map((r) => r.factory_id);
    const more = await scoringSweep(kit.env, kit.teamId(), localStore(kit) as never, ids);
    if (more) kit.timers.setEarliest("scoring.sweep", "scoring.sweep", Date.now() + 15 * 60_000);
  });
}

export type { OpCall };
void newId;
