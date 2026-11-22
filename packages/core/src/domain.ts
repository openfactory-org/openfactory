// Domain functions. Port of the methods and helpers of internal/core: roles,
// principals, source keys, work item references, run sources, inbox
// recipients, integration status and workspaces, definition version state,
// attachment names, outbox partition keys, pull request facts.

import { OfError } from "./errors";
import { cut, equalFold } from "./gostd";
import type {
  DefinitionVersion,
  Event,
  Factory,
  InboxItem,
  Integration,
  OutboundMessage,
  Principal,
  PrincipalType,
  PullRequestFactFilter,
  PullRequestFacts,
  PullRequestRef,
  RepoAtCommit,
  RepoRef,
  RepoStart,
  Role,
  Run,
  RunSource,
  RunState,
  SourceRef,
  Stage,
  Usage,
  User,
  WorkItem,
  WorkItemRef,
} from "./types";
import {
  BUILTIN_MCP_PREFIX,
  BUILTIN_MCP_PROVIDERS,
  DEFINITION_ACTIVE,
  DEFINITION_INVALID,
  DEFINITION_SUPERSEDED,
  DURABLE_TOPICS,
  INTEGRATION_ACTIVE,
  MCP_CONNECTION_PROXY,
  META_TRIAL_MODE,
  OUTBOUND_ATTACH_PULL_REQUEST,
  OUTBOUND_ISSUE_STATE,
  PUSH_BY_PERSON,
  REF_PULL_REQUEST,
  REF_SOURCE,
  RUN_AS_FACTORY,
  RUN_AS_SERVICE_ACCOUNT_PREFIX,
  RUN_AS_USER_PREFIX,
  RUN_SOURCES,
  USAGE_KNOWN,
  USER_KIND_SERVICE_ACCOUNT,
} from "./types";

// ---------- Roles ----------

/** Trims and lowercases the name of a role. */
export function normalizeRole(r: string): Role {
  return r.trim().toLowerCase();
}

/** Orders the built-in roles: user 1, editor 2, admin 3, and 0 for unknown names. */
export function roleRank(r: string): number {
  switch (normalizeRole(r)) {
    case "admin":
      return 3;
    case "editor":
      return 2;
    case "user":
      return 1;
  }
  return 0;
}

/** The role of the user for one factory: its override, else the team-wide role. */
export function roleFor(u: Pick<User, "role" | "factory_roles"> | null | undefined, factoryId = ""): Role {
  if (!u) return "";
  if (factoryId !== "") {
    const r = u.factory_roles?.[factoryId];
    if (r !== undefined && roleRank(r) > 0) return normalizeRole(r);
  }
  return normalizeRole(u.role);
}

// ---------- Principals ----------

/** "<type>:<id>", or "" for a missing principal. */
export function principalString(p: Principal | null | undefined): string {
  if (!p || p.id === "") return "";
  return p.type + ":" + p.id;
}

/** Reports whether p is a person. */
export function isPerson(p: Principal | null | undefined): boolean {
  return !!p && p.type === "user" && p.id !== "";
}

/** Reports whether u is a service account. */
export function isServiceAccount(u: Pick<User, "kind"> | null | undefined): boolean {
  return !!u && u.kind === USER_KIND_SERVICE_ACCOUNT;
}

/** The principal of a user record: a user or a service account. */
export function userPrincipal(u: Pick<User, "id" | "name" | "email" | "kind"> | null | undefined): Principal | null {
  if (!u || u.id === "") return null;
  return {
    type: isServiceAccount(u) ? "service_account" : "user",
    id: u.id,
    name: u.name !== "" ? u.name : u.email,
  };
}

/** The principal of a factory. */
export function factoryPrincipal(f: Pick<Factory, "id" | "name" | "alias"> | null | undefined): Principal | null {
  if (!f) return null;
  return { type: "factory", id: f.id, name: f.name !== "" ? f.name : f.alias };
}

/** The ID of the creator of a run, or "". */
export function runCreatorId(r: Pick<Run, "creator"> | null | undefined): string {
  return r?.creator?.id ?? "";
}

/**
 * Splits a runAs value into its principal type and the email (user) or name
 * (service account). An empty value is the factory. Throws invalid_request.
 */
export function parseRunAs(v: string): { type: PrincipalType; value: string } {
  const s = v.trim();
  if (s === "" || s === RUN_AS_FACTORY) return { type: "factory", value: "" };
  if (s.startsWith(RUN_AS_USER_PREFIX)) {
    const email = s.slice(RUN_AS_USER_PREFIX.length).trim();
    if (email === "" || !email.includes("@")) throw new OfError("invalid_request", `runAs "${s}": want user:<email>`);
    return { type: "user", value: email };
  }
  if (s.startsWith(RUN_AS_SERVICE_ACCOUNT_PREFIX)) {
    const name = s.slice(RUN_AS_SERVICE_ACCOUNT_PREFIX.length).trim();
    if (name === "") throw new OfError("invalid_request", `runAs "${s}": want serviceAccount:<name>`);
    return { type: "service_account", value: name };
  }
  throw new OfError("invalid_request", `runAs "${s}": want factory, user:<email> or serviceAccount:<name>`);
}

// ---------- MCP and integrations ----------

/** The integration record provider of a built-in server ("teams" -> "microsoft_teams"). */
export function builtinIntegrationProvider(provider: string): string {
  return provider === "teams" ? "microsoft_teams" : provider;
}

function normProvider(v: string): string {
  const s = v.trim().replaceAll("-", "_").toLowerCase();
  return s === "teams" ? "microsoft_teams" : s;
}

/** Reports whether two provider names name the same integration. */
export function sameIntegrationProvider(a: string, b: string): boolean {
  return normProvider(a) === normProvider(b);
}

/**
 * The provider of a built-in managedId ("builtin/linear" -> "linear"). known is
 * false for an id that is not built-in or names an unknown provider.
 */
export function builtinMCPProvider(managedId: string): { provider: string; known: boolean } {
  if (!managedId.startsWith(BUILTIN_MCP_PREFIX)) return { provider: "", known: false };
  const p = managedId.slice(BUILTIN_MCP_PREFIX.length);
  return { provider: p, known: BUILTIN_MCP_PROVIDERS.includes(p) };
}

/** Reports whether runs reach a registered server through the control-plane proxy. */
export function mcpServerProxied(m: { url?: string; connection?: string }): boolean {
  return !!m.url && m.connection === MCP_CONNECTION_PROXY;
}

/** The status of an integration record; empty means active. */
export function integrationStatus(i: Pick<Integration, "status">): string {
  return i.status ? i.status : INTEGRATION_ACTIVE;
}

/** Reports whether an integration record can be used. */
export function integrationActive(i: Pick<Integration, "status">): boolean {
  return integrationStatus(i) === INTEGRATION_ACTIVE;
}

/** The config key of a chat or tracker record that names its workspace, or "". */
export function integrationWorkspaceKey(provider: string): string {
  if (sameIntegrationProvider(provider, "slack") || sameIntegrationProvider(provider, "microsoft_teams"))
    return "team_id";
  if (sameIntegrationProvider(provider, "linear")) return "organization_id";
  if (sameIntegrationProvider(provider, "jira")) return "site";
  return "";
}

/**
 * The form of a workspace value that the selection rule compares. For Jira it
 * is the host name, or the cloud ID of an OAuth API address.
 */
export function normalizeWorkspace(provider: string, v: string): string {
  let s = v.trim();
  if (s === "" || !sameIntegrationProvider(provider, "jira")) return s;
  if (!s.includes("://")) s = "https://" + s;
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return "";
  }
  const host = u.host.toLowerCase();
  if (host === "api.atlassian.com" && u.pathname.startsWith("/ex/jira/")) {
    return cut(u.pathname.slice("/ex/jira/".length), "/")[0].toLowerCase();
  }
  return host;
}

/** The normalized workspace of a chat or tracker record, or "". */
export function integrationWorkspace(i: Pick<Integration, "provider" | "config">): string {
  const k = integrationWorkspaceKey(i.provider);
  if (k === "") return "";
  return normalizeWorkspace(i.provider, i.config?.[k] ?? "");
}

// ---------- Sources and references ----------

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
export function pullRequestSource(pr: PullRequestRef): SourceRef {
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

// ---------- Runs ----------

/** Reports whether a state is a finished state (RunState.Terminal). */
export function isTerminalRunState(s: RunState): boolean {
  return s === "succeeded" || s === "failed" || s === "cancelled" || s === "error";
}

/** Reports whether the run finished a turn and waits for a message (blocked, or the old idle). */
export function waitsForInput(s: RunState): boolean {
  return s === "idle" || s === "blocked";
}

/** The coverage of usage for the API: "known" when empty. */
export function usageCoverage(u: Pick<Usage, "coverage">): string {
  return u.coverage ? u.coverage : USAGE_KNOWN;
}

/** Validates a source name of a filter, ignoring case. */
export function runSourceName(v: string): RunSource | null {
  const s = v.trim().toUpperCase();
  return (RUN_SOURCES as readonly string[]).includes(s) ? (s as RunSource) : null;
}

/** Maps a stored triggered_by value to the API source and its reference. */
export function runSourceOf(triggeredBy: string): { source: RunSource; ref: string } {
  const [kind, ref] = cut(triggeredBy.trim(), ":");
  switch (kind.toLowerCase()) {
    case "mcp":
      return { source: "MCP", ref: "" };
    case "dashboard":
      return { source: "DASHBOARD", ref: "" };
    case "cli":
      return { source: "CLI", ref: "" };
    case "mention":
    case "follow_up":
      return { source: "MENTION", ref: "" };
    case "self_improvement":
      return { source: "SELF_IMPROVEMENT", ref };
    case "automation":
      return { source: "AUTOMATION", ref };
    case "schedule":
      return { source: "SCHEDULE", ref };
    case "webhook":
      return { source: "WEBHOOK", ref };
    case "run":
      return { source: "ORCHESTRATION", ref };
    case "scorer":
      return { source: "SCORER", ref };
    case "benchmark":
      return { source: "BENCHMARK", ref };
  }
  return { source: "API", ref: "" };
}

/** The source of a run and its reference: the stored fields, else derived from triggered_by. */
export function runSourceAndRef(r: Pick<Run, "source" | "source_ref" | "triggered_by">): {
  source: RunSource;
  ref: string;
} {
  if (r.source) return { source: r.source, ref: r.source_ref ?? "" };
  return runSourceOf(r.triggered_by ?? "");
}

/** The triggered_by value of a run that an automation starts for an event provider. */
export function triggeredByFor(provider: string, automationId: string): string {
  switch (provider) {
    case "schedule":
      return "schedule:" + automationId;
    case "webhook":
      return "webhook:" + automationId;
  }
  return "automation:" + automationId;
}

/** Reports whether a triggered_by value names the automation. */
export function startedByAutomation(triggeredBy: string, automationId: string): boolean {
  if (automationId === "") return false;
  return ["automation:", "schedule:", "webhook:"].some((p) => triggeredBy === p + automationId);
}

/** Reports whether a run is in trial mode. */
export function isTrialRun(r: Pick<Run, "metadata"> | null | undefined): boolean {
  if (!r) return false;
  return r.metadata?.[META_TRIAL_MODE] === "true" || !!r.metadata?.["benchmark_run"];
}

/** A RepoStart as a RepoAtCommit. */
export function repoStartAtCommit(r: RepoStart): RepoAtCommit {
  return { codeForge: r.codeForge, owner: r.owner, repo: r.repo, ref: r.ref };
}

/** "owner/name" of a repository. */
export function repoFullName(r: RepoRef): string {
  return r.owner + "/" + r.name;
}

/** The key/value key of the start commits of a run. */
export function startCommitsKey(runId: string): string {
  return "run:" + runId + ":start_commits";
}

/** Identifies one turn end of a run. */
export function turnMarker(r: Pick<Run, "consumed_seq" | "turns" | "state">): string {
  let m = `${r.consumed_seq}:${r.turns ?? 0}`;
  if (r.state === "cancelled") m += ":cancelled";
  return m;
}

// ---------- Inbox ----------

/** The recipients of an inbox item: recipients, or the old assignee. */
export function inboxRecipientIds(i: Pick<InboxItem, "recipients" | "assignee_id">): string[] {
  if (i.recipients && i.recipients.length > 0) return i.recipients;
  if (i.assignee_id) return [i.assignee_id];
  return [];
}

/** Reports whether the user marked the item as read. */
export function inboxIsReadBy(i: Pick<InboxItem, "read_by">, userId: string): boolean {
  return (i.read_by ?? []).includes(userId);
}

// ---------- Definition versions ----------

/** The state of a definition version. activeId is the active version of the factory. */
export function definitionVersionState(v: Pick<DefinitionVersion, "state" | "valid" | "id">, activeId: string): string {
  if (v.state) return v.state;
  if (!v.valid) return DEFINITION_INVALID;
  if (v.id === activeId) return DEFINITION_ACTIVE;
  return DEFINITION_SUPERSEDED;
}

// ---------- Attachments ----------

/** The key/value key of the metadata of an attachment. */
export function attachmentKVKey(id: string): string {
  return "attachment:" + id;
}

/**
 * The base name of an uploaded file name (backslashes count as slashes). The
 * name must not be empty, "." or "..", and must have at most 255 bytes.
 */
export function normalizeAttachmentName(raw: string): string {
  const name0 = raw.trim();
  if (name0 === "") throw new OfError("invalid_request", "attachment name is required");
  const name = goPathBase(name0.replaceAll("\\", "/"));
  if (name === "" || name === "." || name === ".." || name === "/") {
    throw new OfError("invalid_request", `invalid attachment name "${raw}"`);
  }
  if (new TextEncoder().encode(name).length > 255) {
    throw new OfError("invalid_request", "attachment name is longer than 255 bytes");
  }
  return name;
}

/** Go path.Base. */
function goPathBase(p: string): string {
  if (p === "") return ".";
  let s = p.replace(/\/+$/, "");
  if (s === "") return "/";
  const i = s.lastIndexOf("/");
  if (i >= 0) s = s.slice(i + 1);
  return s === "" ? "/" : s;
}

// ---------- Outbox ----------

/** Reports whether a topic is delivered through the outbox. */
export function isDurableTopic(topic: string): boolean {
  return DURABLE_TOPICS.includes(topic);
}

/** The partition key of an inbound event: team and source key. */
export function eventPartitionKey(e: Pick<Event, "team_id" | "source"> | null | undefined): string {
  if (!e) return "";
  const k = sourceKey(e.source);
  return k === "" ? "" : `team:${e.team_id}|${k}`;
}

/** The partition key of an outbound notice: its work item, else its source. */
export function noticePartitionKey(m: Pick<OutboundMessage, "work_item_id" | "source"> | null | undefined): string {
  if (!m) return "";
  if (m.work_item_id) return "wi:" + m.work_item_id;
  const k = sourceKey(m.source);
  return k === "" ? "" : "source:" + k;
}

/**
 * The outbox partition key of a payload, prefixed with the topic. kind names
 * the payload: an inbound event, an outbound notice, or an explicit key.
 */
export function outboxPartitionKey(
  topic: string,
  payload:
    | { kind: "event"; event: Pick<Event, "team_id" | "source"> }
    | { kind: "notice"; notice: Pick<OutboundMessage, "work_item_id" | "source"> }
    | { kind: "key"; key: string }
    | { kind: "none" },
): string {
  let key = "";
  switch (payload.kind) {
    case "event":
      key = eventPartitionKey(payload.event);
      break;
    case "notice":
      key = noticePartitionKey(payload.notice);
      break;
    case "key":
      key = payload.key;
      break;
  }
  return key === "" ? "" : topic + "|" + key;
}

// ---------- Pull request facts ----------

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
