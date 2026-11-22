// The effect context of the WorkDO engine. Every engine function takes an Fx
// and runs inside the caller's transactionSync: it reads and writes through
// fx.repo, and effects leave through fx.enqueue (outbox), fx.setTimer and
// fx.wake*.
import { newRoutedId } from "@openfactory/core";
import type { DispatchTarget } from "../../contracts/do-messages";
import type { EnqueueInput } from "../../lib/outbox/outbox";
import type { Repo } from "./repo";

export interface EngineConfig {
  publicUrl: string;
  controlUrl: string;
  r2AccountId: string;
  queueTimeoutMs: number;
  maxSessionMs: number;
  stopGraceMs: number;
  startTimeoutMs: number;
  lostAfterMs: number;
  maxDepth: number;
  runnerVersion: string;
  allowUnenforced: string;
  /** Default task image (OF_SANDBOX_IMAGE); an environment or machine image wins (grant.ts selectImage). */
  sandboxImage: string;
  /** OF_OBJECT_STORE_RETENTION: run data in R2 older than this is pruned; 0 keeps it. */
  retentionMs?: number;
}

/** sandbox.DefaultImage of the Go server. */
export const DEFAULT_SANDBOX_IMAGE = "ghcr.io/openfactory-org/sandbox:latest";

export interface Fx {
  repo: Repo;
  now(): number;
  teamId: string;
  /** Partition token of this WorkDO. */
  token: string;
  doName: string;
  cfg: EngineConfig;
  target(workerHost: string | undefined): DispatchTarget;
  enqueue(input: EnqueueInput): string;
  setTimer(name: string, kind: string, dueMs: number, payload?: unknown): void;
  /** Sets the timer unless it is already due earlier. */
  setEarliestTimer?(name: string, kind: string, dueMs: number, payload?: unknown): void;
  clearTimer(name: string): void;
  wakeCommands(runId: string, session: number): void;
  wakeRun(runId: string): void;
}

/** Timer names of WorkDO. */
export const timerName = {
  grant: (runId: string, n: number) => `grant:${runId}.${n}`,
  start: (task: string) => `start:${task}`,
  live: (task: string) => `live:${task}`,
  deadline: (task: string) => `deadline:${task}`,
  requeue: (runId: string) => `requeue:${runId}`,
  compact: (runId: string) => `compact:${runId}`,
  retention: "retention",
};

export function newMsgId(fx: Fx): string {
  return newRoutedId("msg", fx.token);
}
