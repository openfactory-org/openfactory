// Constants and helpers of runner protocol v1. Port of the constants of
// internal/runnerproto (paths.go, events.go, commands.go).

import type {
  ArtifactData,
  Command,
  DiagnosticData,
  Event,
  EventBatch,
  HarnessSessionData,
  IdleData,
  InputAcceptedData,
  MessageData,
  MessageDeliverData,
  SessionReadyData,
  SessionStopData,
  SetupStepData,
  SnapshotRestoredData,
  SnapshotSavedData,
  ToolCallData,
  ToolResultData,
  TranscriptSavedData,
  TurnEndedData,
  TurnInterruptData,
  TurnStartData,
  UsageData,
} from "./generated/index";
import { validateRunner, type RunnerDefName, type SchemaError } from "./validate";

export const RUNNER_VERSION = "runner/v1";
/** The header that carries the runner version. */
export const HEADER_RUNNER_VERSION = "OF-Runner-Version";

export const RUNNER_PATHS = {
  prefix: "/runner/v1",
  sessionStart: "/runner/v1/session/start",
  sessionSecrets: "/runner/v1/session/secrets",
  sessionEvents: "/runner/v1/session/events",
  sessionCommands: "/runner/v1/session/commands",
  sessionEnd: "/runner/v1/session/end",
  sessionUploads: "/runner/v1/session/uploads",
  sessionDownloads: "/runner/v1/session/downloads",
  gitCredential: "/runner/v1/session/git-credential",
  cloudToken: "/runner/v1/session/cloud-token",
  sessionToken: "/runner/v1/session/token",
  conversation: "/runner/v1/session/conversation",
} as const;

/** The longest wait of a commands request (long-poll), in seconds. */
export const MAX_COMMAND_WAIT_SECONDS = 25;
export const MAX_EVENT_CONTENT_BYTES = 256 * 1024;
export const MAX_SETUP_OUTPUT_BYTES = 16 * 1024;
export const DEFAULT_EVENT_BATCH_BYTES = 1 << 20;
/** A runner that sends nothing for this long is lost. */
export const RUNNER_SILENCE_SECONDS = 90;

/** Error codes of runner protocol answers. */
export const CODE_SESSION_SUPERSEDED = "session_superseded";
export const CODE_RUN_CANCELLED = "run_cancelled";
export const CODE_CONFIGURATION_ERROR = "configuration_error";

/** Limits the wait of a commands request to 0..MAX_COMMAND_WAIT_SECONDS. */
export function clampWait(seconds: number): number {
  if (!(seconds > 0)) return 0;
  return Math.min(Math.floor(seconds), MAX_COMMAND_WAIT_SECONDS);
}

export type EventType = Event["type"];
export type CommandType = Command["type"];
export type TurnOutcome = TurnEndedData["outcome"];

/** The data type of each event type. */
export interface EventDataMap {
  "setup.step": SetupStepData;
  "snapshot.restored": SnapshotRestoredData;
  "session.ready": SessionReadyData;
  "turn.started": undefined;
  "input.accepted": InputAcceptedData;
  message: MessageData;
  tool_call: ToolCallData;
  tool_result: ToolResultData;
  "harness.session": HarnessSessionData;
  usage: UsageData;
  artifact: ArtifactData;
  "transcript.saved": TranscriptSavedData;
  diagnostic: DiagnosticData;
  "turn.ended": TurnEndedData;
  idle: IdleData;
  "snapshot.saved": SnapshotSavedData;
}

/** The data type of each command type. */
export interface CommandDataMap {
  "turn.start": TurnStartData;
  "message.deliver": MessageDeliverData;
  "turn.interrupt": TurnInterruptData;
  "session.stop": SessionStopData;
  "session.cancel": undefined;
}

/** The schema definition of the data of each event type (null: no data). */
export const EVENT_DATA_DEFS: Readonly<Record<EventType, RunnerDefName | null>> = {
  "setup.step": "events#SetupStepData",
  "snapshot.restored": "events#SnapshotRestoredData",
  "session.ready": "events#SessionReadyData",
  "turn.started": null,
  "input.accepted": "events#InputAcceptedData",
  message: "events#MessageData",
  tool_call: "events#ToolCallData",
  tool_result: "events#ToolResultData",
  "harness.session": "events#HarnessSessionData",
  usage: "events#UsageData",
  artifact: "events#ArtifactData",
  "transcript.saved": "events#TranscriptSavedData",
  diagnostic: "events#DiagnosticData",
  "turn.ended": "events#TurnEndedData",
  idle: "events#IdleData",
  "snapshot.saved": "events#SnapshotSavedData",
};

/** The schema definition of the data of each command type (null: no data). */
export const COMMAND_DATA_DEFS: Readonly<Record<CommandType, RunnerDefName | null>> = {
  "turn.start": "commands#TurnStartData",
  "message.deliver": "commands#MessageDeliverData",
  "turn.interrupt": "commands#TurnInterruptData",
  "session.stop": "commands#SessionStopData",
  "session.cancel": null,
};

/** Reports whether t is a known event type. */
export function isEventType(t: string): t is EventType {
  return t in EVENT_DATA_DEFS;
}

/** Reports whether t is a known command type. */
export function isCommandType(t: string): t is CommandType {
  return t in COMMAND_DATA_DEFS;
}

/** A runner event. at defaults to now. */
export function newEvent<T extends EventType>(
  seq: number,
  type: T,
  data: EventDataMap[T],
  opts: { turnId?: string; at?: Date | string } = {},
): Event {
  const ev: Event = {
    seq,
    type,
    at: opts.at === undefined ? new Date().toISOString() : new Date(opts.at).toISOString(),
  };
  if (opts.turnId) ev.turn_id = opts.turnId;
  if (data !== undefined) ev.data = data;
  return ev;
}

/** A control-plane command. */
export function newCommand<T extends CommandType>(seq: number, type: T, data: CommandDataMap[T]): Command {
  const c: Command = { seq, type };
  if (data !== undefined) c.data = data;
  return c;
}

/** The typed data of an event of a known type. Call validateEvent first for untrusted input. */
export function eventData<T extends EventType>(ev: Event & { type: T }): EventDataMap[T] {
  return ev.data as EventDataMap[T];
}

/** The typed data of a command of a known type. */
export function commandData<T extends CommandType>(c: Command & { type: T }): CommandDataMap[T] {
  return c.data as CommandDataMap[T];
}

function prefixed(errors: SchemaError[], prefix: string): SchemaError[] {
  return errors.map((e) => ({ ...e, instanceLocation: prefix + e.instanceLocation.replace(/^#/, "") }));
}

/** Validates an event envelope and its data. */
export function validateEvent(value: unknown): SchemaError[] {
  const r = validateRunner("events#Event", value);
  if (!r.valid) return r.errors;
  const def = EVENT_DATA_DEFS[r.value.type];
  if (def === null) return [];
  const d = validateRunner(def, r.value.data);
  return d.valid ? [] : prefixed(d.errors, "#/data");
}

/** Validates a command envelope and its data. */
export function validateCommand(value: unknown): SchemaError[] {
  const r = validateRunner("commands#Command", value);
  if (!r.valid) return r.errors;
  const def = COMMAND_DATA_DEFS[r.value.type];
  if (def === null) return [];
  const d = validateRunner(def, r.value.data);
  return d.valid ? [] : prefixed(d.errors, "#/data");
}

/** Validates an event batch, each event with its data. Locations name the event index. */
export function validateEventBatch(value: unknown): { batch?: EventBatch; errors: SchemaError[] } {
  const r = validateRunner("events#EventBatch", value);
  if (!r.valid) return { errors: r.errors };
  const errors: SchemaError[] = [];
  (r.value.events ?? []).forEach((ev, i) => {
    errors.push(...prefixed(validateEvent(ev), `#/events/${i}`));
  });
  return errors.length > 0 ? { errors } : { batch: r.value, errors };
}
