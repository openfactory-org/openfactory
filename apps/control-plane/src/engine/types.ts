// Domain types of the engine. JSON shapes follow the Go json tags
// (internal/core/types.go, internal/core/runnerstore.go) so that REST bodies
// and stored `data` columns port 1:1.
import type { FailureCode } from "@openfactory/core";

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
  // Old names that stored runs may still carry (core.RunIdle, core.RunStarting).
  | "idle"
  | "starting";

/** core.RunState.Terminal */
export function isTerminal(s: string): boolean {
  return s === "succeeded" || s === "failed" || s === "cancelled" || s === "error";
}

/** core.RunState.WaitsForInput */
export function waitsForInput(s: string): boolean {
  return s === "idle" || s === "blocked";
}

export type Stage =
  | "intake"
  | "triage"
  | "planning"
  | "awaiting_approval"
  | "building"
  | "reviewing"
  | "handoff"
  | "complete"
  | "cancelled";

export const STAGES: readonly Stage[] = [
  "intake",
  "triage",
  "planning",
  "awaiting_approval",
  "building",
  "reviewing",
  "handoff",
  "complete",
  "cancelled",
];

export function stageTerminal(s: string): boolean {
  return s === "complete" || s === "cancelled";
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  duration_ms: number;
  coverage?: string;
}

export const ZERO_USAGE: Usage = { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 };

export interface Principal {
  kind: string;
  id?: string;
  name?: string;
  email?: string;
}

export interface EffectiveNetworkPolicy {
  restricted: boolean;
  allowed_hosts: string[] | null;
  denied_hosts: string[] | null;
}

export interface MachineSpec {
  name: string;
  description?: string;
  setupCommands?: string[];
  vcpus?: number;
  memoryGb?: number;
  os: string;
  arch: string;
  image?: string;
  user?: string;
  failureSessionRetentionMinutes?: number;
}

/** core.AgentConfig (resolved configuration of one agent). */
export interface AgentConfig {
  id: string;
  factory_id: string;
  name: string;
  description: string;
  type: string;
  instructions: string;
  harness: { type: string; model?: string; reasoning_level?: string; [k: string]: unknown };
  machine?: MachineSpec;
  environment_id?: string;
  worker_host?: string;
  secrets?: string[];
  mcp_servers?: Record<string, unknown>;
  network_policy: EffectiveNetworkPolicy;
  spawnable_by: string[] | null;
  idle_timeout_minutes?: number;
  idle_timeout_set?: boolean;
  credential_strategy?: string;
  skills?: unknown[];
  snapshot?: boolean;
}

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
  failure_code?: FailureCode | "";
  blocked_reason?: string;
  queued_reason?: string;
  turns?: number;
  cancelled_by?: string;
  usage: Usage;
  triggered_by?: string;
  source?: string;
  source_ref?: string;
  creator?: Principal;
  run_as?: Principal;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  queued_at?: string;
}

export type SessionState = "assigned" | "active" | "ended";

export interface InstanceShape {
  name?: string;
  os?: string;
  arch?: string;
  vcpus?: number;
  memory_gb?: number;
}

export interface RunSession {
  run_id: string;
  session: number;
  task_id: string;
  state: SessionState;
  bootstrap_hash?: string;
  instance_hash?: string;
  bootstrap_expires_at: string;
  backend?: string;
  worker_id?: string;
  sandbox_id?: string;
  runner_version?: string;
  capabilities?: string[];
  active_turn_id?: string;
  assigned_at: string;
  started_at?: string;
  ready_at?: string;
  ended_at?: string;
  usage: Usage;
  instance_shape?: InstanceShape;
  compute_seconds?: number;
  end_reason?: string;
  failure_code?: FailureCode | "";
  sandbox_reason?: string;
  version: number;
  event_ack: number;
  command_seq: number;
  command_ack: number;
  last_seen_at: string;
}

export type RunInputState = "pending" | "delivered" | "consumed";

export interface RunInputSender {
  type: string;
  id?: string;
  name?: string;
}

export interface RunInput {
  message_id: string;
  run_id: string;
  seq: number;
  kind: string;
  sender: RunInputSender;
  content: string;
  data?: unknown;
  attachments?: string[];
  untrusted?: boolean;
  dedupe_key?: string;
  created_at: string;
  state: RunInputState;
  session?: number;
  turn_id?: string;
  accepted?: boolean;
}

export interface RunMessage {
  id: string;
  run_id: string;
  seq: number;
  role: string;
  content: string;
  data?: unknown;
  created_at: string;
}

export interface RunnerEvent {
  run_id: string;
  session: number;
  seq: number;
  type: string;
  turn_id?: string;
  at: string;
  data?: unknown;
}

export interface RunnerCommand {
  run_id: string;
  session: number;
  seq: number;
  type: string;
  data?: unknown;
  created_at: string;
}

export interface TimelineEvent {
  event_id: string;
  run_id: string;
  session: number;
  type: string;
  at: string;
  data?: unknown;
}

export interface SourceRef {
  provider?: string;
  kind?: string;
  repo?: string;
  number?: number;
  external_id?: string;
  channel?: string;
  thread?: string;
  title?: string;
  url?: string;
  [k: string]: unknown;
}

export interface WorkItemRef {
  kind: string;
  key: string;
  [k: string]: unknown;
}

export interface PullRequestRef {
  forge: string;
  repo: string;
  number: number;
  url: string;
  branch: string;
  state: string;
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
  created_at: string;
  updated_at: string;
  completed_at?: string;
  version: number;
}

export interface WorkItemEvent {
  id: string;
  work_item_id: string;
  kind: string;
  run_id?: string;
  actor?: string;
  data?: unknown;
  created_at: string;
}

export type InboxKind =
  "approval" | "question" | "handoff" | "failure" | "review" | "spend_limit" | "blocked" | "answered";

export interface Question {
  question_id: string;
  text: string;
  type: string;
  options?: string[];
  recommended_option?: string;
}

export interface InboxItem {
  id: string;
  factory_id: string;
  work_item_id?: string;
  run_id?: string;
  kind: InboxKind;
  title: string;
  body: string;
  options?: string[];
  questions?: Question[];
  assignee_id?: string;
  recipients?: string[];
  read_by?: string[];
  answer_message_id?: string;
  resolved: boolean;
  resolution?: string;
  resolved_by?: string;
  resolved_via?: string;
  created_at: string;
  resolved_at?: string;
  version: number;
}

/** Message outcomes of Dispatcher.Send (internal/dispatch/send.go). */
export type SendOutcome = "live_session" | "new_session" | "queued" | "stored";

export interface MessageReceipt {
  message_id: string;
  outcome: SendOutcome;
}
