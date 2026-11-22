// The contract between TeamDO and its modules. Module files import their
// types and result helpers from here, never from ./index, so the modules do
// not form an import cycle with the class that composes them.
import type { CallMeta, OutboxKind, Principal } from "../../contracts/do-messages";
import type { RouteRole } from "../../contracts/routes";
import type { Env } from "../../contracts/env";
import type { MessageHandler } from "../../lib/base";
import type { LiveHub } from "../../lib/live/index";
import type { EnqueueInput } from "../../lib/outbox/outbox";
import type { OpResponse } from "../../lib/rpc";
import type { Migration } from "../../lib/sql/migrate";
import type { AsyncTimerHandler, SyncTimerHandler, Timers } from "../../lib/timers/timers";
import type { PreflightCheck } from "./factories";
import type { RewrapSource } from "./secrets";

export type { OpError, OpResponse } from "../../lib/rpc";

/** What a module sees of the TeamDO. */
export interface TeamKit {
  readonly env: Env;
  readonly sql: SqlStorage;
  readonly storage: DurableObjectStorage;
  readonly timers: Timers;
  readonly live: LiveHub;
  /** The preflight checks of all modules, in module order. */
  readonly preflights: readonly Preflight[];
  /** The sealed values of all modules that rewrap re-encrypts, besides secrets. */
  readonly rewrapSources: readonly RewrapSource[];
  teamId(): string;
  doName(): string;
  /** One transactionSync; re-arms the alarm after. No await inside. */
  tx<T>(fn: () => T): T;
  /** Outbox row; call inside tx with the state change. */
  enqueue<K extends string>(input: EnqueueInput<K>): string;
  setTimer(name: string, kind: string, dueMs: number, payload?: unknown): void;
  clearTimer(name: string): boolean;
  onTimer<P = unknown>(kind: string, handler: SyncTimerHandler<P>): void;
  onTimerAsync<P = unknown>(kind: string, handler: AsyncTimerHandler<P>): void;
  onMessage<K extends OutboxKind>(kind: K, handler: MessageHandler<K>): void;
  /** Receiver-side dedupe helpers for custom flows. */
  seen(msgId: string): boolean;
  markSeen(msgId: string): void;
}

/** One call of TeamDO.api. */
export interface OpCall {
  op: string;
  meta: CallMeta;
  /** Path parameters. */
  params: Record<string, string>;
  /** Query parameters (single values). */
  query: Record<string, string>;
  body: unknown;
  /** Minimum role of the route ("" for ops that are not routes). */
  role: RouteRole | "";
  /** The principal, or throws 401. */
  principal(): Principal;
  /**
   * Checks the route role against the principal's role for this factory
   * ("" = team role). Factory-scoped routes must call it before reading
   * the resource, with "" when the factory does not exist (then 404).
   */
  authorize(factoryId: string): Principal;
}

export type TeamOp = (kit: TeamKit, call: OpCall) => unknown;

/** A definition check of a module, run after the built-in checks of factories.ts. */
export interface Preflight {
  name: string;
  fn: PreflightCheck;
}

/** What each module file exports. */
export interface TeamModule {
  migrations: readonly Migration[];
  ops: Record<string, TeamOp>;
  setup?: (kit: TeamKit) => void;
  /** Definition checks of this module. */
  preflights?: readonly Preflight[];
  /** Sealed values of this module that POST /secrets/rewrap re-encrypts. */
  rewrapSources?: readonly RewrapSource[];
}

export const created = (body: unknown, headers?: Record<string, string>): OpResponse => ({
  $status: 201,
  body,
  ...(headers ? { headers } : {}),
});
export const noContent = (): OpResponse => ({ $status: 204 });
export const withStatus = (status: number, body?: unknown): OpResponse => ({ $status: status, body });
