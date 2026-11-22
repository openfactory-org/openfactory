// TeamDO module: secrets (WS4). Envelope-encrypted secrets (src/secrets),
// the REST routes of internal/api/platform.go, secret resolution for runs,
// and rewrap (internal/rewrap) over every sealed value of the team.
//
// Other modules seal their own values (MCP OAuth tokens, user forge tokens)
// with sealValue/openValue and register a RewrapSource so that
// POST /secrets/rewrap covers them.
import { OfError, newId } from "@openfactory/core";
import type { CallMeta } from "../../contracts/do-messages";
import { bodyObject, optString, str } from "../../lib/body";
import { listResponse, msOf } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import { Envelopes, envelopes, type SecretIdentity } from "../../secrets/envelope";
import { API_NAME_RE, isSecretType, validateApiValue, validateName, validateValue } from "../../secrets/validate";
import { findFactory } from "./factories";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./index";

export const migrations: readonly Migration[] = [
  {
    version: 2,
    name: "secrets",
    sql: `
CREATE TABLE secrets (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'generic', key_id TEXT NOT NULL DEFAULT '', ciphertext BLOB NOT NULL,
  created_ms INTEGER NOT NULL, updated_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE UNIQUE INDEX secrets_scope_name ON secrets (factory_id, name);
CREATE TABLE secret_access (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL DEFAULT '', factory_id TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '', names TEXT NOT NULL, at_ms INTEGER NOT NULL);
CREATE INDEX secret_access_at ON secret_access (at_ms);
`,
  },
];

export interface Secret {
  id: string;
  team_id: string;
  factory_id?: string;
  name: string;
  type: string;
  created_at: string;
  updated_at: string;
}

interface SecretRow {
  id: string;
  factory_id: string;
  name: string;
  type: string;
  ciphertext: ArrayBuffer;
  data: string;
}

const ACCESS_RETENTION_MS = 30 * 24 * 3600_000;

// ---------- sealing for other modules ----------

/** Seals a value with the team's envelope key. Async: call before the write transaction. */
export async function sealValue(kit: TeamKit, id: SecretIdentity, value: string | Uint8Array): Promise<Uint8Array> {
  const e = await envelopes(kit.env);
  return e.encrypt(id, typeof value === "string" ? new TextEncoder().encode(value) : value);
}

/** Opens a value sealed by sealValue (or a secret envelope). Throws on a wrong key or tampering. */
export async function openValue(kit: TeamKit, id: SecretIdentity, envelope: Uint8Array): Promise<Uint8Array> {
  return (await envelopes(kit.env)).decrypt(id, envelope);
}

/** A set of sealed values that rewrap re-encrypts. */
export interface RewrapSource {
  name: string;
  /** Sync read of all items: key, identity and envelope. */
  list(kit: TeamKit): { key: string; id: SecretIdentity; envelope: Uint8Array }[];
  /** Sync compare-and-swap write inside a transaction. Return false when the stored value changed. */
  replace(kit: TeamKit, key: string, old: Uint8Array, next: Uint8Array): boolean;
}

const rewrapSources: RewrapSource[] = [];

/** Registers a source of sealed values (call from a module's setup). */
export function registerRewrapSource(s: RewrapSource): void {
  if (!rewrapSources.some((x) => x.name === s.name)) rewrapSources.push(s);
}

// ---------- secret records ----------

function toSecret(r: SecretRow): Secret {
  return JSON.parse(r.data) as Secret;
}

function rowOf(kit: TeamKit, factoryId: string, name: string): SecretRow | undefined {
  return first<SecretRow & Record<string, SqlStorageValue>>(
    kit.sql,
    "SELECT id, factory_id, name, type, ciphertext, data FROM secrets WHERE factory_id = ? AND name = ?",
    factoryId,
    name,
  );
}

const identityOf = (kit: TeamKit, factoryId: string, name: string, type: string): SecretIdentity => ({
  teamId: kit.teamId(),
  factoryId,
  name,
  type,
});

/** Lists the metadata of one scope. */
export function listSecrets(kit: TeamKit, factoryId: string): Secret[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM secrets WHERE factory_id = ? ORDER BY name", factoryId).map(
    (r) => JSON.parse(r.data) as Secret,
  );
}

/** Get: the factory secret, else the team secret (core.SecretStore.Get). Undefined when missing. */
export async function getSecretValue(kit: TeamKit, factoryId: string, name: string): Promise<string | undefined> {
  const r = (factoryId ? rowOf(kit, factoryId, name) : undefined) ?? rowOf(kit, "", name);
  if (!r) return undefined;
  try {
    const v = await openValue(kit, identityOf(kit, r.factory_id, r.name, r.type), new Uint8Array(r.ciphertext));
    return new TextDecoder().decode(v);
  } catch (e) {
    throw new OfError("internal_error", `secret ${name}: ${(e as Error).message}`);
  }
}

/** TypeOf: type of the secret that Get resolves, or "". */
export function typeOf(kit: TeamKit, factoryId: string, name: string): string {
  const r = (factoryId ? rowOf(kit, factoryId, name) : undefined) ?? rowOf(kit, "", name);
  return r?.type ?? "";
}

/** Put: validates, seals and stores; an existing name in the scope keeps its ID. */
export async function putSecret(kit: TeamKit, factoryId: string, name: string, type: string, value: string): Promise<Secret> {
  const typ = type || "generic";
  validateName(name);
  validateValue(name, typ, value);
  const ct = await sealValue(kit, identityOf(kit, factoryId, name, typ), value);
  const keyId = Envelopes.versionOf(ct);
  return kit.tx(() => {
    const now = new Date().toISOString();
    const cur = rowOf(kit, factoryId, name);
    const old = cur ? toSecret(cur) : undefined;
    const sec: Secret = {
      id: old?.id ?? newId("sec"),
      team_id: kit.teamId(),
      ...(factoryId ? { factory_id: factoryId } : {}),
      name,
      type: typ,
      created_at: old?.created_at ?? now,
      updated_at: now,
    };
    kit.sql.exec(
      `INSERT INTO secrets (id, factory_id, name, type, key_id, ciphertext, created_ms, updated_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(factory_id, name) DO UPDATE SET type = excluded.type, key_id = excluded.key_id, ciphertext = excluded.ciphertext,
         updated_ms = excluded.updated_ms, data = excluded.data`,
      sec.id,
      factoryId,
      name,
      typ,
      keyId,
      ct,
      msOf(sec.created_at),
      msOf(now),
      JSON.stringify(sec),
    );
    return sec;
  });
}

/** Values: name -> value for a run; missing names reported together. Writes an access row. */
export async function resolve(
  kit: TeamKit,
  meta: CallMeta,
  input: { factoryId: string; runId?: string; names: string[] },
): Promise<Record<string, string>> {
  const out: Record<string, string> = {};
  const missing: string[] = [];
  for (const n of input.names) {
    const v = await getSecretValue(kit, input.factoryId, n);
    if (v === undefined) missing.push(n);
    else out[n] = v;
  }
  if (missing.length) throw new OfError("resource_not_found", `unknown secret(s): ${missing.join(", ")}`);
  kit.tx(() => {
    const now = Date.now();
    kit.sql.exec(
      "INSERT INTO secret_access (run_id, factory_id, actor, names, at_ms) VALUES (?, ?, ?, ?, ?)",
      input.runId ?? "",
      input.factoryId,
      meta.principal?.id ?? "",
      JSON.stringify(input.names),
      now,
    );
    kit.sql.exec("DELETE FROM secret_access WHERE at_ms < ?", now - ACCESS_RETENTION_MS);
  });
  return out;
}

/** Rewrap every secret and every registered source. Each write is a compare-and-swap. */
export async function rewrapAll(kit: TeamKit, _meta: CallMeta): Promise<{ rewrapped: number; skipped: number; key_id: string; failed?: number; errors?: string[] }> {
  const env = await envelopes(kit.env);
  let rewrapped = 0;
  let skipped = 0;
  const errors: string[] = [];
  const sources: RewrapSource[] = [secretSource, ...rewrapSources];
  for (const src of sources) {
    for (const it of src.list(kit)) {
      try {
        const { out, changed } = await env.rewrap(it.id, it.envelope);
        if (!changed) {
          skipped++;
          continue;
        }
        if (kit.tx(() => src.replace(kit, it.key, it.envelope, out))) rewrapped++;
      } catch (e) {
        errors.push(`${src.name} ${it.key}: ${(e as Error).message}`);
      }
    }
  }
  const res: { rewrapped: number; skipped: number; key_id: string; failed?: number; errors?: string[] } = {
    rewrapped,
    skipped,
    key_id: env.currentVersion(),
  };
  if (errors.length) {
    res.failed = errors.length;
    res.errors = errors;
  }
  return res;
}

const eqBytes = (a: Uint8Array, b: Uint8Array) => a.length === b.length && a.every((x, i) => x === b[i]);

const secretSource: RewrapSource = {
  name: "secret",
  list: (kit) =>
    all<SecretRow & Record<string, SqlStorageValue>>(kit.sql, "SELECT id, factory_id, name, type, ciphertext, data FROM secrets").map(
      (r) => ({ key: r.id, id: identityOf(kit, r.factory_id, r.name, r.type), envelope: new Uint8Array(r.ciphertext) }),
    ),
  replace: (kit, key, old, next) => {
    const r = first<{ ciphertext: ArrayBuffer }>(kit.sql, "SELECT ciphertext FROM secrets WHERE id = ?", key);
    if (!r || !eqBytes(new Uint8Array(r.ciphertext), old)) return false;
    kit.sql.exec("UPDATE secrets SET ciphertext = ?, key_id = ? WHERE id = ?", next, Envelopes.versionOf(next), key);
    return true;
  },
};

// ---------- REST ----------

/** secretScope: body factory_id, else ?factory_id; the factory must exist. */
function scopeOf(kit: TeamKit, c: OpCall, bodyFactory = ""): string {
  const fid = bodyFactory || c.query["factory_id"] || "";
  if (!fid) return "";
  const f = findFactory(kit, fid);
  if (!f) throw new OfError("resource_not_found", `factory ${fid} not found`);
  return f.id;
}

function findSecret(kit: TeamKit, factoryId: string, name: string): Secret {
  const r = rowOf(kit, factoryId, name);
  if (!r) throw new OfError("resource_not_found", `secret ${name} not found`);
  return toSecret(r);
}

export const ops: Record<string, TeamOp> = {
  listSecrets: (kit, c) => listResponse(listSecrets(kit, scopeOf(kit, c)), ""),

  createSecret: async (kit, c) => {
    const b = bodyObject(c.body);
    const name = str(b, "name");
    if (!API_NAME_RE.test(name)) {
      throw new OfError("invalid_request", "name must start with a letter or '_' and contain only letters, digits and '_' (max 128)");
    }
    const type = str(b, "type") || "generic";
    if (!isSecretType(type)) throw new OfError("invalid_request", `unknown secret type ${JSON.stringify(type)}`);
    const value = str(b, "value");
    validateApiValue(name, value);
    const fid = scopeOf(kit, c, str(b, "factory_id"));
    if (rowOf(kit, fid, name)) throw new OfError("conflict", `secret ${JSON.stringify(name)} already exists; use PUT to update its value`);
    return created(await putSecret(kit, fid, name, type, value));
  },

  getSecret: (kit, c) => findSecret(kit, scopeOf(kit, c), c.params["name"]!),

  updateSecret: async (kit, c) => {
    const b = bodyObject(c.body);
    const name = c.params["name"]!;
    const fid = scopeOf(kit, c, str(b, "factory_id"));
    const cur = findSecret(kit, fid, name);
    const value = str(b, "value");
    validateApiValue(name, value);
    let type = cur.type;
    const t = optString(b, "type");
    if (t) {
      if (!isSecretType(t)) throw new OfError("invalid_request", `unknown secret type ${JSON.stringify(t)}`);
      type = t;
    }
    return putSecret(kit, fid, name, type, value);
  },

  deleteSecret: (kit, c) =>
    kit.tx(() => {
      const fid = scopeOf(kit, c);
      const sec = findSecret(kit, fid, c.params["name"]!);
      kit.sql.exec("DELETE FROM secrets WHERE id = ?", sec.id);
      return noContent();
    }),

  rewrapSecrets: (kit, c) => rewrapAll(kit, c.meta),

  /** Internal: a decrypted secret for Worker code (webhook verification, forge app keys). */
  secretsGet: async (kit, c) => {
    const b = bodyObject(c.body);
    const v = await getSecretValue(kit, str(b, "factory_id"), str(b, "name"));
    return { found: v !== undefined, value: v ?? "" };
  },
};

export function setup(_kit: TeamKit): void {}
