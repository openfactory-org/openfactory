// Inbound events, their sources, work items, their references and the
// messages a factory sends back to a work source. Port of internal/core
// (types.go, source.go, references.go). Field names are the Go JSON names.

import type { ListOpts, Timestamp } from "./common";

// ---------- Events ----------

/** Sender trust values (Actor.trust). */
export const TRUST_MEMBER = "member";
export const TRUST_EXTERNAL = "external";
export const TRUST_PLATFORM = "platform";
export const TRUST_LEVELS = [TRUST_MEMBER, TRUST_EXTERNAL, TRUST_PLATFORM] as const;
export type Trust = (typeof TRUST_LEVELS)[number];

export interface Actor {
  provider: string;
  external_id: string;
  handle: string;
  is_bot: boolean;
  user_id?: string;
  is_self?: boolean;
  trust?: Trust;
}

/** Where a work item came from, so that results return there. */
export interface SourceRef {
  provider: string;
  kind: string;
  url?: string;
  repo?: string;
  number?: number;
  channel?: string;
  thread_id?: string;
  external_id?: string;
  title?: string;
  /** A sub-thread of the source. Not part of the key. */
  thread_ref?: string;
  /** The workspace, team or site of a chat or tracker source. Not part of the key. */
  workspace?: string;
}

/** A normalized inbound event from any work source. */
export interface Event {
  id: string;
  team_id: string;
  factory_id?: string;
  provider: string;
  type: string;
  delivery_id?: string;
  action_key?: string;
  actor: Actor;
  source: SourceRef;
  attributes: Record<string, unknown> | null;
  payload?: unknown;
  text?: string;
  received_at: Timestamp;
}

// ---------- Work items ----------

export const STAGES = [
  "intake",
  "triage",
  "planning",
  "awaiting_approval",
  "building",
  "reviewing",
  "handoff",
  "complete",
  "cancelled",
] as const;
export type Stage = (typeof STAGES)[number];

/** Pull request states. Empty means not known. */
export const PULL_REQUEST_STATES = ["open", "draft", "merged", "closed", ""] as const;
export type PullRequestState = (typeof PULL_REQUEST_STATES)[number];

export interface PullRequestRef {
  forge: string;
  repo: string;
  number: number;
  url: string;
  branch: string;
  state: PullRequestState;
}

/** Kinds of a work item reference. */
export const REF_SOURCE = "source";
export const REF_PULL_REQUEST = "pull_request";
export const REF_ISSUE = "issue";
export const REF_THREAD = "thread";
export const WORK_ITEM_REF_KINDS = [REF_SOURCE, REF_PULL_REQUEST, REF_ISSUE, REF_THREAD] as const;
export type WorkItemRefKind = (typeof WORK_ITEM_REF_KINDS)[number];

/** One external reference of a work item. Its key routes events to the work item. */
export interface WorkItemRef {
  kind: WorkItemRefKind;
  key: string;
  source: SourceRef;
  added_at?: Timestamp;
}

export interface WorkItem {
  id: string;
  factory_id: string;
  title: string;
  stage: Stage;
  source: SourceRef;
  source_key?: string;
  automation_id?: string;
  creator_id?: string;
  root_run_id?: string;
  pull_requests?: PullRequestRef[];
  references?: WorkItemRef[];
  labels?: Record<string, string>;
  local_owner?: string;
  created_at: Timestamp;
  updated_at: Timestamp;
  completed_at?: Timestamp;
  version: number;
}

/** The timeline of a work item. */
export interface WorkItemEvent {
  id: string;
  work_item_id: string;
  kind: string;
  run_id?: string;
  actor?: string;
  data?: unknown;
  created_at: Timestamp;
}

export interface WorkItemFilter extends ListOpts {
  factoryId?: string;
  stages?: Stage[];
  creatorId?: string;
  search?: string;
  since?: Timestamp;
}

// ---------- Outbound messages ----------

/** Issue states that the engine writes back to a tracker. */
export const ISSUE_STATE_IN_PROGRESS = "in_progress";
export const ISSUE_STATE_IN_REVIEW = "in_review";
export const ISSUE_STATE_DONE = "done";
export const ISSUE_STATES = [ISSUE_STATE_IN_PROGRESS, ISSUE_STATE_IN_REVIEW, ISSUE_STATE_DONE] as const;
export type IssueState = (typeof ISSUE_STATES)[number];

/** Outbound message kinds that are tracker operations, not text. */
export const OUTBOUND_ISSUE_STATE = "issue_state";
export const OUTBOUND_ATTACH_PULL_REQUEST = "attach_pull_request";
export const OUTBOUND_KINDS = [
  "update",
  "question",
  "approval_request",
  "handoff",
  "error",
  OUTBOUND_ISSUE_STATE,
  OUTBOUND_ATTACH_PULL_REQUEST,
] as const;
export type OutboundKind = (typeof OUTBOUND_KINDS)[number];

/** Something the factory says back to a work source. */
export interface OutboundMessage {
  work_item_id: string;
  source: SourceRef;
  /** markdown */
  text: string;
  kind: OutboundKind;
  options?: string[];
  inbox_id?: string;
  factory_id?: string;
  thread_ref?: string;
  issue_state?: IssueState;
  pull_request?: PullRequestRef;
}

// ---------- Functions ----------

/** A stable identity used to attach events to an existing work item. */
export function sourceKey(s: SourceRef): string {
  if (s.repo && s.number) return `${s.provider}:${s.repo}#${s.number}`;
  if (s.thread_id) return `${s.provider}:${s.channel ?? ""}:${s.thread_id}`;
  if (s.external_id) return `${s.provider}:${s.external_id}`;
  return "";
}

/**
 * Reports whether a source is a tracker issue that the engine writes back to:
 * a Linear or Jira issue, or an Azure DevOps work item.
 */
export function isTrackerIssue(s: SourceRef): boolean {
  switch (s.provider.toLowerCase()) {
    case "linear":
    case "jira":
      return !!s.external_id || !!s.channel;
    case "azure_devops":
      return s.kind === "work_item";
  }
  return false;
}

/** Reports whether an outbound kind is a tracker operation. */
export function isTrackerOperation(kind: string): boolean {
  return kind === OUTBOUND_ISSUE_STATE || kind === OUTBOUND_ATTACH_PULL_REQUEST;
}

/** The source reference of a pull request. */
export function pullRequestSource(pr: Pick<PullRequestRef, "forge" | "url" | "repo" | "number">): SourceRef {
  let provider = pr.forge.trim().replaceAll("-", "_").toLowerCase();
  if (provider === "") provider = "github";
  const out: SourceRef = { provider, kind: "pull_request" };
  if (pr.url) out.url = pr.url;
  if (pr.repo) out.repo = pr.repo;
  if (pr.number) out.number = pr.number;
  return out;
}

/**
 * The references of a work item: the stored ones, and the source and the pull
 * requests when they are not stored. The source comes first.
 */
export function workItemRefs(
  w: Pick<WorkItem, "source" | "references" | "pull_requests" | "created_at">,
): WorkItemRef[] {
  const out: WorkItemRef[] = [];
  const seen = new Set<string>();
  const add = (r: WorkItemRef): void => {
    const key = r.key || sourceKey(r.source);
    const id = r.kind + "|" + key;
    if (key === "" || seen.has(id)) return;
    seen.add(id);
    out.push({ ...r, key });
  };
  if (sourceKey(w.source) !== "") add({ kind: REF_SOURCE, key: "", source: w.source, added_at: w.created_at });
  for (const r of w.references ?? []) add(r);
  for (const pr of w.pull_requests ?? []) {
    if (pr.repo && pr.number) add({ kind: REF_PULL_REQUEST, key: "", source: pullRequestSource(pr) });
  }
  return out;
}

/** Reports whether a reference of the work item has the key. */
export function workItemHasRef(
  w: Pick<WorkItem, "source" | "references" | "pull_requests" | "created_at">,
  key: string,
): boolean {
  if (key === "") return false;
  return workItemRefs(w).some((r) => r.key === key);
}

/** Adds a reference to w.references. Reports false when one of the same kind and key exists. */
export function workItemAddRef(w: Pick<WorkItem, "references">, r: WorkItemRef): boolean {
  const key = r.key || sourceKey(r.source);
  if (key === "") return false;
  const refs = w.references ?? [];
  if (refs.some((x) => x.kind === r.kind && x.key === key)) return false;
  w.references = [...refs, { ...r, key }];
  return true;
}

/** Reports whether a stage is terminal. */
export function isTerminalStage(s: Stage): boolean {
  return s === "complete" || s === "cancelled";
}
