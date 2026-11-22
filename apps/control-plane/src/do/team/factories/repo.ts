// Factory records and their tables: factories, definition versions (inline or
// in R2), the compiled runtime, the materialized resources and the factory
// integration settings. Row access only; the apply logic is in definitions.ts.
import { OfError } from "@openfactory/core";
import type { AgentConfig, Automation, ValidationError } from "@openfactory/definition";
import type { JsonObject } from "../../../lib/body";
import { sha256Hex } from "../../../lib/bytes";
import { msOf } from "../../../lib/page";
import type { Migration } from "../../../lib/sql/migrate";
import { all, first } from "../../../lib/sql/query";
import type { OpCall, TeamKit } from "../kit";

export const migrations: readonly Migration[] = [
  {
    version: 3,
    name: "factories",
    sql: `
CREATE TABLE factories (id TEXT PRIMARY KEY, alias TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  active_version_id TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX factories_name ON factories (name);
CREATE INDEX factories_alias ON factories (alias);
CREATE INDEX factories_created ON factories (created_ms DESC, id DESC);
CREATE TABLE definition_versions (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, state TEXT NOT NULL DEFAULT '',
  valid INTEGER NOT NULL DEFAULT 0, created_ms INTEGER NOT NULL, files TEXT, files_r2 TEXT, idem_key TEXT, data TEXT NOT NULL);
CREATE INDEX definition_versions_factory ON definition_versions (factory_id, created_ms DESC, id DESC);
CREATE UNIQUE INDEX definition_versions_idem ON definition_versions (idem_key) WHERE idem_key IS NOT NULL;
CREATE TABLE factory_runtime (factory_id TEXT PRIMARY KEY, version_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE agents (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, name TEXT NOT NULL, ord INTEGER NOT NULL, data TEXT NOT NULL);
CREATE UNIQUE INDEX agents_factory_name ON agents (factory_id, name);
CREATE TABLE automations (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0, ord INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX automations_factory ON automations (factory_id, ord);
CREATE TABLE scorers (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, ord INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX scorers_factory ON scorers (factory_id, ord);
CREATE TABLE factory_integrations (factory_id TEXT NOT NULL, provider TEXT NOT NULL, ord INTEGER NOT NULL DEFAULT 0,
  settings TEXT NOT NULL, PRIMARY KEY (factory_id, provider));
`,
  },
  {
    version: 6,
    name: "factories_alias_nocase",
    sql: `
CREATE INDEX factories_alias_nocase ON factories (alias COLLATE NOCASE);
`,
  },
];

/** Definition trees above this size go to R2 by hash (plan rule 9). */
const INLINE_FILES_LIMIT = 1 << 20;

// ---------- records ----------

export interface FactoryRecord {
  id: string;
  team_id: string;
  name: string;
  alias: string;
  description?: string;
  source: string;
  git_repo?: string;
  git_branch?: string;
  git_path?: string;
  active_version_id?: string;
  created_at: string;
  updated_at: string;
}

export interface DefinitionVersion {
  id: string;
  factory_id: string;
  files: Record<string, string>;
  commit_sha?: string;
  author?: string;
  message?: string;
  valid: boolean;
  errors?: ValidationError[];
  applied_at?: string;
  created_at: string;
  state?: string;
  base_version_id?: string;
  author_run_id?: string;
  inbox_item_id?: string;
  applied_version_id?: string;
  reviewed_by?: string;
  review_comment?: string;
  reviewed_at?: string;
}

/** FactoryRuntime of engine/definition.go. */
export interface FactoryRuntime {
  version_id: string;
  repositories: unknown[] | null;
  machines?: unknown;
  skills?: unknown[];
  secrets?: string[];
  mcp_servers?: unknown;
  credential_strategy?: string;
  self_improvement?: unknown;
  benchmarks?: unknown[];
  scorer_defaults?: unknown;
  cloud_providers?: unknown;
  github_onboarding_notices?: boolean;
  max_concurrent_runs?: number;
  approvers?: string[];
  restrict_followups_to_starter?: boolean;
  require_linked_actor?: boolean;
  allow_agent_merge?: boolean;
  spend_limit?: unknown;
}

export function getFactoryById(kit: TeamKit, id: string): FactoryRecord | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM factories WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as FactoryRecord) : undefined;
}

/** Factory by ID, alias or name, or undefined. */
export function findFactory(kit: TeamKit, ref: string): FactoryRecord | undefined {
  if (!ref) return undefined;
  const r =
    first<{ data: string }>(kit.sql, "SELECT data FROM factories WHERE id = ?", ref) ??
    first<{ data: string }>(
      kit.sql,
      "SELECT data FROM factories WHERE alias = ? COLLATE NOCASE ORDER BY created_ms LIMIT 1",
      ref,
    ) ??
    first<{ data: string }>(kit.sql, "SELECT data FROM factories WHERE name = ? ORDER BY created_ms LIMIT 1", ref);
  return r ? (JSON.parse(r.data) as FactoryRecord) : undefined;
}

export function listFactories(kit: TeamKit): FactoryRecord[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM factories").map((r) => JSON.parse(r.data) as FactoryRecord);
}

export function putFactory(kit: TeamKit, f: FactoryRecord): void {
  kit.sql.exec(
    `INSERT INTO factories (id, alias, name, active_version_id, created_ms, data) VALUES (?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET alias = excluded.alias, name = excluded.name, active_version_id = excluded.active_version_id, data = excluded.data`,
    f.id,
    f.alias,
    f.name,
    f.active_version_id ?? "",
    msOf(f.created_at) || Date.now(),
    JSON.stringify(f),
  );
  runtimeCache(kit).delete(f.id);
}

export function factoryUrl(kit: TeamKit, id: string): string {
  const base = (kit.env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  return base ? `${base}/f/${encodeURIComponent(id)}` : "";
}

/** FactoryView: {uid, ...factory, url}. */
export function factoryView(kit: TeamKit, f: FactoryRecord): JsonObject {
  const url = factoryUrl(kit, f.id);
  return { uid: f.id, ...f, ...(url ? { url } : {}) };
}

/** factoryFor: the factory by ID, alias or name; authorizes the route for it; 404 otherwise. */
export function factoryFor(kit: TeamKit, c: OpCall, ref = c.params["uid"] ?? ""): FactoryRecord {
  const f = findFactory(kit, ref);
  c.authorize(f?.id ?? "");
  if (!f) throw new OfError("resource_not_found", `factory ${ref} not found`);
  return f;
}

// ---------- versions ----------

export interface VersionRow {
  data: string;
  files: string | null;
  files_r2: string | null;
}

export async function loadFiles(kit: TeamKit, r: VersionRow): Promise<Record<string, string>> {
  if (r.files !== null) return JSON.parse(r.files) as Record<string, string>;
  if (r.files_r2) {
    const o = await kit.env.DATA.get(r.files_r2);
    if (!o) throw new OfError("resource_unavailable", `definition files ${r.files_r2} are missing`);
    return (await o.json()) as Record<string, string>;
  }
  return {};
}

export async function getVersion(kit: TeamKit, id: string, withFiles = true): Promise<DefinitionVersion | undefined> {
  const r = first<VersionRow & Record<string, SqlStorageValue>>(
    kit.sql,
    "SELECT data, files, files_r2 FROM definition_versions WHERE id = ?",
    id,
  );
  if (!r) return undefined;
  const v = JSON.parse(r.data) as DefinitionVersion;
  v.files = withFiles ? await loadFiles(kit, r) : (undefined as unknown as Record<string, string>);
  return v;
}

/** Files of the active version ({} when none). */
export async function activeFiles(kit: TeamKit, f: FactoryRecord): Promise<Record<string, string>> {
  if (!f.active_version_id) return {};
  const v = await getVersion(kit, f.active_version_id);
  return { ...(v?.files ?? {}) };
}

export function effectiveState(v: DefinitionVersion, activeId: string): string {
  if (v.state) return v.state;
  if (!v.valid) return "invalid";
  return v.id === activeId ? "active" : "superseded";
}

/** Serializes files and puts large trees in R2 (call before the write transaction). */
export async function storeFiles(
  kit: TeamKit,
  files: Record<string, string>,
): Promise<{ inline: string | null; r2: string | null }> {
  const text = JSON.stringify(files);
  if (text.length <= INLINE_FILES_LIMIT) return { inline: text, r2: null };
  const key = `defs/${await sha256Hex(text)}`;
  await kit.env.DATA.put(key, text, { httpMetadata: { contentType: "application/json" } });
  return { inline: null, r2: key };
}

export function insertVersion(
  kit: TeamKit,
  v: DefinitionVersion,
  stored: { inline: string | null; r2: string | null },
  idemKey?: string,
): void {
  const { files: _files, ...meta } = v;
  kit.sql.exec(
    `INSERT INTO definition_versions (id, factory_id, state, valid, created_ms, files, files_r2, idem_key, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET state = excluded.state, valid = excluded.valid, data = excluded.data`,
    v.id,
    v.factory_id,
    v.state ?? "",
    v.valid ? 1 : 0,
    msOf(v.created_at) || Date.now(),
    stored.inline,
    stored.r2,
    idemKey ?? null,
    JSON.stringify(meta),
  );
}

/** The definition file of an agent, automation, scorer or webhook (paths in preflight messages). */
export const agentFile = (n: string) => `agents/${n}/agent.md`;
export const automationFile = (n: string) => `automations/${n}/automation.md`;
export const scorerFile = (n: string) => `scorers/${n}/scorer.md`;
export const webhookFile = (n: string) => `webhooks/${n}.yaml`;

export function setFactoryIntegrations(
  kit: TeamKit,
  factoryId: string,
  providers: string[],
  settings: Record<string, Record<string, string> | undefined>,
): void {
  runtimeCache(kit).delete(factoryId);
  kit.sql.exec("DELETE FROM factory_integrations WHERE factory_id = ?", factoryId);
  providers.forEach((p, i) =>
    kit.sql.exec(
      "INSERT INTO factory_integrations (factory_id, provider, ord, settings) VALUES (?, ?, ?, ?)",
      factoryId,
      p,
      i,
      JSON.stringify(settings[p] ?? {}),
    ),
  );
}

export function getFactoryIntegrations(kit: TeamKit, factoryId: string): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  for (const r of all<{ provider: string; settings: string }>(
    kit.sql,
    "SELECT provider, settings FROM factory_integrations WHERE factory_id = ? ORDER BY ord",
    factoryId,
  )) {
    out[r.provider] = JSON.parse(r.settings) as Record<string, string>;
  }
  return out;
}

/**
 * configSnapshot results by factory ID, per TeamDO instance. The key is the
 * kit (one per instance), so nothing leaks across teams or test instances
 * that share the module.
 */
const runtimeCaches = new WeakMap<TeamKit, Map<string, { versionId: string; snapshot: unknown }>>();

export function runtimeCache(kit: TeamKit): Map<string, { versionId: string; snapshot: unknown }> {
  let m = runtimeCaches.get(kit);
  if (!m) {
    m = new Map();
    runtimeCaches.set(kit, m);
  }
  return m;
}

// ---------- resources ----------

/** spawnable_by is [] (not null) in API output (api/openapi.json). */
export function agentOut(a: AgentConfig): AgentConfig {
  return { ...a, spawnable_by: a.spawnable_by ?? [] };
}

export function listAgents(kit: TeamKit, factoryId: string): AgentConfig[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? ORDER BY ord", factoryId).map(
    (r) => agentOut(JSON.parse(r.data) as AgentConfig),
  );
}

/** The compiled runtime of the active version, or undefined when none is applied. */
export function activeRuntime(kit: TeamKit, factoryId: string): FactoryRuntime | undefined {
  const rt = first<{ data: string }>(kit.sql, "SELECT data FROM factory_runtime WHERE factory_id = ?", factoryId);
  return rt ? (JSON.parse(rt.data) as FactoryRuntime) : undefined;
}

/** Machine specs of the active runtime, by name (listMachines). */
export function activeMachines(kit: TeamKit, factoryId: string): Record<string, Record<string, unknown>> | undefined {
  return activeRuntime(kit, factoryId)?.machines as Record<string, Record<string, unknown>> | undefined;
}

export function listAutomations(kit: TeamKit, factoryId: string): Automation[] {
  return all<{ data: string }>(
    kit.sql,
    "SELECT data FROM automations WHERE factory_id = ? ORDER BY ord",
    factoryId,
  ).map((r) => JSON.parse(r.data) as Automation);
}

const ALIAS_RE = /^[A-Za-z0-9 ._-]{1,60}$/;
export const validAlias = (s: string) => ALIAS_RE.test(s) && s.trim() !== "";

/** AliasFromName of internal/api/defaults.go. */
export function aliasFromName(name: string): string {
  let b = "";
  for (const ch of name.trim().toLowerCase()) {
    if (/[a-z0-9._-]/.test(ch)) b += ch;
    else if (ch === " ") b += "-";
  }
  let a = b.replace(/^-+|-+$/g, "");
  if (a.length > 60) a = a.slice(0, 60);
  return a || "factory";
}

export function checkAliasFree(kit: TeamKit, alias: string, exceptId: string): void {
  const r = first<{ data: string }>(
    kit.sql,
    "SELECT data FROM factories WHERE alias = ? COLLATE NOCASE AND id != ? ORDER BY rowid LIMIT 1",
    alias,
    exceptId,
  );
  const other = r ? (JSON.parse(r.data) as FactoryRecord) : undefined;
  if (other)
    throw new OfError(
      "conflict",
      `alias ${JSON.stringify(alias)} is already used by factory ${JSON.stringify(other.name)}`,
    );
}

export function deleteFactoryRows(kit: TeamKit, id: string): void {
  for (const t of [
    "agents",
    "automations",
    "scorers",
    "factory_integrations",
    "factory_runtime",
    "definition_versions",
  ]) {
    kit.sql.exec(`DELETE FROM ${t} WHERE factory_id = ?`, id);
  }
  kit.sql.exec("DELETE FROM factories WHERE id = ?", id);
  runtimeCache(kit).delete(id);
}
