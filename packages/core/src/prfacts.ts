// Pull request facts (for metrics) and review context (threads, diffs and
// review comments). Port of internal/core (prfacts.go, reviewcontext.go).
// Field names are the Go JSON names.

import type { Timestamp } from "./common";
import { equalFold } from "./gostd";

// ---------- Pull request facts ----------

/** Actor kinds of a pull request push. */
export const PUSH_BY_AGENT = "agent";
export const PUSH_BY_BOT = "bot";
export const PUSH_BY_PERSON = "person";
export const PUSH_ACTOR_KINDS = [PUSH_BY_AGENT, PUSH_BY_BOT, PUSH_BY_PERSON] as const;
export type PushActorKind = (typeof PUSH_ACTOR_KINDS)[number];

export interface PullRequestPush {
  at: Timestamp;
  actor?: string;
  kind: PushActorKind;
  sha?: string;
}

export interface PullRequestFacts {
  /** team_id + "|" + key */
  id: string;
  team_id: string;
  key: string;
  provider: string;
  repo: string;
  number: number;
  url?: string;
  head_branch?: string;
  opener?: string;
  opener_kind?: PushActorKind;
  opened_at?: Timestamp;
  pushes?: PullRequestPush[];
  first_review_at?: Timestamp;
  merged_at?: Timestamp;
  closed_at?: Timestamp;
  additions?: number;
  deletions?: number;
  updated_at: Timestamp;
}

export interface PullRequestFactFilter {
  teamId?: string;
  keys?: string[];
  provider?: string;
  /** case-insensitive */
  repo?: string;
  headBranch?: string;
  openOnly?: boolean;
}

// ---------- Review context ----------

export interface ThreadComment {
  id?: string;
  author?: string;
  body?: string;
  path?: string;
  line?: number;
  created_at?: Timestamp;
}

export interface ReviewThread {
  ref?: string;
  path?: string;
  line?: number;
  resolved?: boolean;
  comments: ThreadComment[] | null;
}

export interface FileDiff {
  path: string;
  patch: string;
  truncated?: boolean;
}

/** Review events of a forge review. */
export const REVIEW_EVENT_COMMENT = "comment";
export const REVIEW_EVENT_APPROVE = "approve";
export const REVIEW_EVENT_REQUEST_CHANGES = "request_changes";
export const REVIEW_EVENTS = [REVIEW_EVENT_COMMENT, REVIEW_EVENT_APPROVE, REVIEW_EVENT_REQUEST_CHANGES] as const;
export type ReviewEvent = (typeof REVIEW_EVENTS)[number];

export interface ReviewComment {
  path: string;
  line: number;
  side?: string;
  body: string;
}

// ---------- Functions ----------

/** The ID of the facts of a pull request. */
export function pullRequestFactsId(teamId: string, key: string): string {
  return teamId + "|" + key;
}

/** Reports whether the pull request is neither merged nor closed. */
export function pullRequestOpen(f: Pick<PullRequestFacts, "merged_at" | "closed_at">): boolean {
  return !f.merged_at && !f.closed_at;
}

/** Additions plus deletions, or null when the size is not known. */
export function pullRequestChangedLines(f: Pick<PullRequestFacts, "additions" | "deletions">): number | null {
  if (f.additions === undefined && f.deletions === undefined) return null;
  return (f.additions ?? 0) + (f.deletions ?? 0);
}

/** Reports whether a person opened the pull request, or pushed to it, at or before t. */
export function personPushedBefore(f: Pick<PullRequestFacts, "opener_kind" | "pushes">, t: Date | string): boolean {
  if (f.opener_kind === PUSH_BY_PERSON) return true;
  const limit = new Date(t).getTime();
  return (f.pushes ?? []).some((p) => p.kind === PUSH_BY_PERSON && new Date(p.at).getTime() <= limit);
}

/** Reports whether facts match a filter. */
export function matchPullRequestFacts(q: PullRequestFactFilter, f: PullRequestFacts): boolean {
  if (q.teamId && f.team_id !== q.teamId) return false;
  if (q.keys && q.keys.length > 0 && !q.keys.includes(f.key)) return false;
  if (q.provider && !equalFold(q.provider, f.provider)) return false;
  if (q.repo && !equalFold(q.repo, f.repo)) return false;
  if (q.headBranch && q.headBranch !== f.head_branch) return false;
  return !q.openOnly || pullRequestOpen(f);
}
