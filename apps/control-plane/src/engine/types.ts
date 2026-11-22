// Types of the engine. The domain types are the ones of @openfactory/core;
// this file adds the WorkDO storage shapes (sessions, inputs, runner events
// and commands) and the few stored shapes that differ from core on purpose.
// JSON shapes follow the Go json tags (internal/core/types.go,
// internal/core/runnerstore.go) so that REST bodies and stored `data`
// columns port 1:1.
import type {
  FailureCode,
  InboxItem as CoreInboxItem,
  MessageReceipt as CoreMessageReceipt,
  Run as CoreRun,
  Usage,
} from "@openfactory/core";

export type {
  AgentConfig,
  EffectiveNetworkPolicy,
  InboxKind,
  MachineSpec,
  Principal,
  PullRequestRef,
  Question,
  RunMessage,
  RunState,
  SourceRef,
  Stage,
  Usage,
  WorkItem,
  WorkItemEvent,
  WorkItemRef,
} from "@openfactory/core";
export { STAGES } from "@openfactory/core";

/** core.Run as stored: applyResult writes failure_code "" for a failed turn without a code. */
export interface Run extends Omit<CoreRun, "failure_code"> {
  failure_code?: FailureCode | "";
}

/** core.InboxItem as stored: resolved_via is the caller's value; WorkDO does not check it against InboxVia. */
export interface InboxItem extends Omit<CoreInboxItem, "resolved_via"> {
  resolved_via?: string;
}

/** core.RunState.Terminal */
export function isTerminal(s: string): boolean {
  return s === "succeeded" || s === "failed" || s === "cancelled" || s === "error";
}

/** core.RunState.WaitsForInput */
export function waitsForInput(s: string): boolean {
  return s === "idle" || s === "blocked";
}

export function stageTerminal(s: string): boolean {
  return s === "complete" || s === "cancelled";
}

export const ZERO_USAGE: Usage = { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 };

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

/** Message outcomes of Dispatcher.Send (internal/dispatch/send.go): core's outcomes plus "stored" for a message that starts no turn. */
export type SendOutcome = CoreMessageReceipt["outcome"] | "stored";

/** core.MessageReceipt with the WorkDO outcome "stored". */
export interface MessageReceipt extends Omit<CoreMessageReceipt, "outcome"> {
  outcome: SendOutcome;
}
