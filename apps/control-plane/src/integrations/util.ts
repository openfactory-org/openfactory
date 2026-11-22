// Shared helpers of the integrations (port of forgeutil webhook.go and text.go).
import { OfError } from "@openfactory/core";
import type { Integration, OutboundMessage } from "@openfactory/core";
import type { SecretReader } from "./types";

const enc = new TextEncoder();

/** Most bytes of an inbound webhook body. */
export const MAX_WEBHOOK_BODY = 10 << 20;

export function toHex(b: ArrayBuffer | Uint8Array): string {
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  let s = "";
  for (const x of u) s += x.toString(16).padStart(2, "0");
  return s;
}

export function fromHex(s: string): Uint8Array | null {
  if (s.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(s)) return null;
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.slice(i * 2, i * 2 + 2), 16);
  return out;
}

export async function hmacKey(secret: string | Uint8Array): Promise<CryptoKey> {
  const raw = typeof secret === "string" ? enc.encode(secret) : secret;
  return crypto.subtle.importKey("raw", raw, { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
}

export async function hmacSha256(secret: string | Uint8Array, data: string | Uint8Array): Promise<Uint8Array> {
  const k = await hmacKey(secret);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, typeof data === "string" ? enc.encode(data) : data));
}

export async function hmacSha256Hex(secret: string | Uint8Array, data: string | Uint8Array): Promise<string> {
  return toHex(await hmacSha256(secret, data));
}

/** Compares two byte strings without an early exit. Different lengths are not equal. */
export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i]! ^ b[i]!;
  return d === 0;
}

/** Constant-time compare of two secrets. Empty values never match. */
export function constantTimeEqual(a: string, b: string): boolean {
  if (a === "" || b === "") return false;
  return bytesEqual(enc.encode(a), enc.encode(b));
}

/** Checks a hex HMAC-SHA256 signature (an optional "sha256=" prefix is stripped). */
export async function verifyHmacSha256Hex(secret: string, body: string | Uint8Array, sig: string): Promise<boolean> {
  if (secret === "") return false;
  const s = sig.trim().replace(/^sha256=/, "");
  const got = fromHex(s);
  if (!got) return false;
  return bytesEqual(await hmacSha256(secret, body), got);
}

/** Reads at most MAX_WEBHOOK_BODY bytes of the request body as text. Throws when it is longer. */
export async function readBody(req: Request): Promise<string> {
  const buf = new Uint8Array(await req.arrayBuffer());
  if (buf.length > MAX_WEBHOOK_BODY) throw new Error("request body too large");
  return new TextDecoder().decode(buf);
}

export function jsonResponse(status: number, v: unknown): Response {
  return new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
}

export function errorResponse(status: number, code: string, message: string): Response {
  return jsonResponse(status, { error: { code, message } });
}

/** Remembers recently seen delivery IDs to drop provider retries. IngressDO dedupes durably on top of this. */
export class Dedupe {
  private readonly seen = new Map<string, number>();
  constructor(
    private readonly ttlMs: number,
    private readonly now: () => number = Date.now,
  ) {}

  /** Records id and reports whether it was recorded within the TTL. */
  isSeen(id: string): boolean {
    if (id === "") return false;
    const now = this.now();
    if (this.seen.size > 10000) {
      for (const [k, t] of this.seen) if (now - t > this.ttlMs) this.seen.delete(k);
    }
    const t = this.seen.get(id);
    if (t !== undefined && now - t <= this.ttlMs) return true;
    this.seen.set(id, now);
    return false;
  }
}

/** The secret named by in.secret_ref, or by in.config[configKey] when configKey is set. */
export async function integrationSecret(
  secrets: SecretReader | undefined,
  integration: Integration,
  configKey = "",
): Promise<string> {
  const name = configKey ? (integration.config?.[configKey] ?? "") : (integration.secret_ref ?? "");
  if (name === "") {
    throw new OfError("resource_not_found", `integration ${integration.id} (${integration.provider}): no secret configured`);
  }
  if (!secrets) throw new Error(`integration ${integration.id}: no secret store configured`);
  try {
    return await secrets.get(integration.team_id, name);
  } catch (e) {
    throw new Error(`integration ${integration.id}: secret ${name}: ${(e as Error).message}`, { cause: e });
  }
}

export function cfg(integration: Pick<Integration, "config">, key: string): string {
  return integration.config?.[key] ?? "";
}

/** Shortens s to at most n characters, adding an ellipsis when cut. */
export function truncate(s: string, n: number): string {
  const r = Array.from(s);
  if (r.length <= n) return s;
  if (n <= 1) return r.slice(0, n).join("");
  return r.slice(0, n - 1).join("") + "…";
}

/** Renders an outbound message as markdown for comment-based destinations. */
export function formatOutbound(msg: Pick<OutboundMessage, "kind" | "text" | "options">): string {
  let b = "";
  switch (msg.kind) {
    case "question":
      b += "**Question from the factory**\n\n";
      break;
    case "approval_request":
      b += "**Approval needed**\n\n";
      break;
    case "error":
      b += "**The factory hit an error**\n\n";
      break;
  }
  b += msg.text.trim();
  if (msg.options && msg.options.length > 0) {
    b += "\n\nReply with one of:\n";
    for (const o of msg.options) b += `- \`${o}\`\n`;
  }
  return b.replace(/\n+$/, "");
}

/** The distinct non-empty values of in, in order. */
export function stringSet(...list: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const s of list) {
    if (s === "" || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
  }
  return out;
}

/** The elements of after that are not in before. */
export function diff(after: string[], before: string[]): string[] {
  const b = new Set(before);
  return after.filter((s) => !b.has(s));
}

export function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Case-insensitive equality (Go strings.EqualFold, simple folding). */
export function equalFold(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}
