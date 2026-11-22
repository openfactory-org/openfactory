// Runs, their transcripts, usage, sources and attachments. Port of
// internal/core (types.go, attachment.go, turnend.go). Field names are the Go
// JSON names.

import { isOneOf, type ListOpts, type Timestamp } from "./common";
import type { AgentConfig, AgentOverrides, CodeForge, RepoAtCommit } from "./definition";
import { OfError } from "./errors";
import type { FailureCode } from "./failure";
import { cut } from "./gostd";
import type { Principal } from "./identity";

export type RunState =
  | "queued"
  | "pending"
  | "claimed"
  | "running"
  | "blocked"
  | "succeeded"
  | "failed"
  | "error"
  | "cancelled"
  /** Old states that only older stored runs carry. */
  | "starting"
  | "idle";

/** Every run source in the order of the API. */
export const RUN_SOURCES = [
  "API",
  "MCP",
  "DASHBOARD",
  "CLI",
  "AUTOMATION",
  "SCHEDULE",
  "WEBHOOK",
  "MENTION",
  "ORCHESTRATION",
  "SCORER",
  "BENCHMARK",
  "SELF_IMPROVEMENT",
] as const;
/** The closed list of run sources of the API. */
export type RunSource = (typeof RUN_SOURCES)[number];

/** Usage coverage values. */
export const USAGE_KNOWN = "known";
export const USAGE_PARTIAL = "partial";
export const USAGE_UNAVAILABLE = "unavailable";
export const USAGE_COVERAGES = [USAGE_KNOWN, USAGE_PARTIAL, USAGE_UNAVAILABLE] as const;
export type UsageCoverage = (typeof USAGE_COVERAGES)[number];

/** Token, cost and time accounting of a run, a session or a turn. */
export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  duration_ms: number;
  coverage?: UsageCoverage;
}

/** The queued reason of a run that waits because spend is at the limit. */
export const QUEUED_SPEND_LIMIT = "spend_limit";

export interface Run {
  id: string;
  version: number;
  team_id: string;
  factory_id: string;
  work_item_id?: string;
  parent_run_id?: string;
  agent_id?: string;
  agent_name?: string;
  name?: string;
  state: RunState;
  prompt: string;
  config: AgentConfig;
  metadata?: Record<string, string>;
  session_id?: string;
  consumed_seq: number;
  sandbox_id?: string;
  worker_id?: string;
  result?: string;
  error?: string;
  failure_code?: FailureCode;
  blocked_reason?: string;
  queued_reason?: string;
  turns?: number;
  cancelled_by?: string;
  usage: Usage;
  triggered_by?: string;
  source?: RunSource;
  source_ref?: string;
  creator?: Principal;
  run_as?: Principal;
  created_at: Timestamp;
  started_at?: Timestamp;
  finished_at?: Timestamp;
  queued_at?: Timestamp;
}

export const RUN_MESSAGE_ROLES = ["user", "assistant", "tool_call", "tool_result", "system", "log"] as const;
export type RunMessageRole = (typeof RUN_MESSAGE_ROLES)[number];

/** One entry in a run's transcript. */
export interface RunMessage {
  id: string;
  run_id: string;
  seq: number;
  role: RunMessageRole;
  content: string;
  data?: unknown;
  created_at: Timestamp;
}

/** Outcomes of a message to a run. */
export const MESSAGE_LIVE_SESSION = "live_session";
export const MESSAGE_NEW_SESSION = "new_session";
export const MESSAGE_QUEUED = "queued";
export const MESSAGE_OUTCOMES = [MESSAGE_LIVE_SESSION, MESSAGE_NEW_SESSION, MESSAGE_QUEUED] as const;
export type MessageOutcome = (typeof MESSAGE_OUTCOMES)[number];

export interface MessageReceipt {
  message_id: string;
  outcome: MessageOutcome;
}

export interface ChildRunConfig {
  name: string;
  agent_identity_uid: string;
  prompt: string;
  metadata?: Record<string, string>;
  overrides?: AgentOverrides;
  skills?: string[];
}

export interface RunAgentsRequest {
  summary: string;
  base_prompt: string;
  remote?: Record<string, unknown>;
  metadata?: Record<string, string>;
  agent_run_configs: ChildRunConfig[] | null;
}

/** The run metadata key that puts a run in trial mode. */
export const META_TRIAL_MODE = "trial_mode";

/** The start commit of one repository of a run. */
export interface RepoStart {
  codeForge: CodeForge;
  owner: string;
  repo: string;
  path?: string;
  ref: string;
  resolved?: boolean;
}

export interface RunFilter extends ListOpts {
  teamId?: string;
  factoryId?: string;
  workItemId?: string;
  parentRunId?: string;
  agentName?: string;
  name?: string;
  creatorId?: string;
  states?: RunState[];
  since?: Timestamp;
}

// ---------- Attachments ----------

/** The size limit of one uploaded attachment. */
export const MAX_ATTACHMENT_BYTES = 25 << 20;
/** The most attachments one message can carry. */
export const MAX_ATTACHMENTS_PER_MESSAGE = 20;

export interface Attachment {
  attachment_id: string;
  team_id: string;
  name: string;
  content_type: string;
  bytes: number;
  created_by?: string;
  created_at: Timestamp;
}

// ---------- Functions ----------

/** The ID of the creator of a run, or "". */
export function runCreatorId(r: Pick<Run, "creator"> | null | undefined): string {
  return r?.creator?.id ?? "";
}

/** Reports whether the run finished a turn and waits for a message (blocked, or the old idle). */
export function waitsForInput(s: RunState): boolean {
  return s === "idle" || s === "blocked";
}

/** The coverage of usage for the API: "known" when empty. */
export function usageCoverage(u: Pick<Usage, "coverage">): UsageCoverage;
export function usageCoverage(u: { coverage?: string }): string;
export function usageCoverage(u: { coverage?: string }): string {
  return u.coverage ? u.coverage : USAGE_KNOWN;
}

/** Validates a source name of a filter, ignoring case. */
export function runSourceName(v: string): RunSource | null {
  const s = v.trim().toUpperCase();
  return isOneOf(RUN_SOURCES, s) ? s : null;
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
