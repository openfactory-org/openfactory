// IDs, partition tokens and Durable Object names.
//
// Format:
//
//   token = kind(1) + 15 base32 chars          80 random bits, or a hash
//   id    = prefix "_" token rand(12)          28 chars after "_"
//
// kind: "w" = WorkDO, "i" = IndexDO-owned, "b" = BenchmarkDO.
// Base32 is RFC 4648 without padding, lower case (the Go core.NewID alphabet).
// IDs that do not route (TeamDO-owned: usr_, fac_, sec_, ...) keep the Go
// shape: prefix "_" 20 base32 chars (96 random bits).

import { OfError } from "./errors";

/** The single team of the core install. Credentials without a team map to it. */
export const DEFAULT_TEAM = "default";

export type TokenKind = "w" | "i" | "b";

export const TOKEN_LENGTH = 16;
export const ROUTED_RAND_LENGTH = 12;
export const PLAIN_ID_LENGTH = 20;

const ALPHABET = "abcdefghijklmnopqrstuvwxyz234567";
const TOKEN_RE = /^[wib][a-z2-7]{15}$/;
const ROUTED_BODY_RE = /^[wib][a-z2-7]{27}$/;

/** Lower-case base32 (RFC 4648 alphabet, no padding) of the bytes. */
export function base32(bytes: Uint8Array): string {
  let out = "";
  let bits = 0;
  let value = 0;
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      out += ALPHABET[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += ALPHABET[(value << (5 - bits)) & 31];
  return out;
}

function randomBase32(chars: number): string {
  const bytes = new Uint8Array(Math.ceil((chars * 5) / 8));
  crypto.getRandomValues(bytes);
  return base32(bytes).slice(0, chars);
}

/** A new random partition token of the given kind. */
export function newToken(kind: TokenKind): string {
  return kind + randomBase32(TOKEN_LENGTH - 1);
}

/**
 * A deterministic token: kind + base32(sha256(parts joined by "|"))[:15].
 * Used for new work items without a router reservation, so a retried action
 * reaches the same WorkDO.
 */
export async function hashToken(kind: TokenKind, ...parts: string[]): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(parts.join("|")));
  return kind + base32(new Uint8Array(digest)).slice(0, TOKEN_LENGTH - 1);
}

export function isToken(s: string): boolean {
  return TOKEN_RE.test(s);
}

/** A plain random ID with the Go shape, for IDs that do not route. */
export function newId(prefix: string): string {
  const body = randomBase32(PLAIN_ID_LENGTH);
  return prefix === "" ? body : `${prefix}_${body}`;
}

/** An ID that carries its partition token: `<prefix>_<token><rand12>`. */
export function newRoutedId(prefix: string, token: string): string {
  if (!isToken(token)) throw new OfError("invalid_request", `invalid partition token ${JSON.stringify(token)}`);
  return `${prefix}_${token}${randomBase32(ROUTED_RAND_LENGTH)}`;
}

/** Splits an ID into prefix and body at the first "_". */
export function splitId(id: string): { prefix: string; body: string } {
  const i = id.indexOf("_");
  if (i <= 0) return { prefix: "", body: id };
  return { prefix: id.slice(0, i), body: id.slice(i + 1) };
}

export function isRoutedId(id: string, prefix?: string): boolean {
  const { prefix: p, body } = splitId(id);
  if (prefix !== undefined && p !== prefix) return false;
  return p !== "" && ROUTED_BODY_RE.test(body);
}

/**
 * The partition token of a routed ID. A task ID (`<runId>.<n>`) gives the
 * token of its run. Throws OfError resource_not_found for an ID without a
 * token, so that a malformed ID reads as "not found", never as a 500.
 */
export function partitionOf(id: string): string {
  const runId = id.includes(".") ? id.slice(0, id.lastIndexOf(".")) : id;
  const { prefix, body } = splitId(runId);
  if (prefix === "" || !ROUTED_BODY_RE.test(body)) {
    throw new OfError("resource_not_found", `id ${JSON.stringify(id)} has no partition token`);
  }
  return body.slice(0, TOKEN_LENGTH);
}

export function tokenKind(token: string): TokenKind {
  if (!isToken(token)) throw new OfError("invalid_request", `invalid partition token ${JSON.stringify(token)}`);
  return token[0] as TokenKind;
}

// ---------- Durable Object names ----------
// Every key has the form team:<id>|… (plan, "Durable Objects").

export const doName = {
  team: (teamId: string): string => `team:${teamId}`,
  work: (teamId: string, token: string): string => `team:${teamId}|work:${token}`,
  dispatch: (teamId: string): string => `team:${teamId}|dispatch`,
  router: (teamId: string): string => `team:${teamId}|router`,
  index: (teamId: string): string => `team:${teamId}|index`,
  ingress: (teamId: string, partition: string): string => `team:${teamId}|ingress:${partition}`,
  workerHub: (teamId: string, workerId: string): string => `team:${teamId}|worker:${workerId}`,
  benchmark: (teamId: string, token: string): string => `team:${teamId}|bench:${token}`,
} as const;

/** WorkDO name for any routed ID of that work item (run, inbox item, message). */
export function workDOName(teamId: string, id: string): string {
  return doName.work(teamId, partitionOf(id));
}

// ---------- Task IDs (port of core.TaskID / core.ParseTaskID) ----------

export function taskId(runId: string, session: number): string {
  return `${runId}.${session}`;
}

export function parseTaskId(id: string): { runId: string; session: number } {
  const i = id.lastIndexOf(".");
  if (i < 0) throw new OfError("invalid_request", `task id ${JSON.stringify(id)} has no dot`);
  const runId = id.slice(0, i);
  if (runId === "") throw new OfError("invalid_request", `task id ${JSON.stringify(id)} has an empty run id`);
  const tail = id.slice(i + 1);
  const n = /^[+-]?\d+$/.test(tail) ? Number(tail) : Number.NaN;
  if (!Number.isSafeInteger(n) || n < 1) {
    throw new OfError("invalid_request", `task id ${JSON.stringify(id)} has an invalid session number`);
  }
  return { runId, session: n };
}

// ---------- Bootstrap tokens: ofb_<team>.<run>.<secret43> ----------

export const BOOTSTRAP_PREFIX = "ofb_";

function base64url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** A new bootstrap token. The secret is 32 random bytes in base64url (43 chars). */
export function newBootstrapToken(teamId: string, runId: string): string {
  if (teamId.includes(".") || runId.includes(".")) {
    throw new OfError("invalid_request", "team and run IDs must not contain a dot");
  }
  const secret = new Uint8Array(32);
  crypto.getRandomValues(secret);
  return `${BOOTSTRAP_PREFIX}${teamId}.${runId}.${base64url(secret)}`;
}

export function parseBootstrapToken(token: string): { teamId: string; runId: string; secret: string } {
  if (!token.startsWith(BOOTSTRAP_PREFIX)) throw new OfError("authentication_required", "not a bootstrap token");
  const parts = token.slice(BOOTSTRAP_PREFIX.length).split(".");
  const [teamId, runId, secret] = parts;
  if (parts.length !== 3 || !teamId || !runId || !secret || !/^[A-Za-z0-9_-]{43}$/.test(secret)) {
    throw new OfError("authentication_required", "malformed bootstrap token");
  }
  return { teamId, runId, secret };
}
