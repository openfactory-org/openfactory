// A recording fake Forge and ForgeResolver for tests (port of
// internal/forge/forgeutil/forgetest). Every call is appended to `calls`.
import { OfError, type FileDiff, type PullRequestRef, type ReviewThread } from "@openfactory/core";
import type { CheckRunInput } from "../../src/forges/checkrun";
import type { CommitStatus, Forge, ForgeLookup, ForgeResolver, PullRequestInput, ReviewInput } from "../../src/forges/forge";

export interface Call {
  op: string;
  args: unknown[];
}

export class FakeForge implements Forge {
  readonly calls: Call[] = [];
  files: Record<string, string> = {};
  sha = "0123456789abcdef0123456789abcdef01234567";
  prFiles: string[] = [];
  threads: Record<string, ReviewThread> = {};
  diffs: Record<string, FileDiff> = {};
  /** Ops that fail with operation_not_supported. */
  unsupported = new Set<string>();
  /** Ops that fail with this error. */
  failures: Record<string, Error> = {};
  userLogin = "octocat";
  nextNumber = 1;

  constructor(private readonly forgeName = "GITHUB") {}

  private rec(op: string, ...args: unknown[]): void {
    this.calls.push({ op, args });
    if (this.unsupported.has(op)) throw new OfError("operation_not_supported", `fake: ${op} not supported`);
    const f = this.failures[op];
    if (f) throw f;
  }

  callsOf(op: string): Call[] {
    return this.calls.filter((c) => c.op === op);
  }

  name(): string {
    return this.forgeName;
  }
  async cloneURL(owner: string, repo: string): Promise<string> {
    this.rec("cloneURL", owner, repo);
    return `https://x-access-token:fake@example.test/${owner}/${repo}.git`;
  }
  async defaultBranch(owner: string, repo: string): Promise<string> {
    this.rec("defaultBranch", owner, repo);
    return "main";
  }
  async createPullRequest(req: PullRequestInput): Promise<PullRequestRef> {
    this.rec("createPullRequest", req);
    const n = this.nextNumber++;
    return { forge: this.forgeName, repo: `${req.owner}/${req.repo}`, number: n, url: `https://example.test/${req.owner}/${req.repo}/pull/${n}`, branch: req.head, state: req.draft ? "draft" : "open" };
  }
  async getPullRequest(owner: string, repo: string, number: number): Promise<PullRequestRef> {
    this.rec("getPullRequest", owner, repo, number);
    return { forge: this.forgeName, repo: `${owner}/${repo}`, number, url: "", branch: "", state: "open" };
  }
  async comment(owner: string, repo: string, number: number, body: string): Promise<void> {
    this.rec("comment", owner, repo, number, body);
  }
  async commentIssue(owner: string, repo: string, number: number, body: string): Promise<void> {
    this.rec("commentIssue", owner, repo, number, body);
  }
  async setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    this.rec("setCommitStatus", owner, repo, sha, st);
  }
  async getFiles(owner: string, repo: string, ref: string, path: string): Promise<{ files: Record<string, string>; sha: string }> {
    this.rec("getFiles", owner, repo, ref, path);
    return { files: { ...this.files }, sha: this.sha };
  }
  async replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void> {
    this.rec("replyInThread", owner, repo, number, threadRef, body);
  }
  async resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    this.rec("resolveThread", owner, repo, number, threadRef);
  }
  async submitReview(input: ReviewInput): Promise<void> {
    this.rec("submitReview", input);
  }
  async markReady(owner: string, repo: string, number: number): Promise<void> {
    this.rec("markReady", owner, repo, number);
  }
  async listPullRequestFiles(owner: string, repo: string, number: number): Promise<string[]> {
    this.rec("listPullRequestFiles", owner, repo, number);
    return [...this.prFiles];
  }
  async addLabels(owner: string, repo: string, number: number, ...labels: string[]): Promise<void> {
    this.rec("addLabels", owner, repo, number, labels);
  }
  async publishCheckRun(owner: string, repo: string, cr: CheckRunInput): Promise<void> {
    this.rec("publishCheckRun", owner, repo, cr);
  }
  async requestReviewers(owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    this.rec("requestReviewers", owner, repo, number, logins);
  }
  async login(): Promise<string> {
    this.rec("login");
    return this.userLogin;
  }
  async reviewThread(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread> {
    this.rec("reviewThread", owner, repo, number, threadRef);
    const t = this.threads[threadRef];
    if (!t) throw new OfError("resource_not_found", `fake: thread ${threadRef}`);
    return t;
  }
  async fileDiff(owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    this.rec("fileDiff", owner, repo, number, path);
    const d = this.diffs[path];
    if (!d) throw new OfError("resource_not_found", `fake: diff ${path}`);
    return d;
  }
}

/** A resolver that returns one fake forge per codeForge and records lookups. */
export class FakeResolver implements ForgeResolver {
  readonly lookups: ForgeLookup[] = [];
  readonly forges = new Map<string, FakeForge>();
  error: Error | undefined;

  forgeFor(codeForge: string): FakeForge {
    let f = this.forges.get(codeForge);
    if (!f) {
      f = new FakeForge(codeForge);
      this.forges.set(codeForge, f);
    }
    return f;
  }

  async forge(l: ForgeLookup): Promise<Forge> {
    this.lookups.push(l);
    if (this.error) throw this.error;
    return this.forgeFor(l.codeForge);
  }
}
