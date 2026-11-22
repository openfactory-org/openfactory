// In-memory ports for the scoring tests.
import {
  OfError,
  type CompiledDefinition,
  type Run,
  type RepoStart,
  type RunMessageRole,
  type Score,
  type Scorer,
  type WorkItem,
} from "@openfactory/core";
import type { BenchmarkPort } from "./service";
import type {
  DispatchRequest,
  EnginePort,
  FactoryInfo,
  InboxItemInput,
  JudgeRunRequest,
  RunsPort,
  ScoringStore,
} from "./types";

export const T0 = Date.UTC(2026, 2, 1, 12, 0, 0);

export class Clock {
  constructor(public ms = T0) {}
  now = (): number => this.ms;
  advance(ms: number): void {
    this.ms += ms;
  }
  iso(): string {
    return new Date(this.ms).toISOString();
  }
}

export const run = (r: Partial<Run> & Pick<Run, "id">): Run =>
  ({
    version: 1,
    team_id: "team_1",
    factory_id: "fac_1",
    state: "succeeded",
    prompt: "",
    config: {} as Run["config"],
    consumed_seq: 0,
    usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
    created_at: new Date(T0).toISOString(),
    ...r,
  }) as Run;

export class FakeWorld implements ScoringStore, RunsPort, EnginePort, BenchmarkPort {
  factories = new Map<string, FactoryInfo>();
  definition: CompiledDefinition | null = null;
  files: Record<string, string> = {};
  scorers: Scorer[] = [];
  scores: (Score & { factory_id: string })[] = [];
  kv = new Map<string, string>();
  users: { id: string; email: string; role: string }[] = [];
  runsById = new Map<string, Run>();
  messages = new Map<string, { role: RunMessageRole; content: string }[]>();
  workItems = new Map<string, WorkItem>();
  starts = new Map<string, RepoStart[]>();
  inboxOpen: { id: string; run_id?: string; work_item_id?: string }[] = [];
  // recordings
  dispatches: { req: DispatchRequest; key?: string }[] = [];
  judges: { req: JudgeRunRequest; runId: string }[] = [];
  cancels: string[] = [];
  followUps: string[] = [];
  inbox: InboxItemInput[] = [];
  applied: { files: Record<string, string>; actor: string; message: string }[] = [];
  benchRuns: [string, string][] = [];
  benchJudges: [string, string][] = [];
  failDispatchFor = "";
  /** Dispatches after this many wait for releaseHold() (tests of the chunked dispatch). */
  private holdAfter = Infinity;
  private holdPromise: Promise<void> | undefined;
  private releaseFn: (() => void) | undefined;
  inFlight = 0;
  private seq = 0;

  constructor(public clock = new Clock()) {
    this.factories.set("fac_1", {
      id: "fac_1",
      team_id: "team_1",
      name: "demo",
      source: "managed",
      active_version_id: "ver_1",
    });
    this.definition = { name: "demo", agents: [], benchmarks: [] } as unknown as CompiledDefinition;
  }

  /** Blocks every dispatch after the first `after` until releaseHold(). */
  hold(after: number): void {
    this.holdAfter = after;
    this.holdPromise = new Promise<void>((r) => (this.releaseFn = r));
  }
  releaseHold(): void {
    this.holdAfter = Infinity;
    this.releaseFn?.();
  }

  // ScoringStore
  async getFactory(id: string): Promise<FactoryInfo> {
    const f = this.factories.get(id);
    if (!f) throw new OfError("resource_not_found", `factory ${id} not found`);
    return f;
  }
  async activeDefinition(): Promise<{
    definition: CompiledDefinition;
    version: { versionId: string; files: Record<string, string> };
  } | null> {
    return this.definition ? { definition: this.definition, version: { versionId: "ver_1", files: this.files } } : null;
  }
  async listScorers(factoryId: string): Promise<Scorer[]> {
    return this.scorers.filter((s) => s.factory_id === factoryId);
  }
  async getScore(scorerId: string, runId: string): Promise<Score | null> {
    return this.scores.find((s) => s.scorer_id === scorerId && s.run_id === runId) ?? null;
  }
  async putScore(score: Score & { factory_id: string }): Promise<Score> {
    const i = this.scores.findIndex((s) => s.scorer_id === score.scorer_id && s.run_id === score.run_id);
    if (i >= 0) this.scores[i] = score;
    else this.scores.push(score);
    return score;
  }
  async listFailedScores(factoryId: string): Promise<{ score: Score; scorer: Scorer }[]> {
    const out: { score: Score; scorer: Scorer }[] = [];
    for (const s of this.scores) {
      const scorer = this.scorers.find((x) => x.id === s.scorer_id && x.factory_id === factoryId);
      if (scorer?.self_improvement && !s.passed && !s.improvement_run_id) out.push({ score: s, scorer });
    }
    return out;
  }
  async markScoresImproved(ids: string[], improvementRunId: string): Promise<void> {
    for (const s of this.scores) if (ids.includes(s.id)) s.improvement_run_id = improvementRunId;
  }
  async kvGet(key: string): Promise<string> {
    return this.kv.get(key) ?? "";
  }
  async kvSet(key: string, value: string): Promise<void> {
    this.kv.set(key, value);
  }
  async kvClaim(key: string, value: string): Promise<boolean> {
    if (this.kv.get(key)) return false;
    this.kv.set(key, value);
    return true;
  }
  async listUsers(): Promise<{ id: string; email: string; role: string }[]> {
    return this.users;
  }

  // RunsPort
  async getRun(id: string): Promise<Run | null> {
    return this.runsById.get(id) ?? null;
  }
  async getWorkItem(id: string): Promise<WorkItem | null> {
    return this.workItems.get(id) ?? null;
  }
  async listChildren(id: string): Promise<Run[]> {
    return [...this.runsById.values()].filter((r) => r.parent_run_id === id);
  }
  async listMessages(id: string): Promise<{ role: RunMessageRole; content: string }[]> {
    return this.messages.get(id) ?? [];
  }
  async loadStartCommits(id: string): Promise<RepoStart[]> {
    return this.starts.get(id) ?? [];
  }
  async listOpenInbox(): Promise<{ id: string; run_id?: string; work_item_id?: string }[]> {
    return this.inboxOpen;
  }

  // helpers
  put(r: Run): Run {
    this.runsById.set(r.id, r);
    return r;
  }
  say(runId: string, role: RunMessageRole, content: string): void {
    const l = this.messages.get(runId) ?? [];
    l.push({ role, content });
    this.messages.set(runId, l);
  }

  // EnginePort
  async dispatch(req: DispatchRequest, key?: string): Promise<{ workItemId?: string; runId: string }> {
    if (this.failDispatchFor && req.agentName === this.failDispatchFor)
      throw new OfError("internal_error", "dispatch failed on purpose");
    if (this.holdPromise && this.dispatches.length >= this.holdAfter) {
      this.inFlight++;
      await this.holdPromise;
      this.inFlight--;
    }
    this.dispatches.push(key === undefined ? { req } : { req, key });
    const n = ++this.seq;
    const wiId = `wi_${n}`;
    const runId = `run_disp${n}`;
    this.workItems.set(wiId, { id: wiId, factory_id: req.factoryId } as WorkItem);
    this.put(
      run({
        id: runId,
        work_item_id: wiId,
        factory_id: req.factoryId,
        agent_name: req.agentName ?? "foreman",
        state: "running",
        prompt: req.prompt,
        metadata: req.metadata ?? {},
        triggered_by: req.triggeredBy ?? "",
      }),
    );
    return { workItemId: wiId, runId };
  }
  async startJudge(req: JudgeRunRequest, runId: string): Promise<{ runId: string }> {
    this.judges.push({ req, runId });
    if (!this.runsById.has(runId)) {
      this.put(
        run({
          id: runId,
          factory_id: req.factoryId,
          agent_name: req.name,
          name: req.name,
          state: "queued",
          prompt: req.prompt,
          metadata: req.metadata ?? {},
          triggered_by: req.triggeredBy ?? "",
        }),
      );
    }
    return { runId };
  }
  async cancel(runId: string, actor: string): Promise<void> {
    this.cancels.push(`${runId} by ${actor}`);
    const r = this.runsById.get(runId);
    if (r && r.state !== "succeeded" && r.state !== "failed") r.state = "cancelled";
  }
  async followUp(runId: string, message: string, actor: string): Promise<void> {
    this.followUps.push(`${runId}: ${message} (${actor})`);
  }
  async resolveInbox(): Promise<void> {}
  async applyDefinition(
    _f: string,
    files: Record<string, string>,
    actor: string,
    message: string,
  ): Promise<{ id: string }> {
    this.applied.push({ files, actor, message });
    return { id: "ver_applied" };
  }
  async checkDefinition(
    _f: string,
    files: Record<string, string>,
  ): Promise<{ valid: boolean; errors: { file: string; message: string }[] }> {
    return (files["factory.yaml"] ?? "").includes("INVALID")
      ? { valid: false, errors: [{ file: "factory.yaml", message: "broken on purpose" }] }
      : { valid: true, errors: [] };
  }
  async createInboxItem(item: InboxItemInput): Promise<void> {
    if (!this.inbox.some((i) => i.id === item.id)) this.inbox.push(item);
  }

  // BenchmarkPort
  async trialRunUpdate(benchId: string, runId: string): Promise<void> {
    this.benchRuns.push([benchId, runId]);
  }
  async trialJudgeUpdate(benchId: string, judgeRunId: string): Promise<void> {
    this.benchJudges.push([benchId, judgeRunId]);
  }
}

/** Finishes a judge run with a result and usage. */
export function finishJudge(w: FakeWorld, id: string, state: Run["state"], result: string, cost: number): Run {
  const r = w.runsById.get(id)!;
  r.state = state;
  r.result = result;
  r.usage = { input_tokens: 10, output_tokens: 5, cost_usd: cost, duration_ms: 0 };
  r.finished_at = w.clock.iso();
  return r;
}
