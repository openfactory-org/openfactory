// The handshake of worker protocol v2 as WorkerHubDO runs it, without DO
// state: challenge -> hello -> ready.
//
// - An enrolled worker (mode "key") gets auth.challenge first and proves its
//   Ed25519 key with auth.response; then it sends hello.
// - A development process worker (mode "apikey") was checked by the Worker
//   with a team API key and sends hello first.
// - A connection in phase "ready" exchanges requests, responses and events.
import type { TaskStatus } from "@openfactory/sandbox";
import { fromWireStatus, workerAuthMessage, type WireFrame, type WireHello } from "@openfactory/sandbox/microvm";
import { b64url, fromAnyB64, fromB64url, randomBytes } from "../../lib/bytes";
import type { WorkerRecord } from "../team/workers";
import type { HelloSummary } from "./store";

/** Heartbeat interval announced in welcome. */
export const HEARTBEAT_MS = 15_000;
/** No ping or frame for this long closes the connection (3 intervals). */
export const LIVENESS_MS = 45_000;
/** The handshake (challenge answer and hello) must finish in this time. */
export const HANDSHAKE_MS = 10_000;
export const PING_TEXT = '{"type":"ping"}';
export const PONG_TEXT = '{"type":"pong"}';

export type Mode = "key" | "apikey";
export type Phase = "challenge" | "hello" | "ready";

/** Per-socket state, kept in the WebSocket attachment (survives hibernation). */
export interface Attachment {
  connId: string;
  mode: Mode;
  phase: Phase;
  nonce?: string;
  openedMs: number;
  lastSeenMs: number;
}

/** A close code and reason for the socket. */
export interface Refusal {
  code: number;
  reason: string;
}

export function closeQuietly(ws: WebSocket, code: number, reason: string): void {
  try {
    ws.close(code, reason);
  } catch {
    /* already closed */
  }
}

/** a < b for dotted numeric versions ("v1.4.0"); non-numeric versions never compare lower. */
export function versionLess(a: string, b: string): boolean {
  const parse = (v: string): number[] | null => {
    const m = /^v?(\d+(?:\.\d+)*)/.exec(v.trim());
    return m ? m[1]!.split(".").map(Number) : null;
  };
  const x = parse(a);
  const y = parse(b);
  if (!x || !y) return false;
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d !== 0) return d < 0;
  }
  return false;
}

/** The attachment of a new connection; mode "key" starts with a fresh nonce. */
export function openAttachment(connId: string, mode: Mode, now: number): Attachment {
  const att: Attachment = {
    connId,
    mode,
    phase: mode === "key" ? "challenge" : "hello",
    openedMs: now,
    lastSeenMs: now,
  };
  if (mode === "key") att.nonce = b64url(randomBytes(32));
  return att;
}

/** The auth.challenge frame for an attachment in phase "challenge". */
export function challengeFrame(att: Attachment): string {
  return JSON.stringify({
    type: "auth.challenge",
    params: { nonce: att.nonce, connection_id: att.connId, protocols: [1] },
  });
}

/** The close of a connection that did not finish the handshake in time. */
export function handshakeTimeout(phase: Phase): Refusal {
  return phase === "challenge" ? { code: 1008, reason: "auth_failed" } : { code: 1002, reason: "expected hello" };
}

/** The refusal of a frame that its phase does not accept, or null. */
export function frameRefusal(phase: Phase, f: WireFrame): Refusal | null {
  if (phase === "challenge" && f.type !== "auth.response") return { code: 1008, reason: "auth_failed" };
  if (phase === "hello" && f.type !== "hello") return { code: 1002, reason: "expected hello" };
  return null;
}

/** Checks auth.response: the worker signed the challenge with the key of its record. */
export async function verifyAuthResponse(
  rec: WorkerRecord | undefined,
  att: Attachment,
  params: unknown,
): Promise<boolean> {
  const p = (params ?? {}) as { worker_id?: unknown; alg?: unknown; sig?: unknown };
  return (
    !!rec &&
    p.worker_id === rec.id &&
    p.alg === "ed25519" &&
    typeof p.sig === "string" &&
    (await verifyEd25519(rec.publicKey, p.worker_id, att.connId, att.nonce ?? "", p.sig))
  );
}

/** Ed25519 check of workerAuthMessage(workerId, connId, nonce); false on any malformed input. */
export async function verifyEd25519(
  publicKey: string,
  workerId: string,
  connId: string,
  nonce: string,
  sig: string,
): Promise<boolean> {
  try {
    const raw = fromAnyB64(publicKey);
    if (!raw || raw.length !== 32) return false;
    const key = await crypto.subtle.importKey("raw", raw, { name: "Ed25519" }, false, ["verify"]);
    return await crypto.subtle.verify(
      { name: "Ed25519" },
      key,
      fromB64url(sig),
      workerAuthMessage(workerId, connId, nonce) as Uint8Array<ArrayBuffer>,
    );
  } catch {
    return false;
  }
}

/** The refusal of a hello, or null when the hub accepts it. */
export function helloRefusal(
  rec: WorkerRecord | undefined,
  att: Attachment,
  hello: WireHello | undefined,
  minRuntimeVersion: string,
): Refusal | null {
  if (!rec || !hello || hello.worker_id !== rec.id) return { code: 1008, reason: "worker_id does not match" };
  if (att.mode === "key" && (hello.protocol_minor ?? 0) < 1) return { code: 1008, reason: "runtime_too_old" };
  if (minRuntimeVersion && versionLess(hello.version ?? "", minRuntimeVersion))
    return { code: 1008, reason: "runtime_too_old" };
  return null;
}

/** The tasks of a hello whose status parses and names the same task. */
export function heldTasks(hello: WireHello): Map<string, TaskStatus> {
  const held = new Map<string, TaskStatus>();
  for (const t of hello.tasks ?? []) {
    const s = fromWireStatus(t?.status);
    if (s && s.taskId === t.task_id) held.set(s.taskId, s);
  }
  return held;
}

export function helloSummary(hello: WireHello): HelloSummary {
  return {
    version: hello.version ?? "",
    capacity: Math.max(0, hello.capacity ?? 0),
    labels: hello.labels ?? {},
    facts: (hello.facts as Record<string, unknown> | undefined) ?? null,
    features: hello.features ?? [],
    slots: hello.slots ?? 0,
  };
}

/** The result of the welcome frame (without the access token). */
export function welcomeResult(
  connId: string,
  draining: boolean,
  versions: { minRuntimeVersion: string; recommendedRuntimeVersion: string },
): Record<string, unknown> {
  const welcome: Record<string, unknown> = { connection_id: connId, heartbeat_ms: HEARTBEAT_MS, draining };
  if (versions.minRuntimeVersion) welcome.min_runtime_version = versions.minRuntimeVersion;
  if (versions.recommendedRuntimeVersion) welcome.recommended_runtime_version = versions.recommendedRuntimeVersion;
  return welcome;
}
