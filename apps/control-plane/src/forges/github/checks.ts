// GitHub check runs and commit statuses. An App holds checks:write and
// publishes check runs; a personal token uses the commit statuses API.
import { pathEscape, queryEscape, statusCode, truncate } from "@openfactory/core";
import type { CheckRunInput } from "../checkrun";
import type { CommitStatus } from "../forge";
import { repoPath, wrap, type GitHubTransport } from "./rest";

const MAX_OPEN_CHECK_RUNS = 256;

/** A check run to create or update. */
export interface GitHubCheckRun extends CheckRunInput {
  externalId?: string;
}

export function checkRunBody(cr: GitHubCheckRun, create: boolean, nowMs: number): Record<string, unknown> {
  const b: Record<string, unknown> = {};
  if (create) {
    b["name"] = cr.name;
    b["head_sha"] = cr.headSha;
  }
  if (cr.status) b["status"] = cr.status;
  if (cr.conclusion) {
    b["conclusion"] = cr.conclusion;
    b["completed_at"] = new Date(nowMs).toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  if (cr.detailsUrl) b["details_url"] = cr.detailsUrl;
  if (cr.externalId) b["external_id"] = cr.externalId;
  if (cr.title || cr.summary) {
    const out: Record<string, string> = {
      title: truncate(cr.title || cr.name, 255),
      summary: truncate(cr.summary ?? "", 65535),
    };
    if (cr.text) out["text"] = truncate(cr.text, 65535);
    b["output"] = out;
  }
  return b;
}

/** Check runs of one client. It remembers the runs in progress it created. */
export class GitHubChecks {
  /** "owner/repo@sha#name" (lower case) -> ID of the run in progress. */
  private readonly openRuns = new Map<string, number>();

  constructor(private readonly t: GitHubTransport) {}

  /**
   * With an App it publishes a check run (the App holds checks:write); with a
   * token it uses the commit statuses API.
   */
  async setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    if (this.t.isApp()) {
      const cr: GitHubCheckRun = {
        name: st.context,
        headSha: sha,
        detailsUrl: st.targetUrl ?? "",
        title: st.description ?? "",
        summary: st.description ?? "",
      };
      switch (st.state) {
        case "pending":
        case "":
          cr.status = "in_progress";
          break;
        case "success":
          cr.status = "completed";
          cr.conclusion = "success";
          break;
        case "failure":
          cr.status = "completed";
          cr.conclusion = "failure";
          break;
        default:
          cr.status = "completed";
          cr.conclusion = "action_required";
      }
      await this.createCheckRun(owner, repo, cr);
      return;
    }
    const body: Record<string, string> = {
      state: st.state || "pending",
      context: st.context,
      description: truncate(st.description ?? "", 140),
    };
    if (st.targetUrl) body["target_url"] = st.targetUrl;
    try {
      await this.t.call(owner, repo, "POST", repoPath(owner, repo) + "/statuses/" + pathEscape(sha), body);
    } catch (e) {
      throw wrap("github: set commit status", e);
    }
  }

  async createCheckRun(owner: string, repo: string, cr: GitHubCheckRun): Promise<number> {
    try {
      const r = await this.t.call<{ id: number }>(
        owner,
        repo,
        "POST",
        repoPath(owner, repo) + "/check-runs",
        checkRunBody(cr, true, this.t.now()),
      );
      return r?.id ?? 0;
    } catch (e) {
      throw wrap("github: create check run", e);
    }
  }

  async updateCheckRun(owner: string, repo: string, id: number, cr: GitHubCheckRun): Promise<void> {
    try {
      await this.t.call(
        owner,
        repo,
        "PATCH",
        `${repoPath(owner, repo)}/check-runs/${id}`,
        checkRunBody(cr, false, this.t.now()),
      );
    } catch (e) {
      throw wrap("github: update check run", e);
    }
  }

  /**
   * Publishes a check run. A completed run updates the run in progress for the
   * same commit and name. A token client sets a commit status instead.
   */
  async publishCheckRun(owner: string, repo: string, cr: CheckRunInput): Promise<void> {
    if (!this.t.isApp()) {
      const st: CommitStatus = {
        context: cr.name,
        targetUrl: cr.detailsUrl ?? "",
        description: cr.title ?? "",
        state: "",
      };
      if (cr.status !== "completed") st.state = "pending";
      else if (cr.conclusion === "success") st.state = "success";
      else st.state = "failure";
      return this.setCommitStatus(owner, repo, cr.headSha, st);
    }
    const gr: GitHubCheckRun = { ...cr, status: cr.status || "in_progress" };
    const key = `${owner}/${repo}@${cr.headSha}#${cr.name}`.toLowerCase();
    if (gr.status !== "completed") {
      const id = await this.createCheckRun(owner, repo, gr);
      if (this.openRuns.size >= MAX_OPEN_CHECK_RUNS) this.openRuns.clear();
      this.openRuns.set(key, id);
      return;
    }
    let id = this.openRuns.get(key) ?? 0;
    this.openRuns.delete(key);
    if (id === 0) id = await this.findCheckRun(owner, repo, cr.headSha, cr.name);
    if (id !== 0) {
      try {
        await this.updateCheckRun(owner, repo, id, gr);
        return;
      } catch (e) {
        if (statusCode(e) !== 404) throw e;
      }
    }
    await this.createCheckRun(owner, repo, gr);
  }

  private async findCheckRun(owner: string, repo: string, sha: string, name: string): Promise<number> {
    try {
      const r = await this.t.call<{ check_runs?: { id: number; app?: { id?: number } }[] }>(
        owner,
        repo,
        "GET",
        `${repoPath(owner, repo)}/commits/${pathEscape(sha)}/check-runs?filter=latest&check_name=${queryEscape(name)}`,
      );
      const appId = this.t.appId();
      for (const x of r?.check_runs ?? []) if (appId === 0 || x.app?.id === appId) return x.id;
    } catch {
      /* create a new run */
    }
    return 0;
  }
}
