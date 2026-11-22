// The scoring service: automatic and manual scoring with judge runs,
// Self-improvement and the review of its pull requests. Port of service.go,
// scorer.go, judge.go, improve.go and improve_review.go. Benchmarks live in
// BenchmarkDO.
//
// Differences from Go, by design (plan rules 1, 3, 11):
// - No in-process locks and no direct model call. DO serialization and
//   idempotent calls replace the locks: a judge run has an ID that is a hash
//   of (scorer, run, attempt), a Self-improvement dispatch has an idempotency
//   key, and the store upserts a score by (scorer, run).
// - The state of a service call is in TeamDO (score rows, markers), reached
//   through the ScoringStore port.
import { OfError, base32, isOfError, type Run, type Score, type Scorer, type WorkItem } from "@openfactory/core";
import { buildTranscript } from "./transcript";
import {
  improvementPrompt,
  improvementSettings,
  groupFailures,
  eligibleReviewers,
  regressionsSection,
  REVIEWER_NONE,
  thresholdReached,
  type Failure,
  type ImprovementSettings,
} from "./improve";
import { isJudgeRun, judgeAnswer, judgeRequest, scorerExec, scorerName, treeUsage } from "./judge";
import { resolveLabel, shouldSample } from "./sample";
import { oneLine } from "./text";
import {
  DEFAULT_MAX_FAILURE_AGE_MS,
  DEFAULT_MAX_TRANSCRIPT_BYTES,
  JUDGE_SCORE,
  JUDGE_TRIAL,
  MAX_ANCESTOR_DEPTH,
  META_BENCHMARK_RUN,
  META_JUDGE,
  META_JUDGE_SCORED_RUN,
  META_JUDGE_SCORER,
  META_JUDGE_BENCHMARK,
  META_SELF_IMPROVEMENT,
  META_SELF_IMPROVEMENT_AGENT,
  SOURCE_SELF_IMPROVEMENT,
  TRIGGERED_BY_SELF_IMPROVEMENT,
  TRIGGERED_BY_SCORER,
  finishedForScoring,
  isTerminalState,
  type DispatchRequest,
  type EnginePort,
  type FactoryInfo,
  type RunsPort,
  type ScoringOptions,
  type ScoringStore,
} from "./types";
import type { Usage } from "@openfactory/core";

/** What the service tells BenchmarkDO. */
export interface BenchmarkPort {
  /** A root run of the benchmark finished or waits for input. */
  trialRunUpdate(benchId: string, runId: string): Promise<void>;
  /** A judge run of a trial finished or waits for input. */
  trialJudgeUpdate(benchId: string, judgeRunId: string): Promise<void>;
}

export interface ScoringDeps {
  store: ScoringStore;
  runs: RunsPort;
  engine: EnginePort;
  benchmarks: BenchmarkPort;
  opts?: ScoringOptions;
}

/** The marker of the judge run of (scorer, run). `attempt` only grows, so a judge run ID is never reused. */
export interface JudgeMarker {
  attempt: number;
  run_id: string;
  active: boolean;
}

export const pendingJudgeKey = (scorerId: string, runId: string): string => `scoring:judge:${scorerId}:${runId}`;
export const improveTrackKey = (workItemId: string): string => `scoring:improve:wi:${workItemId}`;

export function improvePRKey(workItemId: string, pr: { repo: string; number: number; url: string }): string {
  const id = pr.repo && pr.number ? `${pr.repo.toLowerCase()}#${pr.number}` : pr.url;
  return `scoring:improve:pr:${workItemId}:${id}`;
}

/** What the service remembers about a Self-improvement run until its pull requests are reviewed. */
export interface ImprovementTrack {
  reviewer_id?: string;
  run_id: string;
  agent: string;
  regressions?: string;
}

/** A routed run ID from a hash: `run_` + token (w + 15) + 12 characters. The same input gives the same ID. */
export async function deterministicRunId(...parts: string[]): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(parts.join("|"))));
  const b = base32(digest);
  return `run_w${b.slice(0, 15)}${b.slice(15, 27)}`;
}

export async function sha256Hex(s: string): Promise<string> {
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)));
  return [...d].map((x) => x.toString(16).padStart(2, "0")).join("");
}

const nowIso = (ms: number): string => new Date(ms).toISOString();

export class ScoringService {
  private readonly store: ScoringStore;
  private readonly runs: RunsPort;
  private readonly engine: EnginePort;
  private readonly benchmarks: BenchmarkPort;
  private readonly o: Required<Pick<ScoringOptions, "maxTranscriptBytes" | "maxFailureAgeMs">> & ScoringOptions;

  constructor(deps: ScoringDeps) {
    this.store = deps.store;
    this.runs = deps.runs;
    this.engine = deps.engine;
    this.benchmarks = deps.benchmarks;
    const o = deps.opts ?? {};
    this.o = {
      ...o,
      maxTranscriptBytes:
        o.maxTranscriptBytes && o.maxTranscriptBytes > 0 ? o.maxTranscriptBytes : DEFAULT_MAX_TRANSCRIPT_BYTES,
      maxFailureAgeMs:
        o.maxFailureAgeMs === undefined || o.maxFailureAgeMs === 0 ? DEFAULT_MAX_FAILURE_AGE_MS : o.maxFailureAgeMs,
    };
  }

  private now(): number {
    return (this.o.now ?? Date.now)();
  }

  private newId(prefix: string): string {
    if (this.o.newId) return this.o.newId(prefix);
    return `${prefix}_${base32(crypto.getRandomValues(new Uint8Array(12))).slice(0, 20)}`;
  }

  private randomInt(n: number): number {
    if (this.o.randomInt) return this.o.randomInt(n);
    return crypto.getRandomValues(new Uint32Array(1))[0]! % n;
  }

  private runUrl(runId: string): string {
    return this.o.publicUrl ? `${this.o.publicUrl.replace(/\/+$/, "")}/runs/${runId}` : "";
  }

  private log(level: "info" | "warn" | "error", msg: string, f?: Record<string, unknown>): void {
    this.o.log?.[level](msg, f);
  }

  // ---------- run updates ----------

  /**
   * Processes the end of a turn or a run (the `scoring.runFinished` job): it
   * advances a benchmark the run is a trial of, reviews the pull requests of a
   * Self-improvement run, and scores production runs.
   */
  async handleRunUpdate(runId: string): Promise<void> {
    const run = await this.runs.getRun(runId);
    if (!run || !finishedForScoring(run.state)) return;
    const md = run.metadata ?? {};
    if (md[META_JUDGE]) {
      // A judge run is never scored itself.
      await this.handleJudgeUpdate(run);
      return;
    }
    if (md[META_SELF_IMPROVEMENT] === "true" && run.work_item_id) {
      try {
        await this.reviewImprovementPRs(run);
      } catch (e) {
        this.log("error", "self-improvement review of pull requests", { run_id: run.id, err: String(e) });
      }
    }
    const bid = md[META_BENCHMARK_RUN];
    if (bid) {
      // Root trial runs carry the benchmark id; descendants inherit metadata and
      // are ignored by BenchmarkDO (unknown run ID).
      await this.benchmarks.trialRunUpdate(bid, run.id);
      return;
    }
    if (run.state === "error") return; // scorers ignore runs in error
    await this.autoScore(run);
  }

  private async handleJudgeUpdate(judge: Run): Promise<void> {
    switch (judge.metadata?.[META_JUDGE]) {
      case JUDGE_SCORE:
        await this.finishScoreJudge(judge);
        return;
      case JUDGE_TRIAL: {
        const bid = judge.metadata[META_JUDGE_BENCHMARK];
        if (bid) await this.benchmarks.trialJudgeUpdate(bid, judge.id);
        return;
      }
    }
  }

  // ---------- automatic scoring ----------

  private async autoScore(run: Run): Promise<void> {
    if (run.state !== "succeeded" && run.state !== "failed") return;
    if (!run.agent_name || isJudgeRun(run)) return;
    const scorers = await this.store.listScorers(run.factory_id);
    const eligible: Scorer[] = [];
    for (const sc of scorers) {
      if (!(sc.agents ?? []).some((a) => a.name === run.agent_name)) continue;
      if (!(await shouldSample(sc.id, run.id, sc.sampling_rate))) continue;
      eligible.push(sc);
    }
    if (eligible.length === 0) return;
    // Benchmark trials (and their descendants) are scored by the benchmark.
    if (await this.isBenchmarkRun(run)) return;
    const errs: string[] = [];
    for (const sc of eligible) {
      try {
        const { score, created } = await this.scoreOnce(sc, run, false);
        if (created && score && !score.passed && sc.self_improvement) {
          await this.checkAgent(run.factory_id, run.agent_name, false);
        }
      } catch (e) {
        errs.push(`scorer ${sc.slug}: ${errMsg(e)}`);
      }
    }
    if (errs.length > 0) throw new OfError("internal_error", errs.join("; "));
  }

  /** Reports whether the run or any ancestor was started by a benchmark. */
  async isBenchmarkRun(run: Run): Promise<boolean> {
    let cur: Run = run;
    for (let depth = 0; depth < MAX_ANCESTOR_DEPTH; depth++) {
      if ((cur.triggered_by ?? "").startsWith("benchmark:") || cur.metadata?.[META_BENCHMARK_RUN]) return true;
      if (!cur.parent_run_id) return false;
      const parent = await this.runs.getRun(cur.parent_run_id);
      if (!parent) return false;
      cur = parent;
    }
    return false;
  }

  /**
   * Scores one run on demand with the given scorer (ID or slug), whatever the
   * sampling and whether the scorer selects the run's agent. A new result
   * replaces the previous one of that scorer. The result is a pending score
   * without an ID (judge_run_id names the judge run); the score is stored when
   * the judge run finishes.
   */
  async scoreRun(scorerIdOrSlug: string, runId: string): Promise<Score> {
    const run = await this.runs.getRun(runId);
    if (!run) throw new OfError("resource_not_found", `run ${runId} not found`);
    const sc = await this.findScorer(run.factory_id, scorerIdOrSlug);
    if (isJudgeRun(run)) throw new OfError("invalid_request", "a judge run is not scored");
    const { score } = await this.scoreOnce(sc, run, true);
    if (score.id === "") return score; // the judge run works
    if (!score.passed && sc.self_improvement && run.agent_name) {
      try {
        await this.checkAgent(run.factory_id, run.agent_name, false);
      } catch (e) {
        this.log("error", "self-improvement check after manual score", { run_id: runId, err: String(e) });
      }
    }
    return score;
  }

  async findScorer(factoryId: string, idOrSlug: string): Promise<Scorer> {
    const scorers = await this.store.listScorers(factoryId);
    const sc =
      scorers.find((s) => s.id === idOrSlug) ?? scorers.find((s) => s.slug === idOrSlug || s.name === idOrSlug);
    if (!sc) throw new OfError("resource_not_found", `scorer ${JSON.stringify(idOrSlug)} not found`);
    return sc;
  }

  private async getMarker(scorerId: string, runId: string): Promise<JudgeMarker | undefined> {
    const v = await this.store.kvGet(pendingJudgeKey(scorerId, runId));
    if (!v) return undefined;
    try {
      return JSON.parse(v) as JudgeMarker;
    } catch {
      return undefined;
    }
  }

  private async setMarker(scorerId: string, runId: string, m: JudgeMarker): Promise<void> {
    await this.store.kvSet(pendingJudgeKey(scorerId, runId), JSON.stringify(m));
  }

  /**
   * Starts the judge of (scorer, run) unless a result exists (replace = false)
   * or a judge works. Returns a pending score (no ID, created = false).
   */
  private async scoreOnce(sc: Scorer, run: Run, replace: boolean): Promise<{ score: Score; created: boolean }> {
    const existing = await this.store.getScore(sc.id, run.id);
    if (existing && !replace) return { score: existing, created: false };
    const marker = await this.getMarker(sc.id, run.id);
    let judgeId: string;
    const pending = marker?.active ? await this.runs.getRun(marker.run_id) : null;
    if (marker?.active && (!pending || !finishedForScoring(pending.state))) {
      // A judge works, or its start did not finish: start (or continue) the same run.
      judgeId = marker.run_id;
      if (!pending) await this.startScoreJudge(sc, run, judgeId);
    } else {
      // No judge, or a finished judge whose update was lost: a new judge run.
      const attempt = (marker?.attempt ?? 0) + 1;
      judgeId = await deterministicRunId("judge", sc.id, run.id, String(attempt));
      await this.setMarker(sc.id, run.id, { attempt, run_id: judgeId, active: true });
      await this.startScoreJudge(sc, run, judgeId);
    }
    return {
      score: {
        id: "",
        scorer_id: sc.id,
        run_id: run.id,
        agent_name: run.agent_name ?? "",
        label: "",
        score: 0,
        passed: false,
        rationale: "",
        created_at: "",
        judge_run_id: judgeId,
      },
      created: false,
    };
  }

  private async startScoreJudge(sc: Scorer, run: Run, judgeId: string): Promise<void> {
    const include = (sc.agents ?? []).find((a) => a.name === run.agent_name)?.includeDescendants ?? false;
    const req = await judgeRequest(this.runs, {
      factoryId: run.factory_id,
      name: scorerName(sc),
      rubric: sc.rubric,
      labels: sc.labels ?? [],
      exec: scorerExec(sc),
      scored: run,
      includeDescendants: include,
    });
    req.triggeredBy = TRIGGERED_BY_SCORER + sc.id;
    req.metadata = { [META_JUDGE]: JUDGE_SCORE, [META_JUDGE_SCORER]: sc.id, [META_JUDGE_SCORED_RUN]: run.id };
    await this.engine.startJudge(req, judgeId);
    this.log("info", "judge run started", { scorer: sc.slug, run_id: run.id, judge_run_id: judgeId });
  }

  // ---------- judge results ----------

  private async finishScoreJudge(judge: Run): Promise<void> {
    const scorerId = judge.metadata?.[META_JUDGE_SCORER] ?? "";
    const runId = judge.metadata?.[META_JUDGE_SCORED_RUN] ?? "";
    if (!scorerId || !runId) return;
    const marker = await this.getMarker(scorerId, runId);
    if (!marker || !marker.active || marker.run_id !== judge.id) return; // stored already, or a newer judge replaced this one
    const scored = await this.runs.getRun(runId);
    if (!scored) throw new OfError("resource_not_found", `run ${runId} not found`);
    let sc: Scorer;
    try {
      sc = await this.findScorer(scored.factory_id, scorerId);
    } catch (e) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      throw e;
    }
    const usage = await this.treeUsage(judge);
    let answer: { label: string; rationale: string } | undefined;
    let jerr: string | undefined;
    try {
      answer = await this.judgeAnswer(judge);
    } catch (e) {
      jerr = errMsg(e);
    }
    await this.stopJudge(judge);
    if (!answer) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      this.log("error", "judge run gave no label", {
        scorer: sc.slug,
        run_id: runId,
        judge_run_id: judge.id,
        err: jerr,
      });
      return;
    }
    const l = resolveLabel(sc.labels, answer.label);
    if (!l) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      this.log("error", "judge run returned an undeclared label", {
        scorer: sc.slug,
        run_id: runId,
        judge_run_id: judge.id,
        label: answer.label,
      });
      return;
    }
    const existing = await this.store.getScore(sc.id, runId);
    const score: Score & { factory_id: string } = {
      id: existing?.id ?? this.newId("score"),
      factory_id: scored.factory_id,
      scorer_id: sc.id,
      run_id: runId,
      agent_name: scored.agent_name ?? "",
      label: l.value,
      score: l.score,
      passed: l.score >= sc.passing_score,
      rationale: answer.rationale,
      created_at: nowIso(this.now()),
      judge_run_id: judge.id,
      judge_usage: usage,
    };
    if (existing?.improvement_run_id) score.improvement_run_id = existing.improvement_run_id;
    await this.store.putScore(score);
    await this.setMarker(scorerId, runId, { ...marker, active: false });
    this.log("info", "run scored", {
      scorer: sc.slug,
      run_id: runId,
      judge_run_id: judge.id,
      label: score.label,
      passed: score.passed,
    });
    if (!score.passed && sc.self_improvement && scored.agent_name) {
      try {
        await this.checkAgent(scored.factory_id, scored.agent_name, false);
      } catch (e) {
        this.log("error", "self-improvement check after judge run", { run_id: runId, err: String(e) });
      }
    }
  }

  /** Cancels a judge run that waits for input. A judge has one turn. */
  private async stopJudge(judge: Run): Promise<void> {
    if (isTerminalState(judge.state)) return;
    try {
      await this.engine.cancel(judge.id, "scoring");
    } catch (e) {
      if (!(isOfError(e) && e.code === "resource_not_found"))
        this.log("warn", "stop judge run", { judge_run_id: judge.id, err: String(e) });
    }
  }

  /** Reads the label and rationale from a finished judge run. */
  judgeAnswer(judge: Run): Promise<{ label: string; rationale: string }> {
    return judgeAnswer(this.runs, judge);
  }

  /** The usage of a run and its descendants. */
  treeUsage(run: Run): Promise<Usage> {
    return treeUsage(this.runs, run);
  }

  /** The transcript of a run for a judge, capped. */
  buildTranscript(run: Run, includeDescendants: boolean): Promise<string> {
    return buildTranscript(this.runs, run, includeDescendants, 4000, this.o.maxTranscriptBytes);
  }

  // ---------- Self-improvement ----------

  /** The scheduled check of one factory: every agent that reaches the trigger rule gets one run. */
  async checkSelfImprovement(factoryId: string): Promise<Run[]> {
    return this.check(factoryId, "", false);
  }

  /** "Run now": any agent with at least one unreviewed failure gets a run. An empty agent means every agent. */
  async triggerSelfImprovement(factoryId: string, agent: string): Promise<Run[]> {
    return this.check(factoryId, agent, true);
  }

  /** Unreviewed failures per agent. */
  async unreviewedFailures(factoryId: string): Promise<Map<string, Failure[]>> {
    return groupFailures(await this.store.listFailedScores(factoryId));
  }

  private async check(factoryId: string, onlyAgent: string, adHoc: boolean): Promise<Run[]> {
    const failures = await this.unreviewedFailures(factoryId);
    const agents = [...failures.keys()].filter((a) => onlyAgent === "" || a === onlyAgent).sort();
    const runs: Run[] = [];
    const errs: string[] = [];
    for (const a of agents) {
      try {
        const r = await this.checkAgent(factoryId, a, adHoc);
        if (r) runs.push(r);
      } catch (e) {
        errs.push(`agent ${a}: ${errMsg(e)}`);
      }
    }
    if (errs.length > 0) throw new OfError("internal_error", errs.join("; "));
    return runs;
  }

  /** Starts a Self-improvement run for one agent when its trigger holds. Returns the run or undefined. */
  private async checkAgent(factoryId: string, agent: string, adHoc: boolean): Promise<Run | undefined> {
    const failures = (await this.unreviewedFailures(factoryId)).get(agent) ?? [];
    if (failures.length === 0) return undefined;
    const factory = await this.store.getFactory(factoryId);
    let active: Awaited<ReturnType<ScoringStore["activeDefinition"]>> = null;
    try {
      active = await this.store.activeDefinition(factoryId);
    } catch (e) {
      this.log("warn", "self-improvement: using default settings", { factory_id: factoryId, err: String(e) });
    }
    const cfg = improvementSettings(active?.definition, {
      threshold: this.o.defaultFailedRunThreshold,
      analysisModel: this.o.analysisModel ?? "",
    });
    if (!adHoc && !thresholdReached(failures, cfg.threshold, this.now(), this.o.maxFailureAgeMs)) return undefined;
    return this.startImprovement(factory, active?.version.files, agent, failures, cfg);
  }

  private async startImprovement(
    f: FactoryInfo,
    files: Record<string, string> | undefined,
    agent: string,
    failures: Failure[],
    cfg: ImprovementSettings,
  ): Promise<Run> {
    const runs = new Map<string, Run>();
    for (const fl of failures) {
      const r = await this.runs.getRun(fl.score.run_id);
      if (r) runs.set(r.id, r);
    }
    const prompt = await improvementPrompt({
      factory: f as Parameters<typeof improvementPrompt>[0]["factory"],
      ...(files ? { files } : {}),
      agent,
      failures,
      runs,
      runUrl: (id) => this.runUrl(id),
      runsPort: this.runs,
    });
    const idempotencyKey = `improve:${f.id}:${agent}:${await sha256Hex(
      failures
        .map((x) => x.score.id)
        .sort()
        .join(","),
    )}`;
    const req: DispatchRequest = {
      factoryId: f.id,
      prompt,
      source: {
        provider: SOURCE_SELF_IMPROVEMENT,
        kind: "task",
        external_id: `self-improvement/${agent}/${await sha256Hex(idempotencyKey).then((h) => h.slice(0, 20))}`,
        title: "Self-improvement: " + agent,
      },
      title: `Self-improvement: ${agent} (${failures.length} failing runs)`,
      triggeredBy: TRIGGERED_BY_SELF_IMPROVEMENT,
      metadata: { [META_SELF_IMPROVEMENT]: "true", [META_SELF_IMPROVEMENT_AGENT]: agent },
    };
    if (this.o.improvementAgent) req.agentName = this.o.improvementAgent;
    if (cfg.analysisModel) req.overrides = { model: cfg.analysisModel };
    const out = await this.engine.dispatch(req, idempotencyKey);
    const run = await this.runs.getRun(out.runId);
    if (!run) throw new OfError("internal_error", "dispatch self-improvement run: the engine returned no run");
    const errs: string[] = [];
    try {
      await this.store.markScoresImproved(
        failures.map((x) => x.score.id),
        run.id,
      );
    } catch (e) {
      errs.push(`mark scores: ${errMsg(e)}`);
    }
    // The review item is not created now, because no change exists yet. A managed
    // definition gets it when the run proposes a change. A pull request gets it
    // when the run has linked the pull request to its work item.
    if (out.workItemId) {
      let reviewer = "";
      try {
        reviewer = await this.pickReviewer(cfg);
      } catch (e) {
        this.log("warn", "self-improvement: no reviewer selected", { factory_id: f.id, err: String(e) });
      }
      const track: ImprovementTrack = {
        reviewer_id: reviewer,
        run_id: run.id,
        agent,
        regressions: regressionsSection(failures, (id) => this.runUrl(id)),
      };
      try {
        await this.store.kvSet(improveTrackKey(out.workItemId), JSON.stringify(track));
      } catch (e) {
        errs.push(`record self-improvement run: ${errMsg(e)}`);
      }
    }
    this.log("info", "self-improvement run started", {
      factory_id: f.id,
      agent,
      run_id: run.id,
      failures: failures.length,
    });
    if (errs.length > 0) this.log("error", "self-improvement bookkeeping", { run_id: run.id, err: errs.join("; ") });
    return run;
  }

  /** Randomly selects one eligible reviewer per reviewerType. "" for none. */
  async pickReviewer(cfg: Pick<ImprovementSettings, "reviewerType" | "reviewerEmails">): Promise<string> {
    if (cfg.reviewerType === REVIEWER_NONE) return "";
    const pool = eligibleReviewers(await this.store.listUsers(), cfg.reviewerType, cfg.reviewerEmails);
    if (pool.length === 0)
      throw new Error(`no eligible reviewers for reviewerType ${JSON.stringify(cfg.reviewerType)}`);
    return pool[this.randomInt(pool.length)]!.id;
  }

  /** The reviewer of a Self-improvement run: the one chosen when it started, else a new pick. */
  async reviewerFor(run: Pick<Run, "work_item_id" | "factory_id">): Promise<string> {
    if (run.work_item_id) {
      const track = await this.loadTrack(run.work_item_id);
      if (track) return track.reviewer_id ?? "";
    }
    try {
      const active = await this.store.activeDefinition(run.factory_id);
      return await this.pickReviewer(
        improvementSettings(active?.definition, {
          threshold: this.o.defaultFailedRunThreshold,
          analysisModel: this.o.analysisModel ?? "",
        }),
      );
    } catch {
      return "";
    }
  }

  private async loadTrack(workItemId: string): Promise<ImprovementTrack | undefined> {
    const v = await this.store.kvGet(improveTrackKey(workItemId));
    if (!v) return undefined;
    try {
      return JSON.parse(v) as ImprovementTrack;
    } catch (e) {
      throw new OfError("internal_error", `decode improvement track of ${workItemId}: ${errMsg(e)}`);
    }
  }

  // ---------- review of Self-improvement pull requests ----------

  /**
   * Creates the review item of each open pull request that a Self-improvement
   * run linked to its work item, and asks the reviewer for a review on the
   * forge. It runs when a run of the work item updates.
   */
  async reviewImprovementPRs(run: Run): Promise<void> {
    if (!run.work_item_id) return;
    const track = await this.loadTrack(run.work_item_id);
    if (!track) return;
    const wi = await this.runs.getWorkItem(run.work_item_id);
    if (!wi) throw new OfError("resource_not_found", `work item ${run.work_item_id} not found`);
    const f = await this.store.getFactory(wi.factory_id);
    const errs: string[] = [];
    for (const pr of wi.pull_requests ?? []) {
      if (pr.state === "merged" || pr.state === "closed") continue;
      const key = improvePRKey(wi.id, pr);
      // The claim is atomic in TeamDO: two deliveries create one item.
      if (!(await this.store.kvClaim(key, "1"))) continue;
      try {
        await this.createPRReviewItem(f, wi, track, pr);
      } catch (e) {
        await this.store.kvSet(key, "");
        errs.push(errMsg(e));
      }
    }
    if (errs.length > 0) throw new OfError("internal_error", errs.join("; "));
  }

  private async createPRReviewItem(
    f: FactoryInfo,
    wi: WorkItem,
    track: ImprovementTrack,
    pr: NonNullable<WorkItem["pull_requests"]>[number],
  ): Promise<void> {
    const ref = pr.repo && pr.number ? `${pr.repo}#${pr.number}` : pr.url;
    let note = "No reviewer was chosen: reviewerType is none, or no team member is eligible.";
    if (track.reviewer_id) {
      note = "The item is for the reviewer that was chosen from the pool.";
      if (!this.o.requestReview) {
        note += " This server does not request reviews on the forge.";
      } else {
        try {
          await this.o.requestReview(f.team_id, pr, track.reviewer_id);
          note += " A review is requested from the reviewer on the pull request.";
        } catch (e) {
          this.log("warn", "self-improvement: review request failed", {
            factory_id: f.id,
            pull_request: ref,
            reviewer: track.reviewer_id,
            err: String(e),
          });
          note += ` A review could not be requested on the forge (${oneLine(errMsg(e))}). Add the reviewer on the pull request by hand.`;
        }
      }
    }
    let body = `A Self-improvement run for agent **${track.agent}** opened the pull request [${ref}](${pr.url}). `;
    body += "Review it with the same standards as a human-authored change. Nothing is adopted without review.\n\n";
    body += note + "\n\n";
    body += track.regressions ?? "";
    await this.engine.createInboxItem({
      // Deterministic, so a retry of this step creates the same item.
      id: await inboxIdFor(wi.id, ref),
      factory_id: f.id,
      work_item_id: wi.id,
      run_id: track.run_id,
      kind: "review",
      title: "Review self-improvement pull request " + ref,
      body,
      assignee_id: track.reviewer_id ?? "",
      created_at: nowIso(this.now()),
    });
  }
}

async function inboxIdFor(workItemId: string, ref: string): Promise<string> {
  // `inb_` + the partition token of the work item + 12 characters of a hash.
  const m = /^[a-z]+_([wib][a-z2-7]{15})/.exec(workItemId);
  const token = m?.[1] ?? "w" + base32(new TextEncoder().encode(workItemId)).slice(0, 15).padEnd(15, "a");
  const d = new Uint8Array(
    await crypto.subtle.digest("SHA-256", new TextEncoder().encode(`inbox|${workItemId}|${ref}`)),
  );
  return `inb_${token}${base32(d).slice(0, 12)}`;
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
