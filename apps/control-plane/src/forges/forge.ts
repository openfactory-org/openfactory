// The forge contract: core.Forge and its optional capabilities. Port of the
// forge part of internal/core/interfaces.go, definition.go and scoring.go.
// A forge that does not support an operation throws an OfError with code
// operation_not_supported.
import type { CheckRunInput } from "./checkrun";
import type { FileDiff, PullRequestRef, ReviewComment, ReviewThread } from "@openfactory/core";

export type { CheckRunInput };

/** Code-forge identifiers (definition `codeForge` values). */
export const GITHUB = "GITHUB";
export const GITLAB = "GITLAB";
export const AZURE_DEVOPS = "AZURE_DEVOPS";
export const GIT = "GIT";

export interface PullRequestInput {
  owner: string;
  repo: string;
  head: string;
  base: string;
  title: string;
  body: string;
  draft?: boolean;
}

export interface CommitStatus {
  context: string;
  /** pending, success, failure, error */
  state: string;
  description?: string;
  targetUrl?: string;
}

/** Input of Forge.submitReview. */
export interface ReviewInput {
  owner: string;
  repo: string;
  number: number;
  /** comment (default), approve, request_changes */
  event?: string;
  body: string;
  comments?: ReviewComment[];
}

export interface Forge {
  /** GITHUB, GITLAB, AZURE_DEVOPS */
  name(): string;
  /** Authenticated clone URL. */
  cloneURL(owner: string, repo: string): Promise<string>;
  defaultBranch(owner: string, repo: string): Promise<string>;
  createPullRequest(req: PullRequestInput): Promise<PullRequestRef>;
  getPullRequest(owner: string, repo: string, number: number): Promise<PullRequestRef>;
  comment(owner: string, repo: string, number: number, body: string): Promise<void>;
  setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void>;
  /** Reads a tree at ref (git-backed definitions): files relative to path, and the commit sha. */
  getFiles(owner: string, repo: string, ref: string, path: string): Promise<{ files: Record<string, string>; sha: string }>;
  replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void>;
  resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void>;
  submitReview(input: ReviewInput): Promise<void>;
  markReady(owner: string, repo: string, number: number): Promise<void>;

  // Optional capabilities.
  commentIssue?(owner: string, repo: string, number: number, body: string): Promise<void>;
  commentWorkItem?(project: string, id: number, body: string): Promise<void>;
  setWorkItemState?(project: string, id: number, state: string): Promise<void>;
  attachPullRequestToWorkItem?(project: string, id: number, pr: PullRequestRef): Promise<void>;
  requestReviewers?(owner: string, repo: string, number: number, logins: string[]): Promise<void>;
  login?(): Promise<string>;
  reviewThread?(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread>;
  fileDiff?(owner: string, repo: string, number: number, path: string): Promise<FileDiff>;
  publishCheckRun?(owner: string, repo: string, cr: CheckRunInput): Promise<void>;
  listPullRequestFiles?(owner: string, repo: string, number: number): Promise<string[]>;
  addLabels?(owner: string, repo: string, number: number, ...labels: string[]): Promise<void>;
}

/** One forge action: the team, the factory and one repository. */
export interface ForgeLookup {
  teamId: string;
  /** Empty when the action belongs to no factory. */
  factoryId?: string;
  /** GITHUB, GITLAB, AZURE_DEVOPS or GIT. */
  codeForge: string;
  /** "owner/name"; its owner selects the record. */
  repo: string;
  /** Limits the credential to these repositories where the provider can. */
  scope?: string[];
}

/** Returns an authenticated forge client for one repository of a team. */
export interface ForgeResolver {
  forge(l: ForgeLookup): Promise<Forge>;
}
