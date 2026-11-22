// Self-improvement: groups the unreviewed failures of an agent, starts one
// improvement run when the trigger rule holds, and creates the review items of
// the pull requests the run opens. Port of improve.go and improve_review.go.
//
// A dispatch has an idempotency key over the failing scores, and the review
// item of a pull request is claimed atomically in TeamDO, so a repeated call
// starts no second run and creates no second item.
import { OfError, base32, oneLine, sha256, sha256Hex, utf8, type Run, type WorkItem } from "@openfactory/core";
import { errMsg, type ScoringContext } from "./context";
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
import {
  META_SELF_IMPROVEMENT,
  META_SELF_IMPROVEMENT_AGENT,
  SOURCE_SELF_IMPROVEMENT,
  TRIGGERED_BY_SELF_IMPROVEMENT,
  type DispatchRequest,
  type EnginePort,
  type FactoryInfo,
  type RunsPort,
  type ScoringStore,
} from "./types";

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

export interface SelfImprovementDeps {
  store: ScoringStore;
  runs: RunsPort;
  engine: EnginePort;
  ctx: ScoringContext;
}

export class SelfImprovementCoordinator {
  private readonly store: ScoringStore;
  private readonly runs: RunsPort;
  private readonly engine: EnginePort;
  private readonly ctx: ScoringContext;

  constructor(deps: SelfImprovementDeps) {
    this.store = deps.store;
    this.runs = deps.runs;
    this.engine = deps.engine;
    this.ctx = deps.ctx;
  }

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
  async checkAgent(factoryId: string, agent: string, adHoc: boolean): Promise<Run | undefined> {
    const failures = (await this.unreviewedFailures(factoryId)).get(agent) ?? [];
    if (failures.length === 0) return undefined;
    const factory = await this.store.getFactory(factoryId);
    let active: Awaited<ReturnType<ScoringStore["activeDefinition"]>> = null;
    try {
      active = await this.store.activeDefinition(factoryId);
    } catch (e) {
      this.ctx.log("warn", "self-improvement: using default settings", { factory_id: factoryId, err: String(e) });
    }
    const cfg = this.settings(active?.definition);
    if (!adHoc && !thresholdReached(failures, cfg.threshold, this.ctx.now(), this.ctx.o.maxFailureAgeMs))
      return undefined;
    return this.startImprovement(factory, active?.version.files, agent, failures, cfg);
  }

  private settings(definition: Parameters<typeof improvementSettings>[0]): ImprovementSettings {
    return improvementSettings(definition, {
      threshold: this.ctx.o.defaultFailedRunThreshold,
      analysisModel: this.ctx.o.analysisModel ?? "",
    });
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
      runUrl: (id) => this.ctx.runUrl(id),
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
    if (this.ctx.o.improvementAgent) req.agentName = this.ctx.o.improvementAgent;
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
        this.ctx.log("warn", "self-improvement: no reviewer selected", { factory_id: f.id, err: String(e) });
      }
      const track: ImprovementTrack = {
        reviewer_id: reviewer,
        run_id: run.id,
        agent,
        regressions: regressionsSection(failures, (id) => this.ctx.runUrl(id)),
      };
      try {
        await this.store.kvSet(improveTrackKey(out.workItemId), JSON.stringify(track));
      } catch (e) {
        errs.push(`record self-improvement run: ${errMsg(e)}`);
      }
    }
    this.ctx.log("info", "self-improvement run started", {
      factory_id: f.id,
      agent,
      run_id: run.id,
      failures: failures.length,
    });
    if (errs.length > 0)
      this.ctx.log("error", "self-improvement bookkeeping", { run_id: run.id, err: errs.join("; ") });
    return run;
  }

  /** Randomly selects one eligible reviewer per reviewerType. "" for none. */
  async pickReviewer(cfg: Pick<ImprovementSettings, "reviewerType" | "reviewerEmails">): Promise<string> {
    if (cfg.reviewerType === REVIEWER_NONE) return "";
    const pool = eligibleReviewers(await this.store.listUsers(), cfg.reviewerType, cfg.reviewerEmails);
    if (pool.length === 0)
      throw new Error(`no eligible reviewers for reviewerType ${JSON.stringify(cfg.reviewerType)}`);
    return pool[this.ctx.randomInt(pool.length)]!.id;
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
      if (!this.ctx.o.requestReview) {
        note += " This server does not request reviews on the forge.";
      } else {
        try {
          await this.ctx.o.requestReview(f.team_id, pr, track.reviewer_id);
          note += " A review is requested from the reviewer on the pull request.";
        } catch (e) {
          this.ctx.log("warn", "self-improvement: review request failed", {
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
      created_at: this.ctx.nowIso(),
    });
  }
}

async function inboxIdFor(workItemId: string, ref: string): Promise<string> {
  // `inb_` + the partition token of the work item + 12 characters of a hash.
  const m = /^[a-z]+_([wib][a-z2-7]{15})/.exec(workItemId);
  const token = m?.[1] ?? "w" + base32(utf8(workItemId)).slice(0, 15).padEnd(15, "a");
  const d = await sha256(`inbox|${workItemId}|${ref}`);
  return `inb_${token}${base32(d).slice(0, 12)}`;
}
