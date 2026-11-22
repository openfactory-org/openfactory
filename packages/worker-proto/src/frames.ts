// Frame helpers of worker protocol v2: encode and decode text frames, build
// requests, responses and events, and the enrolled-worker auth signature.

import type {
  AssignParams,
  AuthChallengeFrame,
  CancelParams,
  DrainParams,
  EventFrame,
  HeartbeatFrame,
  Hello,
  HelloFrame,
  PingFrame,
  PongFrame,
  RequestFrame,
  ResponseFrame,
  StatusResult,
  TaskListResult,
  TaskRef,
  TaskStatus,
  Welcome,
  WelcomeFrame,
} from "./generated/index";
import { validateFrame, type SchemaError, type WorkerFrame } from "./validate";

/** The params and the result of each request method. */
export interface WorkerMethods {
  "task.assign": { params: AssignParams; result: StatusResult };
  "task.status": { params: TaskRef; result: StatusResult };
  "task.destroy": { params: TaskRef; result: Record<string, never> };
  "task.cancel": { params: CancelParams; result: StatusResult };
  "task.list": { params: Record<string, never>; result: TaskListResult };
  drain: { params: DrainParams; result: Record<string, never> };
}

export type WorkerMethod = keyof WorkerMethods;

export const WORKER_METHODS: readonly WorkerMethod[] = ["task.assign", "task.status", "task.destroy", "task.cancel", "task.list", "drain"];

/** Error codes of a response frame. */
export type WorkerErrorCode = NonNullable<ResponseFrame["error"]>["code"];

export const WORKER_ERROR_CODES: readonly WorkerErrorCode[] = ["not_found", "at_capacity", "invalid_spec", "internal", "unknown_method"];

/** A frame that failed to decode. */
export class FrameError extends Error {
  override readonly name = "FrameError";
  constructor(
    message: string,
    readonly errors: SchemaError[] = [],
  ) {
    super(message);
  }
}

/** Encodes a frame as a WebSocket text message. */
export function encodeFrame(frame: WorkerFrame): string {
  return JSON.stringify(frame);
}

/** Decodes and validates a text message. Throws FrameError. */
export function decodeFrame(text: string | ArrayBuffer | ArrayBufferView): WorkerFrame {
  const s = typeof text === "string" ? text : new TextDecoder().decode(text);
  let v: unknown;
  try {
    v = JSON.parse(s);
  } catch {
    throw new FrameError("worker-proto: frame is not JSON");
  }
  const r = validateFrame(v);
  if (!r.valid) {
    const t = v !== null && typeof v === "object" ? String((v as { type?: unknown }).type ?? "") : "";
    throw new FrameError(`worker-proto: invalid ${t || "frame"}: ${r.errors[0]?.instanceLocation ?? ""} ${r.errors[0]?.error ?? ""}`.trim(), r.errors);
  }
  return r.value;
}

/** A request frame. */
export function requestFrame<M extends WorkerMethod>(id: string, method: M, params: WorkerMethods[M]["params"]): RequestFrame {
  return { type: "request", id, method, params } as RequestFrame;
}

/** A successful response frame. */
export function responseFrame(id: string, result: StatusResult | TaskListResult | Record<string, never> = {}): ResponseFrame {
  return { type: "response", id, result };
}

/** An error response frame. */
export function errorFrame(id: string, code: WorkerErrorCode, message: string): ResponseFrame {
  return { type: "response", id, error: { code, message } };
}

/** A task.state event frame. */
export function taskStateFrame(status: TaskStatus): EventFrame {
  return { type: "event", method: "task.state", params: { status } };
}

export function helloFrame(params: Hello): HelloFrame {
  return { type: "hello", params };
}

export function welcomeFrame(result: Welcome): WelcomeFrame {
  return { type: "welcome", result };
}

export function heartbeatFrame(active: number, capacity: number): HeartbeatFrame {
  return { type: "heartbeat", params: { active, capacity } };
}

export const PING_FRAME: PingFrame = { type: "ping" };
export const PONG_FRAME: PongFrame = { type: "pong" };

/** Narrows a frame by its type. */
export function isFrameType<T extends WorkerFrame["type"]>(f: WorkerFrame, type: T): f is Extract<WorkerFrame, { type: T }> {
  return f.type === type;
}

// ---------- enrolled worker auth ----------

/** The prefix of the bytes that an enrolled worker signs. */
export const AUTH_SIGNATURE_PREFIX = "openfactory-worker-auth-v1";

/** The bytes that an enrolled worker signs: "openfactory-worker-auth-v1\n<worker_id>\n<connection_id>\n<nonce>\n". */
export function authMessage(workerId: string, connectionId: string, nonce: string): Uint8Array {
  return new TextEncoder().encode(`${AUTH_SIGNATURE_PREFIX}\n${workerId}\n${connectionId}\n${nonce}\n`);
}

/** Base64url without padding. */
export function base64url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Decodes base64url (with or without padding). */
export function fromBase64url(s: string): Uint8Array {
  const b = s.replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b + "=".repeat((4 - (b.length % 4)) % 4));
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** A new auth challenge frame with a 32-byte random nonce. */
export function authChallengeFrame(connectionId: string, protocols?: number[]): AuthChallengeFrame {
  const nonce = new Uint8Array(32);
  crypto.getRandomValues(nonce);
  const params: AuthChallengeFrame["params"] = { nonce: base64url(nonce), connection_id: connectionId };
  if (protocols) params.protocols = protocols;
  return { type: "auth.challenge", params };
}

/**
 * Verifies the Ed25519 signature of an auth.response. publicKey is the raw
 * 32-byte key of the enrolled worker.
 */
export async function verifyAuthSignature(
  publicKey: Uint8Array,
  workerId: string,
  connectionId: string,
  nonce: string,
  sig: string,
): Promise<boolean> {
  let sigBytes: Uint8Array;
  try {
    sigBytes = fromBase64url(sig);
  } catch {
    return false;
  }
  if (publicKey.length !== 32 || sigBytes.length !== 64) return false;
  const key = await crypto.subtle.importKey("raw", publicKey, { name: "Ed25519" }, false, ["verify"]);
  return crypto.subtle.verify({ name: "Ed25519" }, key, sigBytes, authMessage(workerId, connectionId, nonce));
}
