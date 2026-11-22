// The asynchronous effects of WorkDO: grant preparation, provider and notice
// deliveries of the outbox, the liveness checks of a session and the retention
// sweep of run data. Each one does its network I/O first and then commits its
// decision in one transaction (txFx); no await runs inside a transaction.
import { doName, lookupFailure, OfError } from "@openfactory/core";
import {
  failureForSandboxReason,
  gateNetworkPolicy,
  parseProviderError,
  type SandboxProvider,
  type TaskStatus,
} from "@openfactory/sandbox";
import type { Ack, CallMeta, OutboxEnvelope, OutboxPayloads } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { unwrapOp } from "../../lib/rpc";
import type { TimerRow } from "../../lib/timers/timers";
import { decideDeadline, decideSilent } from "../../engine/liveness";
import type { Run } from "../../engine/types";
import { pruneRunData } from "../../retention/sweep";
import { defaultProviderId, providerFor } from "../dispatch/providers";
import { loseCode, loseSession, transition, type Fx } from "./engine";
import {
  buildTask,
  commitGrant,
  mintBootstrap,
  startFailed,
  startSucceeded,
  taskPolicy,
  type Grant,
  type GrantPrep,
} from "./grant";
import type { Repo } from "./repo";
import { enqueueCommand } from "./runner";

/** What the effects need from their WorkDO. */
export interface WorkHost {
  readonly env: Env;
  readonly teamId: string;
  /** A repository for reads outside a transaction. */
  repo(): Repo;
  /** An Fx outside a transaction, for the reads that take an Fx (task policy, task build). */
  readFx(): Fx;
  /** One transaction with an Fx; wakes and the outbox kick follow its commit. */
  txFx<T>(fn: (fx: Fx) => T): T;
  /** One transaction without an Fx. */
  tx<T>(fn: () => T): T;
  /** Sets the timer unless it is already due earlier. */
  setEarliestTimer(name: string, kind: string, dueMs: number, payload: unknown): void;
}

// ---------- notices ----------

/** Delivers notices (the notify service). bootstrapControlPlane in src/wiring.ts sets it. */
export type NoticeDeliverer = (env: Env, envelope: OutboxEnvelope) => Promise<void>;
let noticeDeliverer: NoticeDeliverer | undefined;

/** Sets the deliverer (tests replace it). Returns the one it replaces. */
export function setNoticeDeliverer(fn: NoticeDeliverer | undefined): NoticeDeliverer | undefined {
  const prev = noticeDeliverer;
  noticeDeliverer = fn;
  return prev;
}

/** Delivers one notice. Without a registered deliverer it throws, so the outbox keeps the notice and retries it. */
export async function deliverNotice(env: Env, envelope: OutboxEnvelope): Promise<Ack> {
  if (!noticeDeliverer) {
    console.error("work: no notice deliverer is registered: call bootstrapControlPlane()");
    throw new OfError("internal_error", "no notice deliverer is registered");
  }
  await noticeDeliverer(env, envelope);
  return { ok: true };
}

// ---------- grants ----------

/** The timer "grant": checks the provider and builds the task, then commits the decision. */
export async function prepareAndCommitGrant(host: WorkHost, g: Grant): Promise<void> {
  let prep: GrantPrep;
  try {
    prep = await prepareGrant(host, g);
  } catch (e) {
    prep = { kind: "retry_later", grant: g, message: e instanceof Error ? e.message : String(e) };
  }
  host.txFx((fx) => commitGrant(fx, prep));
}

async function prepareGrant(host: WorkHost, g: Grant): Promise<GrantPrep> {
  const run = host.repo().getRun(g.runId);
  if (!run || run.state !== "queued") return { kind: "release", grant: g, reason: "not queued" };
  const provider = providerFor(host.env, host.teamId, g.providerId || defaultProviderId(host.env), g.workerId);
  const caps = await provider.capabilities();
  const fx = host.readFx();
  const policy = taskPolicy(fx, run);
  const gate = gateNetworkPolicy(caps, policy, { allowUnenforced: fx.cfg.allowUnenforced });
  if (!gate.ok) return { kind: "config_fault", grant: g, code: gate.code, message: gate.message };
  if (gate.warning) console.warn("work: gate:", gate.warning, run.id);
  const { token, hash } = await mintBootstrap(host.teamId, run.id);
  const arch = caps.arch[0] ?? "amd64";
  const task = buildTask(fx, run, g.session, token, g, {
    arch,
    egress: gate.egress,
    loopbackDaemonPorts: caps.loopbackDaemonPorts,
    runnerSha256: await runnerSha256(host.env, arch),
    environmentImage: await environmentImage(host.env, run),
  });
  const pre = await provider.preflight(task);
  if (!pre.ok) {
    // A user-class failure is a configuration fault; an unknown code is the platform's.
    if (lookupFailure(pre.code)?.class === "user")
      return { kind: "config_fault", grant: g, code: pre.code, message: pre.message };
    return { kind: "retry_later", grant: g, message: pre.message };
  }
  if (pre.resources) task.resources = pre.resources;
  return { kind: "start", grant: g, task, bootstrapHash: hash };
}

/**
 * The image of the environment of a run (taskImageFor in the Go server). A
 * failed lookup is logged and gives no image, so the next image applies.
 */
async function environmentImage(env: Env, run: Run): Promise<string | undefined> {
  const id = run.config.environment_id;
  if (!id) return undefined;
  try {
    const meta: CallMeta = {
      requestId: `env-image:${run.id}`,
      principal: { kind: "run", id: run.id, runId: run.id, teamId: run.team_id, role: "admin", factoryRoles: {} },
    };
    const out = unwrapOp<{ image?: string } | null>(
      await env.TEAM.getByName(doName.team(run.team_id || "default")).api(meta, "getEnvironment", { id }, null),
    );
    return out?.image?.trim() || undefined;
  } catch (e) {
    console.warn("work: sandbox image: read environment failed, using the next image", run.id, id, e);
    return undefined;
  }
}

/** The checksum of the published runner binary, or undefined when it is not in R2. */
async function runnerSha256(env: Env, arch: string): Promise<string | undefined> {
  const v = env.OF_RUNNER_VERSION;
  if (!v) return undefined;
  try {
    const obj = await env.DATA.get(`runner/${v}/openfactory-runtime-linux-${arch}.sha256`);
    const text = obj ? (await obj.text()).trim() : "";
    return text.split(/\s+/)[0] || undefined;
  } catch {
    return undefined;
  }
}

// ---------- providers ----------

/** Delivers one outbox row to a sandbox provider (destination provider:<id>). */
export async function deliverProvider(host: WorkHost, envelope: OutboxEnvelope): Promise<Ack> {
  const { env, teamId: team } = host;
  switch (envelope.kind) {
    case "provider.start": {
      const p = envelope.payload as OutboxPayloads["provider.start"];
      const provider = providerFor(env, team, p.providerId, p.workerId);
      try {
        await provider.start(p.task);
        host.txFx((fx) =>
          startSucceeded(
            fx,
            p.task.runId,
            p.task.session,
            p.workerId ? `worker:${p.workerId}` : p.providerId,
            p.workerId,
          ),
        );
      } catch (e) {
        host.txFx((fx) => startFailed(fx, p.task.runId, p.task.session, e));
      }
      return { ok: true };
    }
    case "provider.ack": {
      const p = envelope.payload as OutboxPayloads["provider.ack"];
      const prov = providerOf(env, team, p.providerId, p.workerId);
      if (canAck(prov)) await ignoreNotFound(prov.ack(p.taskId));
      return { ok: true };
    }
    case "provider.cancel": {
      const p = envelope.payload as OutboxPayloads["provider.cancel"];
      await ignoreNotFound(providerOf(env, team, p.providerId, p.workerId).cancel(p.taskId, p.graceSeconds));
      return { ok: true };
    }
    case "provider.destroy": {
      const p = envelope.payload as OutboxPayloads["provider.destroy"];
      await ignoreNotFound(providerOf(env, team, p.providerId, p.workerId).destroy(p.taskId));
      return { ok: true };
    }
  }
  throw new Error(`unexpected provider kind ${envelope.kind}`);
}

/** A provider that can forget the bootstrap token of a started task. `ack` is not part of SandboxProvider. */
type AckingProvider = SandboxProvider & { ack(taskId: string): Promise<void> };

function canAck(p: SandboxProvider): p is AckingProvider {
  return "ack" in p && typeof p.ack === "function";
}

/** The provider of a session backend: a provider ID, or worker:<id> for a worker host. */
function providerOf(env: Env, team: string, backend: string, workerId?: string) {
  if (workerId) return providerFor(env, team, "", workerId);
  return providerFor(
    env,
    team,
    backend.startsWith("worker:") ? "" : backend,
    backend.startsWith("worker:") ? backend.slice(7) : undefined,
  );
}

async function ignoreNotFound(p: Promise<void>): Promise<void> {
  try {
    await p;
  } catch (e) {
    if (parseProviderError(e)?.kind === "not_found") return;
    throw e;
  }
}

// ---------- liveness ----------

type SessionTimer = TimerRow<{ runId: string; session: number }>;

/** The timer "deadline": waits, asks the runner to stop, or loses the session. */
export function onDeadline(fx: Fx, t: SessionTimer): void {
  const s = fx.repo.getSession(t.payload.runId, t.payload.session);
  if (!s) return;
  const d = decideDeadline(s, fx.repo.stopSentMs(s.run_id, s.session), fx.now(), {
    maxSessionMs: fx.cfg.maxSessionMs,
    stopGraceMs: fx.cfg.stopGraceMs,
    lostAfterMs: fx.cfg.lostAfterMs,
  });
  switch (d.kind) {
    case "wait":
      fx.setTimer(t.name, "deadline", d.atMs, t.payload);
      return;
    case "stop":
      enqueueCommand(fx, s, "session.stop", { reason: "deadline", snapshot: true });
      fx.repo.setStopSent(s.run_id, s.session, fx.now());
      if (d.applyDeadlineEvent) transition(fx, s.run_id, { event: "session_deadline" });
      fx.setTimer(t.name, "deadline", fx.now() + fx.cfg.stopGraceMs + fx.cfg.lostAfterMs + 1, t.payload);
      return;
    case "lose":
      loseSession(fx, s, "deadline_exceeded", d.applyEvent, { cancel: true });
      return;
  }
}

/**
 * The timer "liveness": a silent session is asked for its task status at its
 * provider, then waits again or is lost.
 */
export async function onLiveness(host: WorkHost, t: SessionTimer): Promise<void> {
  const repo = host.repo();
  const s = repo.getSession(t.payload.runId, t.payload.session);
  if (!s || s.state === "ended") return;
  const { lostAfterMs } = host.readFx().cfg;
  let d = decideSilent(s, Date.now(), { lostAfterMs });
  if (d.kind === "ask_status") {
    let status: TaskStatus | "not_found" | "unavailable";
    try {
      status = await providerOf(host.env, host.teamId, s.backend ?? "", s.worker_id).status(s.task_id);
    } catch (e) {
      status = parseProviderError(e)?.kind === "not_found" ? "not_found" : "unavailable";
    }
    const cur = repo.getSession(s.run_id, s.session);
    if (!cur || cur.state === "ended") return;
    d = decideSilent(cur, Date.now(), { lostAfterMs }, status);
  }
  const action = d;
  host.txFx((fx) => {
    const cur = fx.repo.getSession(s.run_id, s.session);
    if (!cur || cur.state === "ended") return;
    if (action.kind === "wait" || action.kind === "ask_status") {
      const at = action.kind === "wait" ? action.recheckAtMs : Date.now() + lostAfterMs;
      fx.setTimer(t.name, "liveness", Math.max(at, Date.now() + 1000), t.payload);
      return;
    }
    if (action.useReasonMap)
      loseCode(fx, cur, action.reason, failureForSandboxReason(action.reason) ?? "", true, { cancel: action.cancel });
    else loseCode(fx, cur, action.reason, action.code, true, { cancel: action.cancel });
  });
}

// ---------- retention ----------

/** Finished runs read in one retention step. */
export const RETENTION_PAGE = 200;

const CURSOR_KEY = "retention_cursor";
const MIN_LEFT_KEY = "retention_min_left";

/**
 * One retention step: deletes the R2 data (transcript, compacted messages,
 * snapshots) of the finished runs of one page that are older than keepMs. The
 * rows stay. Pages go by run ID; the cursor and the oldest activity of the
 * runs that stay are kept in the meta table between steps. After the last
 * page the next timer is set for the run that expires first.
 */
export async function retentionStep(
  host: WorkHost,
  keepMs: number,
  pageSize = RETENTION_PAGE,
): Promise<{ more: boolean }> {
  if (keepMs <= 0) return { more: false };
  const now = Date.now();
  const repo = host.repo();
  const cursor = repo.getMeta(CURSOR_KEY) ?? "";
  const page = repo.listUnprunedFinishedRuns(cursor, pageSize);
  const runs = page.map((r) => ({ id: r.id, lastActivityMs: Date.parse(r.finished_at ?? r.created_at) || now }));
  const out = await pruneRunData({ bucket: host.env.DATA, teamId: host.teamId, runs, nowMs: now, retentionMs: keepMs });
  return host.tx(() => {
    const repo = host.repo();
    for (const id of out.pruned) repo.setRunState(id, "data_pruned", String(now));
    // The delete budget ended inside this page: the next step repeats it without the pruned runs.
    if (out.more) return { more: true };
    const pruned = new Set(out.pruned);
    let minLeft = Number(repo.getMeta(MIN_LEFT_KEY) || "Infinity");
    for (const r of runs) if (!pruned.has(r.id)) minLeft = Math.min(minLeft, r.lastActivityMs);
    if (page.length === pageSize) {
      repo.setMeta(CURSOR_KEY, page[page.length - 1]!.id);
      repo.setMeta(MIN_LEFT_KEY, String(minLeft));
      return { more: true };
    }
    repo.setMeta(CURSOR_KEY, "");
    repo.setMeta(MIN_LEFT_KEY, "");
    if (Number.isFinite(minLeft)) host.setEarliestTimer("retention", "retention", minLeft + keepMs + 60_000, null);
    return { more: false };
  });
}
