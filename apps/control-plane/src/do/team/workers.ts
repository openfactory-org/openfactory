// TeamDO module: worker records and enrollment. Migrations 300-399.
//
// A worker is an enrolled host (kind "microvm": Firecracker per session) or a
// development process worker (kind "process", only with
// OF_DEV_PROCESS_WORKERS=true). The record is the admin's view: name,
// labels, slots, slot size, factory list (empty = all factories) and state.
// The WorkerHubDO of the worker pushes the live part (online/offline, facts,
// version, slots used, last seen) with the internal op "workers.hubState".
import { OfError, invalid, newId, notFound } from "@openfactory/core";
import type { CallMeta, Principal, TeamRpc } from "../../contracts/do-messages";
import { serverConfig } from "../../config";
import { bodyObject, optStringArray, optStringMap, optString, type JsonObject } from "../../lib/body";
import { fromAnyB64, randomToken, sha256Hex } from "../../lib/bytes";
import { isoOf, listResponse } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first, run } from "../../lib/sql/query";
import { registeredProviders } from "../dispatch/providers";
import type { DefinitionCheck } from "./factories/definitions";
import { activeMachines, agentFile, automationFile, findFactory, listFactories } from "./factories/repo";
import { created, noContent, type OpCall, type Preflight, type TeamKit, type TeamOp } from "./kit";
import type { CompiledDefinition } from "@openfactory/definition";

// ---------- schema ----------

const M0300 = `
CREATE TABLE workers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  conn TEXT NOT NULL DEFAULT 'pending',
  draining INTEGER NOT NULL DEFAULT 0,
  revoked_ms INTEGER,
  labels TEXT NOT NULL DEFAULT '{}',
  slots INTEGER NOT NULL DEFAULT 0,
  slot_size TEXT NOT NULL DEFAULT '{}',
  factories TEXT NOT NULL DEFAULT '[]',
  public_key TEXT NOT NULL DEFAULT '',
  facts TEXT NOT NULL DEFAULT '{}',
  version TEXT NOT NULL DEFAULT '',
  slots_used INTEGER NOT NULL DEFAULT 0,
  last_seen_ms INTEGER,
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX workers_name ON workers (name);
CREATE TABLE worker_enrollments (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  labels TEXT NOT NULL DEFAULT '{}',
  factories TEXT NOT NULL DEFAULT '[]',
  created_by TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL,
  used_ms INTEGER,
  worker_id TEXT
);
CREATE TABLE worker_repos (
  worker_id TEXT NOT NULL,
  repo TEXT NOT NULL,
  last_ms INTEGER NOT NULL,
  PRIMARY KEY (worker_id, repo)
);
CREATE TABLE worker_audit (
  id TEXT PRIMARY KEY,
  worker_id TEXT NOT NULL,
  action TEXT NOT NULL,
  outcome TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  at_ms INTEGER NOT NULL
);
CREATE INDEX worker_audit_worker ON worker_audit (worker_id, at_ms);
`;

export const migrations: readonly Migration[] = [{ version: 300, name: "workers", sql: M0300 }];

// ---------- types ----------

export type WorkerKind = "microvm" | "process";
export type WorkerStatus = "pending" | "online" | "offline" | "draining" | "revoked";

export interface SlotSizeJson {
  vcpus: number;
  memory_mib: number;
  scratch_gib?: number;
}

/** What WorkerHubDO needs of the record (op "workers.hubConnect" and configure). */
export interface WorkerRecord {
  id: string;
  name: string;
  kind: WorkerKind;
  publicKey: string;
  revoked: boolean;
  draining: boolean;
  labels: Record<string, string>;
  slots: number;
  slotSize: SlotSizeJson | null;
  factories: string[];
}

/** The Worker schema of openapi.json. */
export interface WorkerView {
  id: string;
  team_id: string;
  name: string;
  provider: WorkerKind;
  kind: WorkerKind;
  labels: Record<string, string>;
  capacity: number;
  active: number;
  last_heartbeat?: string;
  version?: string;
  status: WorkerStatus;
  slots: number;
  slots_used: number;
  slot_size?: SlotSizeJson;
  factories: string[];
  facts: Record<string, unknown>;
  last_seen?: string;
  created_at: string;
}

interface WorkerRow extends Record<string, SqlStorageValue> {
  id: string;
  name: string;
  kind: string;
  conn: string;
  draining: number;
  revoked_ms: number | null;
  labels: string;
  slots: number;
  slot_size: string;
  factories: string;
  public_key: string;
  facts: string;
  version: string;
  slots_used: number;
  last_seen_ms: number | null;
  created_ms: number;
  updated_ms: number;
}

// ---------- constants ----------

export const ENROLLMENT_PREFIX = "ofe_";
const ENROLLMENT_TTL_S = 3600;
const MIN_TTL_S = 60;
const MAX_TTL_S = 86400;
const MAX_SLOTS = 256;
/** A worker the hub has not confirmed for this long reads as offline. */
const STALE_MS = 3 * 60_000;
/** Repositories sent to a worker in the last 7 days may get a git credential. */
const REPO_WINDOW_MS = 7 * 24 * 3600_000;
const WORKER_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._ -]{0,63}$/;

/**
 * Worker IDs and names that a worker cannot use: they name built-in worker
 * hosts and sandbox providers (direct.ReservedWorkerIDs of the Go server).
 */
export const RESERVED_WORKER_IDS: readonly string[] = [
  "server",
  "local",
  "direct",
  "managed",
  "cloudflare-sandbox",
  "daytona",
  "boatdev",
];

/** ValidateWorkerID (internal/sandbox/direct/protocol.go). */
export function validateWorkerId(id: string): void {
  if (!WORKER_ID_RE.test(id))
    throw invalid(`worker id ${JSON.stringify(id)} must be 1-64 characters of letters, digits, '.', '_' or '-'`);
  const lower = id.toLowerCase();
  if (lower.startsWith("openfactory")) throw invalid(`worker ids starting with "openfactory" are reserved`);
  if (RESERVED_WORKER_IDS.includes(lower)) throw invalid(`worker id ${JSON.stringify(id)} is reserved`);
}

// ---------- helpers ----------

function parseJson<T>(s: string, fallback: T): T {
  try {
    return JSON.parse(s) as T;
  } catch {
    return fallback;
  }
}

function getRow(kit: TeamKit, id: string): WorkerRow | undefined {
  return first<WorkerRow>(kit.sql, "SELECT * FROM workers WHERE id = ?", id);
}

function mustRow(kit: TeamKit, id: string): WorkerRow {
  const r = getRow(kit, id);
  if (!r) throw notFound(`worker ${id}`);
  return r;
}

export function statusOf(
  r: Pick<WorkerRow, "conn" | "draining" | "revoked_ms" | "last_seen_ms">,
  nowMs = Date.now(),
): WorkerStatus {
  if (r.revoked_ms !== null) return "revoked";
  if (r.draining) return "draining";
  if (r.conn === "online") {
    return r.last_seen_ms !== null && nowMs - r.last_seen_ms > STALE_MS ? "offline" : "online";
  }
  return r.conn === "offline" ? "offline" : "pending";
}

function view(kit: TeamKit, r: WorkerRow): WorkerView {
  const slotSize = parseJson<SlotSizeJson | Record<string, never>>(r.slot_size, {});
  const out: WorkerView = {
    id: r.id,
    team_id: kit.teamId(),
    name: r.name,
    provider: r.kind as WorkerKind,
    kind: r.kind as WorkerKind,
    labels: parseJson(r.labels, {}),
    capacity: r.slots,
    active: r.slots_used,
    status: statusOf(r),
    slots: r.slots,
    slots_used: r.slots_used,
    factories: parseJson(r.factories, []),
    facts: parseJson(r.facts, {}),
    created_at: isoOf(r.created_ms),
  };
  if ("vcpus" in slotSize) out.slot_size = slotSize as SlotSizeJson;
  if (r.version) out.version = r.version;
  if (r.last_seen_ms !== null) {
    out.last_seen = isoOf(r.last_seen_ms);
    out.last_heartbeat = out.last_seen;
  }
  return out;
}

export function recordOf(r: WorkerRow): WorkerRecord {
  const slotSize = parseJson<SlotSizeJson | Record<string, never>>(r.slot_size, {});
  return {
    id: r.id,
    name: r.name,
    kind: r.kind as WorkerKind,
    publicKey: r.public_key,
    revoked: r.revoked_ms !== null,
    draining: r.draining === 1,
    labels: parseJson(r.labels, {}),
    slots: r.slots,
    slotSize: "vcpus" in slotSize ? (slotSize as SlotSizeJson) : null,
    factories: parseJson(r.factories, []),
  };
}

function audit(kit: TeamKit, workerId: string, action: string, outcome: string, detail: string, actor: string): void {
  kit.sql.exec(
    "INSERT INTO worker_audit (id, worker_id, action, outcome, detail, actor, at_ms) VALUES (?, ?, ?, ?, ?, ?, ?)",
    newId("wau"),
    workerId,
    action,
    outcome,
    detail,
    actor,
    Date.now(),
  );
}

function actorOf(p: Principal | undefined): string {
  return p ? `${p.kind}:${p.id}` : "";
}

function checkLabels(labels: Record<string, string> | undefined): Record<string, string> | undefined {
  if (!labels) return undefined;
  for (const k of Object.keys(labels)) {
    if (k === "" || k.length > 63 || /[\s:]/.test(k)) throw invalid(`invalid label key ${JSON.stringify(k)}`);
  }
  return labels;
}

function checkSlots(v: unknown, field = "slots"): number | undefined {
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "number" || !Number.isInteger(v) || v < 0 || v > MAX_SLOTS) {
    throw invalid(`${field} must be an integer between 0 and ${MAX_SLOTS}`);
  }
  return v;
}

function checkSlotSize(v: unknown, required: boolean): SlotSizeJson | undefined {
  if (v === undefined || v === null) {
    if (required) throw invalid("slot_size is required");
    return undefined;
  }
  if (typeof v !== "object" || Array.isArray(v)) throw invalid("slot_size must be an object");
  const o = v as JsonObject;
  const int = (k: string, min: number): number | undefined => {
    const x = o[k];
    if (x === undefined || x === null) return undefined;
    if (typeof x !== "number" || !Number.isInteger(x) || x < min)
      throw invalid(`slot_size.${k} must be an integer of at least ${min}`);
    return x;
  };
  const vcpus = int("vcpus", 1);
  const memory = int("memory_mib", 128);
  if (vcpus === undefined || memory === undefined) throw invalid("slot_size needs vcpus and memory_mib");
  const scratch = int("scratch_gib", 1);
  return { vcpus, memory_mib: memory, ...(scratch !== undefined ? { scratch_gib: scratch } : {}) };
}

function checkName(name: string): string {
  const n = name.trim();
  if (!NAME_RE.test(n)) throw invalid("name must be 1-64 letters, digits, spaces, dots, dashes or underscores");
  // A workerHost names a worker by ID or name: a name cannot shadow a built-in host.
  if (RESERVED_WORKER_IDS.includes(n.toLowerCase()) || n.toLowerCase().startsWith("openfactory"))
    throw invalid(`worker name ${JSON.stringify(n)} is reserved`);
  return n;
}

function nameTaken(kit: TeamKit, name: string, exceptId = ""): boolean {
  return first(kit.sql, "SELECT 1 FROM workers WHERE name = ? AND id != ?", name, exceptId) !== undefined;
}

/** OF_DEV_PROCESS_WORKERS, parsed like the other boolean vars (serverConfig). */
export function devAllowed(kit: Pick<TeamKit, "env">): boolean {
  return serverConfig(kit.env).devProcessWorkers;
}

// ---------- enrollment ----------

async function createEnrollment(kit: TeamKit, call: OpCall): Promise<unknown> {
  const p = call.principal();
  const b = bodyObject(call.body, true);
  const name = optString(b, "name");
  const labels = checkLabels(optStringMap(b, "labels")) ?? {};
  const factories = optStringArray(b, "factories") ?? [];
  const ttlRaw = b.expires_in_seconds;
  let ttl = ENROLLMENT_TTL_S;
  if (ttlRaw !== undefined && ttlRaw !== null) {
    if (typeof ttlRaw !== "number" || !Number.isInteger(ttlRaw) || ttlRaw < MIN_TTL_S || ttlRaw > MAX_TTL_S) {
      throw invalid(`expires_in_seconds must be between ${MIN_TTL_S} and ${MAX_TTL_S}`);
    }
    ttl = ttlRaw;
  }
  if (name !== undefined && name !== "") checkName(name);
  const token = ENROLLMENT_PREFIX + randomToken(32);
  const hash = await sha256Hex(token);
  const id = newId("wen");
  const now = Date.now();
  const expires = now + ttl * 1000;
  kit.tx(() => {
    kit.sql.exec(
      "INSERT INTO worker_enrollments (id, token_hash, name, labels, factories, created_by, created_ms, expires_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
      id,
      hash,
      name?.trim() ?? "",
      JSON.stringify(labels),
      JSON.stringify(factories),
      actorOf(p),
      now,
      expires,
    );
    // Purge enrollments that expired more than a day ago.
    kit.sql.exec("DELETE FROM worker_enrollments WHERE expires_ms < ?", now - 24 * 3600_000);
  });
  const url = (kit.env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  return created({
    id,
    token,
    expires_at: isoOf(expires),
    created_at: isoOf(now),
    command: `openfactory-runtime worker enroll --url ${url} --token ${token}`,
  });
}

/** TeamRpc.workersEnroll: public enrollment with a single-use token. */
export async function workersEnroll(
  kit: TeamKit,
  input: Parameters<TeamRpc["workersEnroll"]>[0],
): Promise<{ workerId: string }> {
  const token = input.enrollmentToken ?? "";
  if (!token.startsWith(ENROLLMENT_PREFIX)) throw new OfError("authentication_required", "invalid enrollment token");
  const hash = await sha256Hex(token);
  const now = Date.now();
  // The token is consumed first, in its own transaction: it is used up even if a later step fails.
  const enr = kit.tx(() => {
    const r = first<{
      id: string;
      name: string;
      labels: string;
      factories: string;
      expires_ms: number;
      used_ms: number | null;
    }>(
      kit.sql,
      "SELECT id, name, labels, factories, expires_ms, used_ms FROM worker_enrollments WHERE token_hash = ?",
      hash,
    );
    if (!r || r.used_ms !== null || r.expires_ms <= now) return undefined;
    kit.sql.exec("UPDATE worker_enrollments SET used_ms = ? WHERE id = ?", now, r.id);
    return r;
  });
  if (!enr) throw new OfError("authentication_required", "invalid, used or expired enrollment token");

  const key = fromAnyB64((input.publicKey ?? "").trim());
  if (!key || key.length !== 32) throw invalid("public_key must be the base64 of a raw 32-byte Ed25519 public key");
  const facts = (input.facts ?? {}) as Record<string, unknown>;
  if (typeof facts !== "object" || Array.isArray(facts)) throw invalid("facts must be an object");
  let kind: WorkerKind;
  if (facts.kvm === true) kind = "microvm";
  else if (devAllowed(kit)) kind = "process";
  else {
    throw invalid(
      "the host has no usable /dev/kvm: a worker needs KVM to run microVMs (development process workers need OF_DEV_PROCESS_WORKERS=true on the control plane)",
    );
  }
  const name = checkName(enr.name || input.name || "");
  const slots = checkSlots(input.slots) ?? 0;
  const slotSize = checkSlotSize(input.slotSize, kind === "microvm");
  if (kind === "microvm" && slots < 1) throw invalid("slots must be at least 1");
  const labels = { ...(checkLabels(input.labels) ?? {}), ...parseJson<Record<string, string>>(enr.labels, {}) };
  const workerId = newId("wkr");
  kit.tx(() => {
    if (nameTaken(kit, name)) throw new OfError("conflict", `a worker named ${JSON.stringify(name)} already exists`);
    kit.sql.exec(
      `INSERT INTO workers (id, name, kind, conn, labels, slots, slot_size, factories, public_key, facts, version, created_ms, updated_ms)
       VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      workerId,
      name,
      kind,
      JSON.stringify(labels),
      slots,
      JSON.stringify(slotSize ?? {}),
      enr.factories,
      input.publicKey.trim(),
      JSON.stringify(facts),
      typeof facts.runtime_version === "string" ? facts.runtime_version : "",
      now,
      now,
    );
    kit.sql.exec("UPDATE worker_enrollments SET worker_id = ? WHERE id = ?", workerId, enr.id);
    audit(kit, workerId, "enroll", "ok", `enrollment ${enr.id}`, "");
  });
  return { workerId };
}

// ---------- updates ----------

/** TeamRpc.workersUpdate: PATCH /workers/{id}. */
export async function workersUpdate(
  kit: TeamKit,
  meta: CallMeta,
  workerId: string,
  patch: unknown,
): Promise<WorkerView> {
  const b = bodyObject(patch);
  const name = optString(b, "name");
  const labels = checkLabels(optStringMap(b, "labels"));
  const slots = checkSlots(b.slots);
  const slotSize = checkSlotSize(b.slot_size, false);
  const factories = optStringArray(b, "factories");
  return kit.tx(() => {
    const r = mustRow(kit, workerId);
    if (r.revoked_ms !== null) throw new OfError("operation_not_supported", "the worker is revoked");
    const sets: string[] = [];
    const args: unknown[] = [];
    if (name !== undefined) {
      const n = checkName(name);
      if (nameTaken(kit, n, workerId))
        throw new OfError("conflict", `a worker named ${JSON.stringify(n)} already exists`);
      sets.push("name = ?");
      args.push(n);
    }
    if (labels !== undefined) {
      sets.push("labels = ?");
      args.push(JSON.stringify(labels));
    }
    if (slots !== undefined) {
      sets.push("slots = ?");
      args.push(slots);
    }
    if (slotSize !== undefined) {
      if (JSON.stringify(slotSize) !== r.slot_size && r.slots_used > 0) {
        throw new OfError("conflict", "the slot size cannot change while sessions run; drain the worker first");
      }
      sets.push("slot_size = ?");
      args.push(JSON.stringify(slotSize));
    }
    if (factories !== undefined) {
      sets.push("factories = ?");
      args.push(JSON.stringify([...new Set(factories)]));
    }
    if (sets.length > 0) {
      sets.push("updated_ms = ?");
      args.push(Date.now());
      kit.sql.exec(`UPDATE workers SET ${sets.join(", ")} WHERE id = ?`, ...args, workerId);
      audit(kit, workerId, "update", "ok", Object.keys(b).sort().join(","), actorOf(meta.principal));
    }
    return view(kit, mustRow(kit, workerId));
  });
}

function setDraining(kit: TeamKit, call: OpCall, draining: boolean): WorkerView {
  const id = call.params.id ?? "";
  return kit.tx(() => {
    const r = mustRow(kit, id);
    if (r.revoked_ms !== null) throw new OfError("operation_not_supported", "the worker is revoked");
    run(kit.sql, "UPDATE workers SET draining = ?, updated_ms = ? WHERE id = ?", draining ? 1 : 0, Date.now(), id);
    audit(kit, id, draining ? "drain" : "resume", "ok", "", actorOf(call.meta.principal));
    return view(kit, mustRow(kit, id));
  });
}

/** TeamRpc.workersRevoke: POST /workers/{id}/revoke. Refuses connections and git credentials for good. */
export async function workersRevoke(kit: TeamKit, meta: CallMeta, workerId: string): Promise<WorkerView> {
  return kit.tx(() => {
    const r = mustRow(kit, workerId);
    if (r.revoked_ms === null) {
      run(
        kit.sql,
        "UPDATE workers SET revoked_ms = ?, conn = 'offline', updated_ms = ? WHERE id = ?",
        Date.now(),
        Date.now(),
        workerId,
      );
      audit(kit, workerId, "revoke", "ok", "", actorOf(meta.principal));
    }
    return view(kit, mustRow(kit, workerId));
  });
}

// ---------- git credential ----------

/** Mints a read-only, single-repository credential. Registered by the forge module. */
export type GitCredentialMinter = (
  kit: TeamKit,
  input: { workerId: string; repo: string; factories: string[] },
) => Promise<{ username: string; password: string; expiresAt: string }>;

let gitCredentialMinter: GitCredentialMinter | null = null;

export function setGitCredentialMinter(fn: GitCredentialMinter | null): void {
  gitCredentialMinter = fn;
}

/** Clone URL normal form for the repository allow list: lower-case host, no ".git", no trailing slash. */
export function normalizeRepo(repo: string): string {
  const t = repo.trim();
  try {
    const u = new URL(t);
    const path = u.pathname.replace(/\/+$/, "").replace(/\.git$/, "");
    return `${u.protocol}//${u.host.toLowerCase()}${path}`;
  } catch {
    return t.replace(/\/+$/, "").replace(/\.git$/, "");
  }
}

async function gitCredential(kit: TeamKit, call: OpCall): Promise<unknown> {
  const p = call.principal();
  const id = call.params.id ?? "";
  const repo = normalizeRepo(call.query.repo ?? "");
  if (p.kind !== "worker" || p.id !== id)
    throw new OfError("not_authorized", "a worker can ask only for its own git credentials");
  if (repo === "") throw invalid("repo is required");
  const decision = kit.tx(() => {
    const r = getRow(kit, id);
    let refusal = "";
    if (!r) refusal = "unknown worker";
    else if (r.revoked_ms !== null) refusal = "the worker is revoked";
    else if (
      !first(
        kit.sql,
        "SELECT 1 FROM worker_repos WHERE worker_id = ? AND repo = ? AND last_ms >= ?",
        id,
        repo,
        Date.now() - REPO_WINDOW_MS,
      )
    ) {
      refusal = "the repository was not sent to this worker in the last 7 days";
    }
    audit(kit, id, "git_credential", refusal ? "refused" : "ok", refusal ? `${repo}: ${refusal}` : repo, actorOf(p));
    return { refusal, factories: r ? (parseJson(r.factories, []) as string[]) : [] };
  });
  if (decision.refusal) throw new OfError("not_authorized", decision.refusal);
  if (!gitCredentialMinter)
    throw new OfError("feature_not_available", "git credentials for the worker cache are not configured");
  const c = await gitCredentialMinter(kit, { workerId: id, repo, factories: decision.factories });
  return { username: c.username, password: c.password, expires_at: c.expiresAt };
}

// ---------- internal ops (WorkerHubDO and the Worker; not routes) ----------

/** "workers.hubConnect": the record for a connecting worker, or 404. */
function hubConnect(kit: TeamKit, call: OpCall): WorkerRecord {
  return recordOf(mustRow(kit, call.params.id ?? ""));
}

/** "workers.devRegister": records a development process worker that connects with a team API key. */
function devRegister(kit: TeamKit, call: OpCall): WorkerRecord {
  const p = call.principal();
  const id = call.params.id ?? "";
  validateWorkerId(id);
  if (!devAllowed(kit))
    throw new OfError("not_authorized", "development process workers are not allowed (OF_DEV_PROCESS_WORKERS)");
  if (p.role !== "admin") throw new OfError("not_authorized", "a development worker needs the API key of an admin");
  return kit.tx(() => {
    const r = getRow(kit, id);
    if (r) {
      if (r.revoked_ms !== null) throw new OfError("not_authorized", "the worker is revoked");
      if (r.kind !== "process" || r.public_key !== "") {
        throw new OfError("not_authorized", "this worker is enrolled: it must connect with its key");
      }
      return recordOf(r);
    }
    let name = id;
    for (let i = 2; nameTaken(kit, name); i++) name = `${id}-${i}`;
    const now = Date.now();
    kit.sql.exec(
      "INSERT INTO workers (id, name, kind, conn, created_ms, updated_ms) VALUES (?, ?, 'process', 'pending', ?, ?)",
      id,
      name,
      now,
      now,
    );
    audit(kit, id, "dev_register", "ok", "", actorOf(p));
    return recordOf(mustRow(kit, id));
  });
}

/** "workers.hubState": live state from the WorkerHubDO. */
function hubState(kit: TeamKit, call: OpCall): { ok: true } {
  const id = call.params.id ?? "";
  const b = bodyObject(call.body);
  kit.tx(() => {
    const r = getRow(kit, id);
    if (!r) return;
    const sets: string[] = [];
    const args: unknown[] = [];
    if (typeof b.online === "boolean" && r.revoked_ms === null) {
      sets.push("conn = ?");
      args.push(b.online ? "online" : "offline");
    }
    if (typeof b.slotsUsed === "number") {
      sets.push("slots_used = ?");
      args.push(b.slotsUsed);
    }
    if (typeof b.lastSeenMs === "number") {
      sets.push("last_seen_ms = MAX(COALESCE(last_seen_ms, 0), ?)");
      args.push(b.lastSeenMs);
    }
    if (typeof b.version === "string") {
      sets.push("version = ?");
      args.push(b.version);
    }
    if (b.facts && typeof b.facts === "object") {
      sets.push("facts = ?");
      args.push(JSON.stringify(b.facts));
    }
    // A development worker announces its own labels and capacity.
    if (r.kind === "process") {
      if (b.labels && typeof b.labels === "object") {
        sets.push("labels = ?");
        args.push(JSON.stringify(b.labels));
      }
      if (typeof b.capacity === "number") {
        sets.push("slots = ?");
        args.push(b.capacity);
      }
    }
    if (sets.length === 0) return;
    sets.push("updated_ms = ?");
    args.push(Date.now());
    kit.sql.exec(`UPDATE workers SET ${sets.join(", ")} WHERE id = ?`, ...args, id);
  });
  return { ok: true };
}

/** "workers.noteRepos": repositories sent to the worker for its git cache. */
function noteRepos(kit: TeamKit, call: OpCall): { ok: true } {
  const id = call.params.id ?? "";
  const repos = optStringArray(bodyObject(call.body), "repos") ?? [];
  kit.tx(() => {
    const now = Date.now();
    for (const repo of repos.slice(0, 50)) {
      kit.sql.exec(
        "INSERT INTO worker_repos (worker_id, repo, last_ms) VALUES (?, ?, ?) ON CONFLICT (worker_id, repo) DO UPDATE SET last_ms = excluded.last_ms",
        id,
        normalizeRepo(repo),
        now,
      );
    }
    kit.sql.exec("DELETE FROM worker_repos WHERE last_ms < ?", now - REPO_WINDOW_MS);
  });
  return { ok: true };
}

/** One row of "workers.list" and "workers.record". */
export type ListedWorker = WorkerRecord & { status: WorkerStatus; slotsUsed: number; arch: string };

function listed(r: WorkerRow): ListedWorker {
  return {
    ...recordOf(r),
    status: statusOf(r),
    slotsUsed: r.slots_used,
    arch: String(parseJson<Record<string, unknown>>(r.facts, {}).arch ?? ""),
  };
}

/** "workers.list": every record, for the local provider and DispatchDO. */
function listRecords(kit: TeamKit): ListedWorker[] {
  return all<WorkerRow>(kit.sql, "SELECT * FROM workers ORDER BY created_ms, id").map(listed);
}

// ---------- machines (GET /machines) ----------

/** Port of handleListMachines (internal/api/platform.go). */
function listMachines(kit: TeamKit, call: OpCall): unknown {
  const fid = call.query.factory_id ?? "";
  let fs: { id: string; name: string }[];
  if (fid !== "") {
    const f = findFactory(kit, fid);
    call.authorize(f?.id ?? "");
    if (!f) throw notFound(`factory ${fid}`);
    fs = [{ id: f.id, name: f.name }];
  } else {
    call.authorize("");
    fs = listFactories(kit);
  }
  const out: Record<string, unknown>[] = [];
  for (const f of fs) {
    const machines = activeMachines(kit, f.id);
    if (!machines) continue;
    for (const n of Object.keys(machines).sort()) {
      const spec = { ...machines[n] };
      if (!spec.name) spec.name = n;
      out.push({ factory_id: f.id, factory_name: f.name, ...spec });
    }
  }
  return listResponse(out, "");
}

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listWorkers: (kit) => listResponse(listRecordsView(kit), ""),
  getWorker: (kit, call) => view(kit, mustRow(kit, call.params.id ?? "")),
  createWorkerEnrollment: (kit, call) => createEnrollment(kit, call),
  updateWorker: (kit, call) => workersUpdate(kit, call.meta, call.params.id ?? "", call.body),
  deleteWorker: (kit, call) =>
    kit.tx(() => {
      const id = call.params.id ?? "";
      mustRow(kit, id);
      audit(kit, id, "delete", "ok", "", actorOf(call.meta.principal));
      kit.sql.exec("DELETE FROM workers WHERE id = ?", id);
      kit.sql.exec("DELETE FROM worker_repos WHERE worker_id = ?", id);
      return noContent();
    }),
  drainWorker: (kit, call) => setDraining(kit, call, true),
  resumeWorker: (kit, call) => setDraining(kit, call, false),
  revokeWorker: (kit, call) => workersRevoke(kit, call.meta, call.params.id ?? ""),
  getWorkerGitCredential: (kit, call) => gitCredential(kit, call),
  listMachines: (kit, call) => listMachines(kit, call),
  "workers.hubConnect": hubConnect,
  "workers.devRegister": devRegister,
  "workers.hubState": hubState,
  "workers.noteRepos": noteRepos,
  "workers.list": (kit) => listRecords(kit),
  /** One record in the "workers.list" row shape (params id), or 404. */
  "workers.record": (kit, call) => listed(mustRow(kit, call.params.id ?? "")),
};

function listRecordsView(kit: TeamKit): WorkerView[] {
  return all<WorkerRow>(kit.sql, "SELECT * FROM workers ORDER BY created_ms, id").map((r) => view(kit, r));
}

// ---------- definition preflight: worker hosts ----------

/** Hosts that are not workers: the default provider. */
const BUILTIN_HOSTS = new Set(["", "server", "managed"]);
/** Sandbox providers of the control plane (a workerHost may name one). */
const PROVIDER_HOSTS = new Set(["cloudflare-sandbox", "daytona", "boatdev", "microvm"]);

/** isProviderHost: a workerHost that names the default provider or a sandbox provider, not a worker. */
export function isProviderHost(host: string, providers: readonly string[]): boolean {
  return BUILTIN_HOSTS.has(host) || PROVIDER_HOSTS.has(host) || providers.includes(host);
}

/**
 * Port of preflightWorkerHosts (internal/engine/preflight.go): a workerHost of
 * an agent or an automation override must be a built-in host, a sandbox
 * provider, a registered worker of the team (ID or name) or a pool whose
 * label a worker has. Sync, no I/O.
 */
export function preflightWorkerHosts(
  kit: TeamKit,
  def: CompiledDefinition,
  chk: DefinitionCheck,
  providers: readonly string[] = [],
): void {
  const refs: { file: string; host: string }[] = [];
  const add = (file: string, host: string | undefined) => {
    if (host === undefined || isProviderHost(host, providers)) return;
    refs.push({ file, host });
  };
  for (const a of def.agents ?? []) add(agentFile(a.name), a.worker_host);
  for (const au of def.automations ?? []) add(automationFile(au.name), au.overrides?.worker_host);
  if (refs.length === 0) return;
  const known = new Set<string>();
  const labels = new Set<string>();
  for (const r of all<WorkerRow>(kit.sql, "SELECT * FROM workers WHERE revoked_ms IS NULL")) {
    known.add(r.id);
    known.add(r.name);
    for (const k of Object.keys(parseJson<Record<string, string>>(r.labels, {}))) labels.add(k);
  }
  const seen = new Set<string>();
  for (const r of refs) {
    const key = `${r.file}\0${r.host}`;
    if (seen.has(key)) continue;
    if (r.host.startsWith("pool:") ? labels.has(r.host.slice(5)) : known.has(r.host)) continue;
    seen.add(key);
    chk.errors.push({
      file: r.file,
      field: "workerHost",
      message: r.host.startsWith("pool:")
        ? `worker pool ${JSON.stringify(r.host)} has no registered worker with the label ${JSON.stringify(r.host.slice(5))}`
        : `worker host ${JSON.stringify(r.host)} is not a registered worker of the team or a sandbox provider of this server`,
    });
  }
}

export const preflights: readonly Preflight[] = [
  {
    name: "worker hosts",
    fn: (kit, _f, def, _files, chk) => preflightWorkerHosts(kit, def, chk, registeredProviders()),
  },
];
