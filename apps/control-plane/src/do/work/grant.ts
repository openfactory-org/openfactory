// Slot grant and task start (replaces claimRun).
// prepareGrant runs outside a transaction (provider capabilities, the
// capability gate, provider preflight, bootstrap hash). commitGrant is one
// transaction: session, prompt input, assigned, timeline, provider.start.
import { lookupFailure, newBootstrapToken } from "@openfactory/core";
import {
  ENV_LOOPBACK_DAEMON_PORTS,
  egressLockFor,
  failureForProviderError,
  gateNetworkPolicy,
  TASK_ENV,
  type SandboxTask,
  type TaskNetworkPolicy,
} from "@openfactory/sandbox";
import type { OutboxPayloads } from "../../contracts/do-messages";
import { sha256Hex } from "@openfactory/core";
import { requiredHosts } from "../../engine/hosts";
import { iso, ms, priorRetries, sortedUnique } from "../../engine/sessions";
import { queuedEventId, taskIdOf } from "../../engine/timeline";
import type { Run, RunSession } from "../../engine/types";
import { dest } from "../../lib/outbox/dest";
import { deniedWithoutRequired, mcpServerUrlsOf } from "../../runner-api/startconfig";
import { endSession, requestSlot, timeline, timerName, transition, type Fx } from "./engine";

export type Grant = OutboxPayloads["dispatch.grant"];

export interface PreparedGrant {
  kind: "start";
  grant: Grant;
  task: SandboxTask;
  bootstrapHash: string;
}

export type GrantPrep =
  | PreparedGrant
  | { kind: "release"; grant: Grant; reason: string }
  | { kind: "config_fault"; grant: Grant; code: string; message: string }
  | { kind: "retry_later"; grant: Grant; message: string };

/** The network policy of a task: required hosts are allowed and cannot be denied. */
export function taskPolicy(fx: Fx, run: Run): TaskNetworkPolicy | undefined {
  const np = run.config.network_policy;
  if (!np?.restricted) return undefined;
  const required = requiredHosts(run.config, {
    controlUrl: fx.cfg.controlUrl,
    r2AccountId: fx.cfg.r2AccountId,
    mcpServerUrls: mcpServerUrlsOf(run.config),
  });
  const denied = deniedWithoutRequired(np.denied_hosts, required);
  return {
    mode: "restricted",
    allowedHosts: sortedUnique([...(np.allowed_hosts ?? []), ...required]),
    deniedHosts: denied,
    alwaysAllow: required,
  };
}

export interface TaskExtras {
  arch: "amd64" | "arm64";
  egress: string;
  loopbackDaemonPorts?: readonly number[];
  /** sha256 of the runner binary for arch (R2 runner/<version>/openfactory-runtime-linux-<arch>.sha256). */
  runnerSha256?: string;
  /** The image of the run's environment (empty when it has none or the lookup failed). */
  environmentImage?: string;
}

/**
 * sandbox.SelectImage of the Go server: the image of the environment, then
 * the image of the machine, then the default image (OF_SANDBOX_IMAGE).
 */
export function selectImage(environmentImage: string | undefined, machineImage: string | undefined, def: string) {
  return environmentImage?.trim() || machineImage?.trim() || def;
}

export function buildTask(fx: Fx, run: Run, n: number, token: string, _grant: Grant, x: TaskExtras): SandboxTask {
  const policy = taskPolicy(fx, run);
  const arch = x.arch;
  const lock = egressLockFor(x.egress as "native", policy);
  const m = run.config.machine;
  return {
    taskId: taskIdOf(run.id, n),
    runId: run.id,
    session: n,
    teamId: fx.teamId,
    factoryId: run.factory_id,
    image: selectImage(x.environmentImage, m?.image, fx.cfg.sandboxImage),
    platform: { os: "linux", arch, ...(m?.user ? { user: m.user } : {}) },
    resources: {
      ...(m?.vcpus ? { vcpus: m.vcpus } : {}),
      ...(m?.memoryGb ? { memoryMiB: m.memoryGb * 1024 } : {}),
    },
    env: {
      [TASK_ENV.controlUrl]: fx.cfg.controlUrl,
      [TASK_ENV.runId]: run.id,
      [TASK_ENV.session]: String(n),
      [TASK_ENV.bootstrapToken]: token,
      [TASK_ENV.runnerVersion]: fx.cfg.runnerVersion,
      [TASK_ENV.egressLock]: lock,
      // The bootstrap substitutes {arch}.
      [TASK_ENV.runnerUrl]: `${fx.cfg.controlUrl}/runner/v1/bin/linux-{arch}`,
      ...(x.runnerSha256 ? { [TASK_ENV.runnerSha256]: x.runnerSha256 } : {}),
      ...(lock !== "none" && x.loopbackDaemonPorts?.length
        ? { [ENV_LOOPBACK_DAEMON_PORTS]: x.loopbackDaemonPorts.join(",") }
        : {}),
    },
    ...(policy ? { networkPolicy: policy } : {}),
    maxSessionSeconds: Math.round(fx.cfg.maxSessionMs / 1000),
    failureRetentionSeconds: (m?.failureSessionRetentionMinutes ?? 0) * 60,
    labels: { "of-task": taskIdOf(run.id, n) },
  };
}

/** Called inside the receive transaction of dispatch.grant: store it and schedule prepare. */
export function onGrant(fx: Fx, g: Grant): void {
  const run = fx.repo.getRun(g.runId);
  if (!run) {
    releaseGrant(fx, g, "unknown run");
    return;
  }
  fx.repo.addGrant(g);
  fx.setTimer(timerName.grant(g.runId, g.session), "grant", fx.now(), g);
}

export function releaseGrant(fx: Fx, g: Pick<Grant, "runId" | "session">, reason: string): void {
  fx.repo.setGrantState(g.runId, g.session, "released");
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.release",
    lane: "dispatch",
    msgId: `drel:${taskIdOf(g.runId, g.session)}`,
    payload: { runId: g.runId, session: g.session, computeSeconds: 0, endReason: reason },
  });
}

/** True when the grant can still start session n of the run. */
function grantUsable(fx: Fx, g: Grant): boolean {
  const run = fx.repo.getRun(g.runId);
  if (!run || run.state !== "queued") return false;
  const latest = fx.repo.latestSession(g.runId);
  const want = latest ? latest.session + 1 : 1;
  if (latest && latest.state !== "ended") return false;
  return want === g.session;
}

/** commit for a prepared grant (or its refusal). One transaction. */
export function commitGrant(fx: Fx, prep: GrantPrep): void {
  const g = prep.grant;
  const state = fx.repo.grantState(g.runId, g.session);
  if (state && state !== "pending") return; // done before a reset
  if (!grantUsable(fx, g)) {
    if (!fx.repo.getSession(g.runId, g.session)) releaseGrant(fx, g, "not queued");
    else fx.repo.setGrantState(g.runId, g.session, "used");
    return;
  }
  switch (prep.kind) {
    case "release":
      releaseGrant(fx, g, prep.reason);
      return;
    case "retry_later":
      releaseGrant(fx, g, "retry");
      fx.setTimer(timerName.requeue(g.runId), "requeue", fx.now() + 30_000, { runId: g.runId });
      return;
    case "config_fault":
      releaseGrant(fx, g, "config_fault");
      transition(fx, g.runId, { event: "config_fault", failureCode: prep.code }, (r) => {
        if (prep.message && r.state === "failed") r.error = `${r.failure_code}: ${prep.message}`;
      });
      return;
  }
  const run = fx.repo.mustRun(g.runId);
  const n = g.session;
  const now = fx.now();
  const task = prep.task;
  const s: RunSession = {
    run_id: run.id,
    session: n,
    task_id: task.taskId,
    state: "assigned",
    bootstrap_hash: prep.bootstrapHash,
    bootstrap_expires_at: iso(now + fx.cfg.startTimeoutMs),
    backend: g.workerId ? `worker:${g.workerId}` : g.providerId,
    ...(g.workerId ? { worker_id: g.workerId } : {}),
    assigned_at: iso(now),
    last_seen_at: iso(now),
    usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
    version: 0,
    event_ack: 0,
    command_seq: 0,
    command_ack: 0,
    ...(run.config.machine
      ? {
          instance_shape: {
            name: run.config.machine.name,
            os: run.config.machine.os,
            arch: run.config.machine.arch,
            ...(run.config.machine.vcpus ? { vcpus: run.config.machine.vcpus } : {}),
            ...(run.config.machine.memoryGb ? { memory_gb: run.config.machine.memoryGb } : {}),
          },
        }
      : {}),
  };
  fx.repo.createSession(s);
  // ensurePrompt: the dedupe key makes it a no-op when the engine queued it.
  if (run.prompt) {
    fx.repo.appendInput({
      message_id: "",
      run_id: run.id,
      kind: "user",
      sender: { type: "platform" },
      content: run.prompt,
      dedupe_key: "prompt",
    });
  }
  if (n === 1) {
    timeline(
      fx,
      run.id,
      0,
      "run.queued",
      queuedEventId(run),
      { reason: "new" },
      ms(run.queued_at) ?? ms(run.created_at),
    );
  }
  const { changed } = transition(fx, run.id, { event: "assigned" });
  fx.repo.setGrantState(g.runId, g.session, "used");
  if (!changed) {
    endSession(fx, s, "not_started", "");
    return;
  }
  fx.setTimer(timerName.start(task.taskId), "start_timeout", now + fx.cfg.startTimeoutMs + 1, {
    runId: run.id,
    session: n,
  });
  fx.enqueue({
    dest: `provider:${s.backend}`,
    kind: "provider.start",
    lane: `task:${task.taskId}`,
    msgId: `pstart:${task.taskId}`,
    payload: { target: g.target, providerId: g.providerId, ...(g.workerId ? { workerId: g.workerId } : {}), task },
  });
}

/** After provider.start succeeded: timeline and dispatch.taskStarted. */
export function startSucceeded(fx: Fx, runId: string, n: number, backend: string, workerId?: string): void {
  const task = taskIdOf(runId, n);
  const assigned: Record<string, unknown> = { backend };
  if (workerId) assigned.worker_id = workerId;
  timeline(fx, runId, n, "sandbox.assigned", "sandbox.assigned:" + task, assigned);
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.taskStarted",
    lane: "dispatch",
    msgId: `dstarted:${task}`,
    payload: { runId, session: n, taskId: task, startedMs: fx.now() },
  });
}

/** The wait before a run asks for a slot again after a start failed for capacity. */
export const CAPACITY_BACKOFF_MS = 2_000;

/** startFailed: end the session that never started and apply the lost event (claim.go). */
export function startFailed(fx: Fx, runId: string, n: number, err: unknown): void {
  const s = fx.repo.getSession(runId, n);
  if (!s || s.state !== "assigned") return;
  endSession(fx, s, "not_started", "");
  const f = failureForProviderError(err);
  let code = f.code ?? "resource_unavailable";
  let prior = priorRetries(fx.repo.listSessions(runId), n);
  if (!f.code || !f.consumesRetry) prior = 0;
  if (!lookupFailure(code)) code = "sandbox_create_failed";
  console.warn("work: start task failed", runId, n, err instanceof Error ? err.message : String(err));
  if (code === "resource_unavailable") fx.repo.setMeta(`slot_after:${runId}`, String(fx.now() + CAPACITY_BACKOFF_MS));
  transition(fx, runId, { event: "session_lost", failureCode: code, priorRetries: prior });
}

/** Bootstrap token and hash (outside a transaction: SHA-256 is async). */
export async function mintBootstrap(teamId: string, runId: string): Promise<{ token: string; hash: string }> {
  const token = newBootstrapToken(teamId, runId);
  return { token, hash: await sha256Hex(token) };
}

/** Gate result helper used by WorkDO.prepare. */
export { gateNetworkPolicy, requestSlot };
