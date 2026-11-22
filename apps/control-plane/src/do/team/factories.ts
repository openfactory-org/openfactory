// TeamDO module: factories (WS4). Factories, definition versions, the
// compiled runtime and the materialized resources (agents, automations,
// scorers, factory integrations). Ports internal/api/factories.go,
// internal/api/dashboard.go (settings, integrations, restore, agent spec,
// automation files) and engine.ApplyDefinition + the preflight checks this
// module owns (alias, secrets, environments). Other modules add checks with
// registerPreflight.
//
// Definition apply (plan rule 2): compile and preflight run before the write;
// network work (name resolution, skill fetches) happens in the Worker before
// the call; one transactionSync writes the version, resources, runtime,
// integrations, factory record, schedules, webhooks and the limits outbox row.
import { OfError, newId } from "@openfactory/core";
import {
  compile,
  defaultDefinition,
  DEFAULT_AGENT_NAMES,
  keyLine,
  type AgentConfig,
  type Automation,
  type CompiledDefinition,
  type Scorer,
  type ValidationError,
} from "@openfactory/definition";
import { Document, isMap, isScalar, isSeq, parseDocument, YAMLMap, YAMLSeq, Scalar } from "yaml";
import type { CallMeta } from "../../contracts/do-messages";
import { actorId, roleRank } from "../../auth/roles";
import { bodyObject, optString, optStringArray, str, type JsonObject } from "../../lib/body";
import { sha256Hex } from "../../lib/bytes";
import { dest } from "../../lib/outbox/dest";
import { listResponse, msOf, pageFrom, paginate, sqlPage } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import * as schedules from "./schedules";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./index";

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

function putFactory(kit: TeamKit, f: FactoryRecord): void {
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

interface VersionRow {
  data: string;
  files: string | null;
  files_r2: string | null;
}

async function loadFiles(kit: TeamKit, r: VersionRow): Promise<Record<string, string>> {
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

function effectiveState(v: DefinitionVersion, activeId: string): string {
  if (v.state) return v.state;
  if (!v.valid) return "invalid";
  return v.id === activeId ? "active" : "superseded";
}

/** Serializes files and puts large trees in R2 (call before the write transaction). */
async function storeFiles(
  kit: TeamKit,
  files: Record<string, string>,
): Promise<{ inline: string | null; r2: string | null }> {
  const text = JSON.stringify(files);
  if (text.length <= INLINE_FILES_LIMIT) return { inline: text, r2: null };
  const key = `defs/${await sha256Hex(text)}`;
  await kit.env.DATA.put(key, text, { httpMetadata: { contentType: "application/json" } });
  return { inline: null, r2: key };
}

function insertVersion(
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

// ---------- preflight ----------

export interface SkippedCheck {
  check: string;
  reason: string;
}

export interface DefinitionCheck {
  definition: CompiledDefinition | null;
  errors: ValidationError[];
  warnings: ValidationError[];
  skipped: SkippedCheck[];
}

/** A preflight check of another module (workers, MCP servers, webhooks...). Sync, no I/O. */
export type PreflightCheck = (
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  files: Record<string, string>,
  chk: DefinitionCheck,
) => void;

const preflights: { name: string; fn: PreflightCheck }[] = [];

/** Registers a preflight check (call from a module's setup). */
export function registerPreflight(name: string, fn: PreflightCheck): void {
  if (!preflights.some((p) => p.name === name)) preflights.push({ name, fn });
}

const agentFile = (n: string) => `agents/${n}/agent.md`;
const automationFile = (n: string) => `automations/${n}/automation.md`;
const scorerFile = (n: string) => `scorers/${n}/scorer.md`;
const webhookFile = (n: string) => `webhooks/${n}.yaml`;

interface DefRef {
  file: string;
  field: string;
  name: string;
}

function secretRefs(def: CompiledDefinition): DefRef[] {
  const refs: DefRef[] = [];
  const add = (file: string, field: string, name: string | undefined) => {
    if (name) refs.push({ file, field, name });
  };
  const auth = (file: string, field: string, a: { source: string; secretName?: string } | undefined) => {
    if (a && a.source === "managedSecret") add(file, field + ".auth.secretName", a.secretName);
  };
  for (const s of def.secrets ?? []) add("factory.yaml", "secrets", s);
  for (const a of def.agents ?? []) {
    for (const s of a.secrets ?? []) add(agentFile(a.name), "secrets", s);
    auth(agentFile(a.name), "harness", a.harness?.auth);
  }
  for (const au of def.automations ?? []) {
    const o = au.overrides as
      { secrets?: string[]; harness?: { auth?: { source: string; secretName?: string } } } | undefined;
    if (!o) continue;
    for (const s of o.secrets ?? []) add(automationFile(au.name), "secrets", s);
    auth(automationFile(au.name), "harness", o.harness?.auth);
  }
  const sd = def.scorerDefaults as
    { secrets?: string[]; harness?: { auth?: { source: string; secretName?: string } } } | undefined;
  if (sd) {
    for (const s of sd.secrets ?? []) add("factory.yaml", "scorerDefaults.secrets", s);
    auth("factory.yaml", "scorerDefaults.harness", sd.harness?.auth);
  }
  for (const sc of def.scorers ?? []) for (const s of sc.secrets ?? []) add(scorerFile(sc.slug), "secrets", s);
  for (const w of def.webhooks ?? []) add(webhookFile(w.name), "secretName", w.secret_name);
  return refs;
}

function preflightSecrets(
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  _files: Record<string, string>,
  chk: DefinitionCheck,
): void {
  const refs = secretRefs(def);
  if (refs.length === 0) return;
  const known = new Set(
    all<{ name: string }>(kit.sql, "SELECT name FROM secrets WHERE factory_id = '' OR factory_id = ?", f.id).map(
      (r) => r.name,
    ),
  );
  const seen = new Set<string>();
  for (const r of refs) {
    if (known.has(r.name) || seen.has(r.name)) continue;
    seen.add(r.name);
    chk.errors.push({
      file: r.file,
      field: r.field,
      message: `secret ${JSON.stringify(r.name)} does not exist in the team or the factory; add it under Secrets first`,
    });
  }
}

function preflightEnvironments(
  kit: TeamKit,
  _f: FactoryRecord,
  def: CompiledDefinition,
  files: Record<string, string>,
  chk: DefinitionCheck,
): void {
  const refs: DefRef[] = [];
  const envField = "environmentId";
  const defaultsSet = keyLine("factory.yaml", files["factory.yaml"] ?? "", "agentDefaults." + envField) > 0;
  for (const a of def.agents ?? []) {
    if (!a.environment_id) continue;
    let file = agentFile(a.name);
    let field = envField;
    if (defaultsSet && keyLine(file, files[file] ?? "", envField) === 0) {
      file = "factory.yaml";
      field = "agentDefaults." + envField;
    }
    refs.push({ file, field, name: a.environment_id });
  }
  for (const au of def.automations ?? []) {
    const e = au.overrides as { environmentId?: string | null; environment_id?: string | null } | undefined;
    const id = e?.environmentId ?? e?.environment_id;
    if (id) refs.push({ file: automationFile(au.name), field: envField, name: id });
  }
  if (refs.length === 0) return;
  let known: Set<string>;
  try {
    known = new Set(all<{ id: string }>(kit.sql, "SELECT id FROM environments").map((r) => r.id));
  } catch (e) {
    chk.skipped.push({ check: "environments", reason: "could not list the environments: " + (e as Error).message });
    return;
  }
  const seen = new Set<string>();
  for (const r of refs) {
    const key = r.file + "\0" + r.name;
    if (known.has(r.name) || seen.has(key)) continue;
    seen.add(key);
    chk.errors.push({
      file: r.file,
      field: r.field,
      message: `environment ${JSON.stringify(r.name)} does not exist in the team`,
    });
  }
}

function checkAlias(kit: TeamKit, f: FactoryRecord, def: CompiledDefinition): ValidationError[] {
  if (!def.alias) return [];
  const other = all<{ data: string }>(kit.sql, "SELECT data FROM factories WHERE alias = ? COLLATE NOCASE", def.alias)
    .map((r) => JSON.parse(r.data) as FactoryRecord)
    .find((o) => o.id !== f.id);
  if (!other) return [];
  return [
    {
      file: "factory.yaml",
      field: "alias",
      message: `alias ${JSON.stringify(def.alias)} is already used by factory ${JSON.stringify(other.name)}`,
    },
  ];
}

function sortIssues(v: ValidationError[]): void {
  v.sort((a, b) =>
    a.file !== b.file
      ? a.file < b.file
        ? -1
        : 1
      : (a.field ?? "") < (b.field ?? "")
        ? -1
        : (a.field ?? "") > (b.field ?? "")
          ? 1
          : 0,
  );
}

function addIssueLines(files: Record<string, string>, issues: ValidationError[]): void {
  for (const is of issues) {
    if ((is.line ?? 0) !== 0 || !is.field) continue;
    const content = files[is.file];
    if (content !== undefined) {
      const l = keyLine(is.file, content, is.field);
      if (l > 0) is.line = l;
    }
  }
}

/** checkDefinition: compile, alias check, preflight. */
export function checkDefinition(kit: TeamKit, f: FactoryRecord, files: Record<string, string>): DefinitionCheck {
  const chk: DefinitionCheck = { definition: null, errors: [], warnings: [], skipped: [] };
  const res = compile(files);
  let verrs = res.errors;
  if (!res.definition && verrs.length === 0)
    verrs = [{ file: "factory.yaml", message: "definition could not be compiled" }];
  if (verrs.length > 0) {
    chk.errors.push(...verrs);
    return chk;
  }
  const def = res.definition!;
  chk.definition = def;
  chk.errors.push(...checkAlias(kit, f, def));
  preflightSecrets(kit, f, def, files, chk);
  preflightEnvironments(kit, f, def, files, chk);
  for (const p of preflights) {
    try {
      p.fn(kit, f, def, files, chk);
    } catch (e) {
      chk.skipped.push({ check: p.name, reason: (e as Error).message });
    }
  }
  sortIssues(chk.errors);
  sortIssues(chk.warnings);
  addIssueLines(files, chk.errors);
  addIssueLines(files, chk.warnings);
  return chk;
}

function validationError(detail: string, errs: ValidationError[] | undefined): OfError {
  return new OfError("invalid_request", detail, { extra: { errors: errs ?? [] } });
}

// ---------- materialize and apply ----------

/** StableID: prefix_ + hex(sha256(factory \0 prefix \0 name))[:20]. */
export async function stableId(prefix: string, factoryId: string, name: string): Promise<string> {
  return `${prefix}_${(await sha256Hex(`${factoryId}\0${prefix}\0${name}`)).slice(0, 20)}`;
}

interface Materialized {
  agents: AgentConfig[];
  automations: Automation[];
  scorers: Scorer[];
}

async function materialize(factoryId: string, def: CompiledDefinition): Promise<Materialized> {
  let foreman = "";
  const agents: AgentConfig[] = [];
  for (const a0 of def.agents ?? []) {
    const a: AgentConfig = structuredClone(a0);
    a.id = await stableId("agt", factoryId, a.name);
    a.factory_id = factoryId;
    if (!a.credential_strategy && def.credentialStrategy) a.credential_strategy = def.credentialStrategy;
    if (a.type === "FOREMAN") foreman = a.name;
    agents.push(a);
  }
  const automations: Automation[] = [];
  for (const au0 of def.automations ?? []) {
    const au: Automation = structuredClone(au0);
    au.id = await stableId("aut", factoryId, au.name);
    au.factory_id = factoryId;
    if (!au.agent) au.agent = foreman;
    automations.push(au);
  }
  const scorers: Scorer[] = [];
  for (const s0 of def.scorers ?? []) {
    const s: Scorer = structuredClone(s0);
    s.id = await stableId("scr", factoryId, s.slug || s.name);
    s.factory_id = factoryId;
    scorers.push(s);
  }
  return { agents, automations, scorers };
}

function runtimeOf(versionId: string, def: CompiledDefinition): FactoryRuntime {
  const rt: FactoryRuntime = { version_id: versionId, repositories: def.repositories ?? null };
  const set = <K extends keyof FactoryRuntime>(k: K, v: FactoryRuntime[K] | undefined | null) => {
    if (v !== undefined && v !== null && !(Array.isArray(v) && v.length === 0) && v !== "" && v !== 0 && v !== false)
      rt[k] = v;
  };
  set("machines", def.machines && Object.keys(def.machines).length ? def.machines : undefined);
  set("skills", def.skills ?? undefined);
  set("secrets", def.secrets);
  set("mcp_servers", def.mcpServers && Object.keys(def.mcpServers).length ? def.mcpServers : undefined);
  set("credential_strategy", def.credentialStrategy);
  set("self_improvement", def.selfImprovement);
  set("benchmarks", def.benchmarks ?? undefined);
  set("scorer_defaults", def.scorerDefaults);
  set("cloud_providers", def.cloudProviders);
  rt.github_onboarding_notices = def.githubOnboardingNotices;
  set("max_concurrent_runs", def.maxConcurrentRuns);
  set("require_linked_actor", (def as { requireLinkedActor?: boolean }).requireLinkedActor);
  set("allow_agent_merge", def.allowAgentMerge);
  set("spend_limit", def.spendLimit);
  set("approvers", def.approvers);
  set("restrict_followups_to_starter", def.restrictFollowupsToStarter);
  return rt;
}

function defaultAlias(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function replaceResources(kit: TeamKit, factoryId: string, m: Materialized): void {
  kit.sql.exec("DELETE FROM agents WHERE factory_id = ?", factoryId);
  kit.sql.exec("DELETE FROM automations WHERE factory_id = ?", factoryId);
  kit.sql.exec("DELETE FROM scorers WHERE factory_id = ?", factoryId);
  m.agents.forEach((a, i) =>
    kit.sql.exec(
      "INSERT INTO agents (id, factory_id, name, ord, data) VALUES (?, ?, ?, ?, ?)",
      a.id,
      factoryId,
      a.name,
      i,
      JSON.stringify(a),
    ),
  );
  m.automations.forEach((a, i) =>
    kit.sql.exec(
      "INSERT INTO automations (id, factory_id, name, enabled, ord, data) VALUES (?, ?, ?, ?, ?, ?)",
      a.id,
      factoryId,
      a.name,
      a.enabled ? 1 : 0,
      i,
      JSON.stringify(a),
    ),
  );
  m.scorers.forEach((s, i) =>
    kit.sql.exec(
      "INSERT INTO scorers (id, factory_id, ord, data) VALUES (?, ?, ?, ?)",
      s.id,
      factoryId,
      i,
      JSON.stringify(s),
    ),
  );
}

function setFactoryIntegrations(
  kit: TeamKit,
  factoryId: string,
  providers: string[],
  settings: Record<string, Record<string, string> | undefined>,
): void {
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

export interface ApplyInput {
  files: Record<string, string>;
  author?: string;
  message?: string;
  commitSha?: string;
}

export interface ApplyResult {
  version: DefinitionVersion;
  /** Set when the definition is invalid: the stored invalid version has its errors. */
  invalid?: boolean;
}

const runtimeCache = new Map<string, { versionId: string; snapshot: unknown }>();

/**
 * ApplyDefinition. Stores an invalid version with its errors (the active
 * version stays) and returns invalid: true; else writes everything in one
 * transaction and returns the applied version.
 */
export async function applyDefinition(kit: TeamKit, factoryId: string, input: ApplyInput): Promise<ApplyResult> {
  const f0 = getFactoryById(kit, factoryId);
  if (!f0) throw new OfError("resource_not_found", `factory ${factoryId} not found`);
  const now = new Date().toISOString();
  const v: DefinitionVersion = {
    id: newId("defv"),
    factory_id: factoryId,
    files: input.files,
    ...(input.commitSha ? { commit_sha: input.commitSha } : {}),
    ...(input.author ? { author: input.author } : {}),
    ...(input.message ? { message: input.message } : {}),
    valid: false,
    created_at: now,
  };
  const stored = await storeFiles(kit, input.files);
  const chk = checkDefinition(kit, f0, input.files);
  for (const w of chk.warnings) console.warn("definition preflight warning", factoryId, w.file, w.field, w.message);
  if (chk.errors.length > 0 || !chk.definition) {
    v.errors = chk.errors;
    kit.tx(() => insertVersion(kit, v, stored));
    return { version: v, invalid: true };
  }
  const def = chk.definition;
  const m = await materialize(factoryId, def);
  return kit.tx(() => {
    const f = getFactoryById(kit, factoryId);
    if (!f) throw new OfError("resource_not_found", `factory ${factoryId} not found`);
    const nf: FactoryRecord = { ...f };
    if (def.name) nf.name = def.name;
    if (def.alias) nf.alias = def.alias;
    else if (!nf.alias) nf.alias = defaultAlias(nf.name);
    if (def.description) nf.description = def.description;
    else delete nf.description;
    nf.active_version_id = v.id;
    nf.updated_at = now;
    v.valid = true;
    v.applied_at = now;
    insertVersion(kit, v, stored);
    replaceResources(kit, factoryId, m);
    const rt = runtimeOf(v.id, def);
    kit.sql.exec(
      "INSERT INTO factory_runtime (factory_id, version_id, data) VALUES (?, ?, ?) ON CONFLICT(factory_id) DO UPDATE SET version_id = excluded.version_id, data = excluded.data",
      factoryId,
      v.id,
      JSON.stringify(rt),
    );
    if (def.integrationsSet) {
      const decls = def.integrations ?? [];
      setFactoryIntegrations(
        kit,
        factoryId,
        decls.map((d) => d.type),
        Object.fromEntries(decls.map((d) => [d.type, d.settings])),
      );
    }
    putFactory(kit, nf);
    // Other modules' tables, in the same transaction (no I/O).
    const s = schedules as unknown as {
      syncSchedules?: (kit: TeamKit, factoryId: string, automations: unknown[]) => void;
      syncWebhooks?: (kit: TeamKit, factoryId: string, webhooks: unknown[]) => void;
    };
    s.syncSchedules?.(kit, factoryId, m.automations);
    s.syncWebhooks?.(kit, factoryId, def.webhooks ?? []);
    kit.enqueue({
      dest: dest.dispatch(kit.teamId()),
      kind: "dispatch.limits",
      lane: "dispatch",
      msgId: `limits:${factoryId}:${v.id}`,
      payload: {
        factoryId,
        versionId: v.id,
        maxConcurrentRuns: def.maxConcurrentRuns ?? null,
        spendLimit: def.spendLimit ?? null,
      },
    });
    runtimeCache.delete(factoryId);
    kit.live.broadcast(
      "definitions",
      { type: "event", id: "", event: "definition", data: factoryView(kit, nf) },
      (a) => {
        const fid = (a as { factoryId?: string } | null)?.factoryId ?? "";
        return fid === "" || fid === factoryId;
      },
    );
    return { version: v };
  });
}

/** TeamRpc.definitionApply: input {files, message?, author?, commitSha?, baseVersionId?}. Throws a validation error when invalid. */
export async function definitionApply(
  kit: TeamKit,
  meta: CallMeta,
  factoryId: string,
  input: unknown,
): Promise<unknown> {
  const b = bodyObject(input);
  const files = b["files"] as Record<string, string> | undefined;
  if (!files || typeof files !== "object" || Object.keys(files).length === 0)
    throw new OfError("invalid_request", "files is required");
  const base = str(b, "baseVersionId");
  const f = getFactoryById(kit, factoryId);
  if (!f) throw new OfError("resource_not_found", `factory ${factoryId} not found`);
  if (base && base !== (f.active_version_id ?? "")) {
    throw new OfError(
      "conflict",
      `the definition changed since version ${base} (active version is ${f.active_version_id ?? ""})`,
    );
  }
  const r = await applyDefinition(kit, factoryId, {
    files,
    author: str(b, "author") || actorId(meta.principal),
    message: str(b, "message") || "Update definition",
    commitSha: str(b, "commitSha"),
  });
  if (r.invalid)
    throw validationError("definition could not be applied; the previous version stays active", r.version.errors);
  return { version: r.version };
}

/** TeamRpc.configSnapshot: the read model of the active version, cached by version ID. */
export async function configSnapshot(kit: TeamKit, factoryId: string): Promise<unknown> {
  const f = findFactory(kit, factoryId);
  if (!f) throw new OfError("resource_not_found", `factory ${factoryId} not found`);
  const vid = f.active_version_id ?? "";
  const hit = runtimeCache.get(f.id);
  if (hit && hit.versionId === vid) return hit.snapshot;
  const rt = first<{ data: string }>(kit.sql, "SELECT data FROM factory_runtime WHERE factory_id = ?", f.id);
  const snapshot = {
    factory: f,
    version_id: vid,
    runtime: rt ? (JSON.parse(rt.data) as FactoryRuntime) : { version_id: "", repositories: null },
    // Raw configs: spawnable_by null (foreman only) differs from [] (nobody); agentOut is for API output.
    agents: all<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? ORDER BY ord", f.id).map(
      (r) => JSON.parse(r.data) as AgentConfig,
    ),
    automations: listAutomations(kit, f.id),
    scorers: all<{ data: string }>(kit.sql, "SELECT data FROM scorers WHERE factory_id = ? ORDER BY ord", f.id).map(
      (r) => JSON.parse(r.data) as Scorer,
    ),
    integrations: getFactoryIntegrations(kit, f.id),
  };
  runtimeCache.set(f.id, { versionId: vid, snapshot });
  return snapshot;
}

/** TeamRpc.proposeDefinition (team.proposeDefinition): a proposed version, idempotent on the tool key. */
export async function proposeDefinition(
  kit: TeamKit,
  _meta: CallMeta,
  input: {
    factoryId: string;
    files: Record<string, string>;
    message: string;
    authorRunId: string;
    idempotencyKey: string;
  },
): Promise<unknown> {
  const f = findFactory(kit, input.factoryId);
  if (!f) throw new OfError("resource_not_found", `factory ${input.factoryId} not found`);
  const prev = first<{ id: string }>(
    kit.sql,
    "SELECT id FROM definition_versions WHERE idem_key = ?",
    input.idempotencyKey,
  );
  if (prev) return { version: await getVersion(kit, prev.id, false) };
  const chk = checkDefinition(kit, f, input.files);
  const stored = await storeFiles(kit, input.files);
  return kit.tx(() => {
    const again = first<{ id: string }>(
      kit.sql,
      "SELECT id FROM definition_versions WHERE idem_key = ?",
      input.idempotencyKey,
    );
    if (again) return { duplicate: again.id };
    const v: DefinitionVersion = {
      id: newId("defv"),
      factory_id: f.id,
      files: input.files,
      author: "run:" + input.authorRunId,
      message: input.message,
      valid: chk.errors.length === 0,
      ...(chk.errors.length ? { errors: chk.errors } : {}),
      created_at: new Date().toISOString(),
      state: "proposed",
      ...(f.active_version_id ? { base_version_id: f.active_version_id } : {}),
      author_run_id: input.authorRunId,
    };
    insertVersion(kit, v, stored, input.idempotencyKey);
    const { files: _f, ...meta } = v;
    return { version: meta };
  });
}

// ---------- resources ----------

/** spawnable_by is [] (not null) in API output (api/openapi.json). */
function agentOut(a: AgentConfig): AgentConfig {
  return { ...a, spawnable_by: a.spawnable_by ?? [] };
}

export function listAgents(kit: TeamKit, factoryId: string): AgentConfig[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? ORDER BY ord", factoryId).map(
    (r) => agentOut(JSON.parse(r.data) as AgentConfig),
  );
}

/** Machine specs of the active runtime, by name (WS6 listMachines). */
export function activeMachines(kit: TeamKit, factoryId: string): Record<string, Record<string, unknown>> | undefined {
  const rt = first<{ data: string }>(kit.sql, "SELECT data FROM factory_runtime WHERE factory_id = ?", factoryId);
  if (!rt) return undefined;
  return (
    ((JSON.parse(rt.data) as FactoryRuntime).machines as Record<string, Record<string, unknown>> | undefined) ??
    undefined
  );
}

export function listAutomations(kit: TeamKit, factoryId: string): Automation[] {
  return all<{ data: string }>(
    kit.sql,
    "SELECT data FROM automations WHERE factory_id = ? ORDER BY ord",
    factoryId,
  ).map((r) => JSON.parse(r.data) as Automation);
}

// ---------- YAML editing (yaml Document API; keeps comments and key order) ----------

function yamlMapping(src: string): Document.Parsed {
  const doc = parseDocument(src.trim() === "" ? "{}" : src);
  if (doc.errors.length) throw new OfError("invalid_request", `cannot parse YAML: ${doc.errors[0]!.message}`);
  if (doc.contents === null) doc.contents = new YAMLMap() as never;
  if (!isMap(doc.contents)) throw new OfError("invalid_request", "YAML document root must be a mapping");
  return doc;
}

const yamlText = (doc: Document) => doc.toString({ indent: 2, lineWidth: 0 });

/** editYAMLDocument: sets (or with null removes) top-level keys, sorted. */
export function editYamlDocument(src: string, set: Record<string, unknown>): string {
  const doc = yamlMapping(src);
  for (const k of Object.keys(set).sort()) {
    const v = set[k];
    if (v === null || v === undefined) doc.delete(k);
    else doc.set(k, v);
  }
  return yamlText(doc);
}

interface MdDoc {
  front: Document;
  body: string;
}

/** parseMarkdownDoc: YAML frontmatter between "---" lines, then the body. */
export function parseMarkdownDoc(src: string): MdDoc {
  const s = src.replace(/\r\n/g, "\n");
  const empty = () => new Document(new YAMLMap());
  if (!s.startsWith("---\n") && s !== "---") return { front: empty(), body: s };
  let rest = s.slice(3);
  if (rest.startsWith("\n")) rest = rest.slice(1);
  let end = -1;
  if (rest.startsWith("---\n") || rest === "---") end = 0;
  else if (rest.includes("\n---\n")) end = rest.indexOf("\n---\n") + 1;
  else if (rest.endsWith("\n---")) end = rest.length - 3;
  if (end < 0) throw new Error("frontmatter is not terminated by a '---' line");
  const fm = rest.slice(0, end);
  let body = rest.slice(end).replace(/^---/, "");
  if (body.startsWith("\n")) body = body.slice(1);
  let front = empty();
  if (fm.trim() !== "") {
    const d = parseDocument(fm);
    if (d.errors.length) throw new Error(`frontmatter: ${d.errors[0]!.message}`);
    if (d.contents !== null) {
      if (!isMap(d.contents)) throw new Error("frontmatter must be a YAML mapping");
      front = d;
    }
  }
  return { front, body };
}

export function mdString(d: MdDoc): string {
  let out = "---\n";
  if (isMap(d.front.contents) && d.front.contents.items.length > 0) out += yamlText(d.front);
  out += "---\n";
  const body = d.body.replace(/^\n+/, "");
  if (body !== "") out += "\n" + body + (body.endsWith("\n") ? "" : "\n");
  return out;
}

function nodeString(n: unknown): string {
  if (isScalar(n)) return n.value === null || n.value === undefined ? "" : String(n.value);
  return "";
}

function frontGet(d: Document, path: string[]): unknown {
  return d.getIn(path, true);
}

function setOrDelete(d: Document, path: string[], v: string): void {
  const t = v.trim();
  if (t === "") d.deleteIn(path);
  else d.setIn(path, t);
}

// ---------- agent spec ----------

export interface AgentSpec {
  name: string;
  path: string;
  has_file: boolean;
  editable: boolean;
  description: string;
  harness_type: string;
  model: string;
  reasoning_level: string;
  auth_secret: string;
  machine: string;
  worker_host: string;
  environment_id: string;
  secrets: string[];
  mcp_servers: string;
  idle_timeout_minutes: string;
  prompt: string;
  machines: string[];
}

function fillAgentSpec(s: AgentSpec, d: MdDoc): void {
  const f = d.front;
  s.description = nodeString(frontGet(f, ["description"]));
  s.model = nodeString(frontGet(f, ["model"]));
  s.machine = nodeString(frontGet(f, ["machine"]));
  s.worker_host = nodeString(frontGet(f, ["workerHost"]));
  s.environment_id = nodeString(frontGet(f, ["environmentId"]));
  const sec = frontGet(f, ["secrets"]);
  if (isSeq(sec)) s.secrets = sec.items.map((x) => nodeString(x));
  const mcp = frontGet(f, ["mcpServers"]);
  if (mcp) s.mcp_servers = new Document(mcp).toString({ indent: 2, lineWidth: 0 });
  s.idle_timeout_minutes = nodeString(frontGet(f, ["idleTimeoutMinutes"]));
  s.prompt = d.body.replace(/^\n+/, "");
  const h = frontGet(f, ["harness"]);
  if (isMap(h)) {
    s.harness_type = nodeString(h.get("type", true));
    if (s.harness_type === "claude-code") s.harness_type = "claude";
    s.model = nodeString(h.get("model", true));
    s.reasoning_level = nodeString(h.get("reasoningLevel", true));
    const a = h.get("auth", true);
    if (isMap(a)) s.auth_secret = nodeString(a.get("secretName", true));
  }
}

const SECRET_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

function applyAgentSpec(d: MdDoc, u: JsonObject): void {
  const f = d.front;
  const s = (k: string) => str(u, k);
  setOrDelete(f, ["description"], s("description"));
  switch (s("harness_type")) {
    case "":
      f.delete("harness");
      setOrDelete(f, ["model"], s("model"));
      break;
    case "claude":
    case "codex": {
      f.delete("model");
      if (!isMap(f.get("harness", true))) f.set("harness", new YAMLMap());
      f.setIn(["harness", "type"], s("harness_type"));
      setOrDelete(f, ["harness", "model"], s("model"));
      setOrDelete(f, ["harness", "reasoningLevel"], s("reasoning_level"));
      if (s("auth_secret").trim() !== "") {
        const a = new YAMLMap();
        a.set("source", "managedSecret");
        a.set("secretName", s("auth_secret").trim());
        f.setIn(["harness", "auth"], a);
      } else f.deleteIn(["harness", "auth"]);
      break;
    }
    default:
      throw new OfError(
        "invalid_request",
        `unknown harness ${JSON.stringify(s("harness_type"))} (use claude or codex)`,
      );
  }
  setOrDelete(f, ["machine"], s("machine"));
  setOrDelete(f, ["workerHost"], s("worker_host"));
  setOrDelete(f, ["environmentId"], s("environment_id"));
  const secrets: string[] = [];
  for (const raw of optStringArray(u, "secrets") ?? []) {
    const sn = raw.trim();
    if (sn === "" || secrets.includes(sn)) continue;
    if (!SECRET_NAME_RE.test(sn)) throw new OfError("invalid_request", `invalid secret name ${JSON.stringify(sn)}`);
    secrets.push(sn);
  }
  if (secrets.length) {
    const seq = new YAMLSeq();
    for (const x of secrets) seq.add(new Scalar(x));
    f.set("secrets", seq);
  } else f.delete("secrets");
  if (s("mcp_servers").trim() !== "") {
    const m = parseDocument(s("mcp_servers"));
    if (m.errors.length || !isMap(m.contents)) {
      throw new OfError("invalid_request", `mcpServers: ${m.errors[0]?.message ?? "must be a YAML mapping"}`);
    }
    f.set("mcpServers", m.contents);
  } else f.delete("mcpServers");
  const t = s("idle_timeout_minutes").trim();
  if (t !== "") {
    const n = /^[+-]?\d+$/.test(t) ? Number(t) : NaN;
    if (!Number.isInteger(n) || n < 0 || n > 60)
      throw new OfError("invalid_request", "idle_timeout_minutes must be 0 through 60");
    f.set("idleTimeoutMinutes", n);
  } else f.delete("idleTimeoutMinutes");
  const prompt = optString(u, "prompt");
  if (prompt !== undefined) d.body = prompt.replace(/\r\n/g, "\n");
}

async function agentSpec(
  kit: TeamKit,
  f: FactoryRecord,
  name: string,
): Promise<{ spec: AgentSpec; files: Record<string, string> }> {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND name = ?", f.id, name);
  if (!r) throw new OfError("resource_not_found", `agent ${name} not found`);
  const agent = JSON.parse(r.data) as AgentConfig;
  const files = await activeFiles(kit, f);
  const spec: AgentSpec = {
    name: agent.name,
    path: agentFile(name),
    has_file: false,
    editable: false,
    description: agent.description,
    harness_type: "",
    model: "",
    reasoning_level: "",
    auth_secret: "",
    machine: "",
    worker_host: "",
    environment_id: "",
    secrets: [],
    mcp_servers: "",
    idle_timeout_minutes: "",
    prompt: agent.instructions,
    machines: [],
  };
  const src = files[spec.path];
  if (src !== undefined) {
    spec.has_file = true;
    try {
      fillAgentSpec(spec, parseMarkdownDoc(src));
    } catch {
      /* keep the compiled values */
    }
  }
  spec.editable = spec.has_file && f.source !== "git";
  if (Object.keys(files).length > 0) {
    const def = compile(files).definition;
    if (def?.machines) spec.machines = Object.keys(def.machines).sort();
  }
  return { spec, files };
}

// ---------- automation files ----------

const RESOURCE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/;

async function automationFiles(
  kit: TeamKit,
  f: FactoryRecord,
  name: string,
  content0: string,
): Promise<{ files: Record<string, string>; path: string }> {
  if (!RESOURCE_NAME_RE.test(name) || name === "new") {
    throw new OfError("invalid_request", `automation names use letters, numbers, '-' and '_' (and cannot be "new")`);
  }
  const content = content0.replace(/\r\n/g, "\n");
  if (content.trim() === "") throw new OfError("invalid_request", "content is empty");
  if (content.length > 512 * 1024) throw new OfError("invalid_request", "content is larger than 512 KB");
  const path = automationFile(name);
  try {
    parseMarkdownDoc(content);
  } catch (e) {
    throw validationError("automation file cannot be parsed", [{ file: path, message: (e as Error).message }]);
  }
  const files = await activeFiles(kit, f);
  files[path] = content;
  return { files, path };
}

/** applyFiles: apply edited files as a new version or answer the validation error. */
async function applyFiles(
  kit: TeamKit,
  c: OpCall,
  f: FactoryRecord,
  files: Record<string, string>,
  message: string,
  detail = "updated definition is invalid",
): Promise<DefinitionVersion> {
  const r = await applyDefinition(kit, f.id, { files, author: actorId(c.meta.principal), message });
  if (r.invalid) throw validationError(detail, r.version.errors);
  return r.version;
}

function summarize(def: CompiledDefinition): JsonObject {
  return {
    name: def.name,
    ...(def.alias ? { alias: def.alias } : {}),
    agents: (def.agents ?? []).map((a) => a.name),
    automations: (def.automations ?? []).map((a) => a.name),
    machines: Object.keys(def.machines ?? {}).sort(),
    scorers: (def.scorers ?? []).map((s) => s.slug),
    webhooks: (def.webhooks ?? []).map((w) => w.name),
    benchmarks: (def.benchmarks ?? []).map((b) => (b as { slug: string }).slug),
    skills: (def.skills ?? []).map((s) => (s as { name: string }).name),
  };
}

function validateFiles(kit: TeamKit, f: FactoryRecord, files: Record<string, string> | undefined): JsonObject {
  if (!files || Object.keys(files).length === 0) throw new OfError("invalid_request", "files is required");
  const chk = checkDefinition(kit, f, files);
  const valid = chk.errors.length === 0 && chk.definition !== null;
  return {
    valid,
    errors: chk.errors,
    warnings: chk.warnings,
    skipped: chk.skipped,
    ...(valid ? { summary: summarize(chk.definition!) } : {}),
  };
}

const INTEGRATION_TYPES = [
  { type: "slack", label: "Slack", hint: "Mention the foreman in channels and threads." },
  { type: "microsoft-teams", label: "Microsoft Teams", hint: "Mention the foreman in Teams channels." },
  { type: "linear", label: "Linear", hint: "Assign or mention the foreman on Linear issues." },
  { type: "jira", label: "Jira", hint: "Route Jira issues to the factory." },
];
const CODE_FORGES = ["GITHUB", "GITLAB", "AZURE_DEVOPS"];
const ALIAS_RE = /^[A-Za-z0-9 ._-]{1,60}$/;
const validAlias = (s: string) => ALIAS_RE.test(s) && s.trim() !== "";

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

function checkAliasFree(kit: TeamKit, alias: string, exceptId: string): void {
  const other = listFactories(kit).find((f) => f.id !== exceptId && f.alias.toLowerCase() === alias.toLowerCase());
  if (other)
    throw new OfError(
      "conflict",
      `alias ${JSON.stringify(alias)} is already used by factory ${JSON.stringify(other.name)}`,
    );
}

function resolveCreateFiles(
  b: JsonObject,
  source: string,
  name: string,
  alias: string,
  description: string,
): Record<string, string> | null {
  const files = b["files"];
  if (files && typeof files === "object" && Object.keys(files).length > 0) return files as Record<string, string>;
  let useDefaults = source === "managed";
  const d = b["definition"];
  if (d !== undefined && d !== null) {
    if (typeof d === "string") {
      if (d !== "defaults")
        throw new OfError("invalid_request", `definition must be "defaults" or an object with "files"`);
      useDefaults = true;
    } else {
      const fs = (d as { files?: unknown }).files;
      if (typeof d !== "object" || !fs || typeof fs !== "object" || Object.keys(fs).length === 0) {
        throw new OfError("invalid_request", `definition must be "defaults" or an object with a non-empty "files" map`);
      }
      return fs as Record<string, string>;
    }
  }
  if (!useDefaults) return null;
  let agents: string[] | undefined;
  const ra = b["agents"];
  if (Array.isArray(ra)) {
    agents = [];
    for (const a0 of ra) {
      const a = String(a0).trim().toLowerCase();
      if (a === "foreman" || a === "main") continue;
      if (!(DEFAULT_AGENT_NAMES as readonly string[]).includes(a)) {
        throw new OfError(
          "invalid_request",
          `unknown default agent ${JSON.stringify(a)} (choose from ${DEFAULT_AGENT_NAMES.join(", ")})`,
        );
      }
      if (!agents.includes(a)) agents.push(a);
    }
  }
  const integrations = Array.isArray(b["integrations"]) ? (b["integrations"] as { type?: string }[]) : [];
  let trackers = 0;
  for (const it of integrations) {
    let t = String(it?.type ?? "")
      .trim()
      .toLowerCase();
    if (t === "teams" || t === "microsoft_teams") t = "microsoft-teams";
    if (!INTEGRATION_TYPES.some((x) => x.type === t)) {
      throw new OfError(
        "invalid_request",
        `unknown integration type ${JSON.stringify(it?.type ?? "")} (choose from ${INTEGRATION_TYPES.map((x) => x.type).join(", ")})`,
      );
    }
    if (t === "linear" || t === "jira") trackers++;
  }
  if (trackers > 1)
    throw new OfError("invalid_request", "linear and jira are mutually exclusive: declare at most one issue tracker");
  const repos = Array.isArray(b["repositories"])
    ? (b["repositories"] as { owner?: string; name?: string; codeForge?: string }[])
    : [];
  repos.forEach((r, i) => {
    if (!String(r?.owner ?? "").trim() || !String(r?.name ?? "").trim()) {
      throw new OfError("invalid_request", `repositories[${i}]: owner and name are required`);
    }
    if (r.codeForge && !CODE_FORGES.includes(r.codeForge.toUpperCase())) {
      throw new OfError("invalid_request", `repositories[${i}]: codeForge must be one of ${CODE_FORGES.join(", ")}`);
    }
  });
  if (!name) throw new OfError("invalid_request", "name is required");
  return defaultDefinition({
    name,
    alias: alias || aliasFromName(name),
    ...(description ? { description } : {}),
    repositories: repos.map((r) => ({ owner: String(r.owner), name: String(r.name), codeForge: r.codeForge ?? "" })),
    ...(str(b, "model") ? { model: str(b, "model") } : {}),
    ...(agents ? { agents } : {}),
  });
}

function deleteFactoryRows(kit: TeamKit, id: string): void {
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
  runtimeCache.delete(id);
}

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listFactories: (kit, c) => {
    const page = pageFrom(c.query);
    const q = (c.query["search"] ?? "").trim().toLowerCase();
    let fs = listFactories(kit);
    if (q) fs = fs.filter((f) => f.name.toLowerCase().includes(q) || f.alias.toLowerCase().includes(q));
    const p = paginate(fs, (f) => ({ ms: msOf(f.created_at), id: f.id }), page);
    return listResponse(
      p.items.map((f) => factoryView(kit, f)),
      p.next,
    );
  },

  getFactory: (kit, c) => factoryView(kit, factoryFor(kit, c)),

  createFactory: async (kit, c) => {
    const p = c.principal();
    if (roleRank(p.role) < roleRank("editor"))
      throw new OfError("not_authorized", "creating a factory requires the team-wide editor role");
    const b = bodyObject(c.body);
    let name = str(b, "name").trim();
    let alias = str(b, "alias").trim();
    let description = str(b, "description");
    const source = str(b, "source") || "managed";
    if (source !== "managed" && source !== "git")
      throw new OfError("invalid_request", `source must be "managed" or "git"`);
    if (source === "git" && str(b, "git_repo").trim() === "")
      throw new OfError("invalid_request", "git_repo is required for a git-backed definition");
    const files = resolveCreateFiles(b, source, name, alias, description);
    if (files && Object.keys(files).length > 0) {
      const res = compile(files);
      if (res.errors.length > 0) throw validationError("definition is invalid", res.errors);
      const def = res.definition;
      if (def) {
        name ||= def.name;
        alias ||= def.alias ?? "";
        description ||= def.description ?? "";
      }
    }
    if (name === "") throw new OfError("invalid_request", "name is required");
    if ([...name].length > 100) throw new OfError("invalid_request", "name must be at most 100 characters");
    if (alias === "") alias = aliasFromName(name);
    if (!validAlias(alias))
      throw new OfError("invalid_request", "alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'");
    const now = new Date().toISOString();
    const f: FactoryRecord = {
      id: newId("fac"),
      team_id: kit.teamId(),
      name,
      alias,
      ...(description ? { description } : {}),
      source,
      created_at: now,
      updated_at: now,
    };
    if (source === "git") {
      f.git_repo = str(b, "git_repo").trim();
      f.git_branch = str(b, "git_branch").trim() || "main";
      const gp = str(b, "git_path")
        .trim()
        .replace(/^\/+|\/+$/g, "");
      if (gp) f.git_path = gp;
    }
    kit.tx(() => {
      checkAliasFree(kit, alias, "");
      putFactory(kit, f);
    });
    let version: DefinitionVersion | undefined;
    if (files && Object.keys(files).length > 0) {
      let r: ApplyResult;
      try {
        r = await applyDefinition(kit, f.id, { files, author: actorId(p), message: "Create factory" });
      } catch (e) {
        kit.tx(() => deleteFactoryRows(kit, f.id));
        throw e;
      }
      if (r.invalid) {
        kit.tx(() => deleteFactoryRows(kit, f.id));
        throw validationError("definition could not be applied", r.version.errors);
      }
      version = r.version;
    }
    const nf = getFactoryById(kit, f.id) ?? f;
    return created({ factory: factoryView(kit, nf), ...(version ? { version } : {}) });
  },

  updateFactory: async (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    const yamlEdits: Record<string, unknown> = {};
    const name = optString(b, "name");
    if (name !== undefined) {
      const n = name.trim();
      if (n === "" || [...n].length > 100) throw new OfError("invalid_request", "name must be 1-100 characters");
      f.name = n;
      yamlEdits["name"] = n;
    }
    const alias = optString(b, "alias");
    if (alias !== undefined) {
      const a = alias.trim();
      if (!validAlias(a))
        throw new OfError("invalid_request", "alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'");
      checkAliasFree(kit, a, f.id);
      f.alias = a;
      yamlEdits["alias"] = a;
    }
    const desc = optString(b, "description");
    if (desc !== undefined) {
      const d = desc.trim();
      if (d) f.description = d;
      else delete f.description;
      yamlEdits["description"] = d || null;
    }
    const cs0 = optString(b, "credential_strategy");
    if (cs0 !== undefined) {
      const cs = cs0.trim().toUpperCase();
      if (cs !== "" && cs !== "EXECUTOR" && cs !== "CREATOR")
        throw new OfError("invalid_request", "credential_strategy must be EXECUTOR or CREATOR");
      yamlEdits["credentialStrategy"] = cs || null;
      if (f.source !== "managed")
        throw new OfError(
          "operation_not_supported",
          "credentialStrategy of a git-backed factory is set in its factory.yaml",
        );
    }
    const source = optString(b, "source");
    const gitRepo = optString(b, "git_repo");
    const gitBranch = optString(b, "git_branch");
    const gitPath = optString(b, "git_path");
    if (source !== undefined) {
      if (source === "managed") f.source = "managed";
      else if (source === "git") {
        const repo = gitRepo !== undefined ? gitRepo.trim() : (f.git_repo ?? "");
        if (!repo) throw new OfError("invalid_request", "git_repo is required to switch the definition source to git");
        f.source = "git";
        f.git_branch ||= "main";
      } else throw new OfError("invalid_request", "source must be managed or git");
    }
    if (gitPath !== undefined) {
      const gp = gitPath.trim().replace(/^\/+|\/+$/g, "");
      if (gp.includes(".."))
        throw new OfError("invalid_request", "git_path must be a relative directory inside the repository");
    }
    if (gitRepo !== undefined || gitBranch !== undefined || gitPath !== undefined) {
      if (f.source !== "git")
        throw new OfError("operation_not_supported", "git settings apply only to git-backed factories");
      if (gitRepo !== undefined) {
        if (gitRepo.trim() === "") throw new OfError("invalid_request", "git_repo cannot be empty");
        f.git_repo = gitRepo.trim();
      }
      if (gitBranch !== undefined) f.git_branch = gitBranch.trim() || "main";
      if (gitPath !== undefined) f.git_path = gitPath.trim().replace(/^\/+|\/+$/g, "");
    }
    let out = f;
    if (f.source === "managed" && f.active_version_id && Object.keys(yamlEdits).length > 0) {
      const files = await activeFiles(kit, f);
      files["factory.yaml"] = editYamlDocument(files["factory.yaml"] ?? "", yamlEdits);
      await applyFiles(kit, c, f, files, "Update factory settings");
      const nf = getFactoryById(kit, f.id)!;
      out = { ...nf, name: f.name, alias: f.alias, source: f.source };
      if (f.description) out.description = f.description;
      else delete out.description;
      for (const k of ["git_repo", "git_branch", "git_path"] as const) {
        if (f[k]) out[k] = f[k];
      }
    }
    out.updated_at = new Date().toISOString();
    kit.tx(() => putFactory(kit, out));
    return factoryView(kit, out);
  },

  deleteFactory: (kit, c) => {
    const f = factoryFor(kit, c);
    kit.tx(() => {
      deleteFactoryRows(kit, f.id);
      const s = schedules as unknown as {
        syncSchedules?: (k: TeamKit, id: string, a: unknown[]) => void;
        syncWebhooks?: (k: TeamKit, id: string, w: unknown[]) => void;
      };
      s.syncSchedules?.(kit, f.id, []);
      s.syncWebhooks?.(kit, f.id, []);
    });
    return noContent();
  },

  getDefinition: async (kit, c) => {
    const f = factoryFor(kit, c);
    const out: JsonObject = { factory_id: f.id, source: f.source, files: {}, version: null };
    if (f.active_version_id) {
      const v = await getVersion(kit, f.active_version_id);
      if (v) {
        v.state = effectiveState(v, f.active_version_id);
        out["files"] = v.files;
        out["version"] = v;
      }
    }
    return out;
  },

  putDefinition: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git") {
      throw new OfError(
        "operation_not_supported",
        `the definition of this factory lives in ${f.git_repo}; merge a change to branch "${f.git_branch || "main"}" instead`,
      );
    }
    const b = bodyObject(c.body);
    const files = b["files"] as Record<string, string> | undefined;
    if (!files || typeof files !== "object" || Object.keys(files).length === 0)
      throw new OfError("invalid_request", "files is required");
    const base = str(b, "base_version_id");
    if (base && base !== (f.active_version_id ?? "")) {
      throw new OfError(
        "conflict",
        `the definition changed since version ${base} (active version is ${f.active_version_id ?? ""})`,
      );
    }
    const res = compile(files);
    if (res.errors.length > 0) throw validationError("definition is invalid", res.errors);
    const v = await applyFiles(
      kit,
      c,
      f,
      files,
      str(b, "message") || "Update definition",
      "definition could not be applied; the previous version stays active",
    );
    return { version: v };
  },

  validateDefinition: (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    return validateFiles(kit, f, b["files"] as Record<string, string> | undefined);
  },

  listDefinitionVersions: async (kit, c) => {
    const f = factoryFor(kit, c);
    const page = pageFrom(c.query);
    const inc = c.query["include_files"] ?? "";
    if (inc !== "" && !/^(1|t|T|TRUE|true|True|0|f|F|FALSE|false|False)$/.test(inc)) {
      throw new OfError("invalid_request", `query parameter "include_files" must be a boolean`);
    }
    const withFiles = /^(1|t|T|TRUE|true|True)$/.test(inc);
    const r = sqlPage<VersionRow & { id: string; created_ms: number } & Record<string, SqlStorageValue>>(
      kit.sql,
      {
        select: "SELECT id, created_ms, data, files, files_r2 FROM definition_versions",
        where: ["factory_id = ?"],
        args: [f.id],
      },
      page,
    );
    const items: DefinitionVersion[] = [];
    for (const row of r.rows) {
      const v = JSON.parse(row.data) as DefinitionVersion;
      v.state = effectiveState(v, f.active_version_id ?? "");
      if (withFiles) v.files = await loadFiles(kit, row);
      items.push(v);
    }
    return listResponse(items, r.next, { active_version_id: f.active_version_id ?? "" });
  },

  getDefinitionVersion: async (kit, c) => {
    const f = factoryFor(kit, c);
    const id = c.params["versionId"]!;
    const v = await getVersion(kit, id);
    if (!v || v.factory_id !== f.id) throw new OfError("resource_not_found", `definition version ${id} not found`);
    v.state = effectiveState(v, f.active_version_id ?? "");
    v.files ??= {};
    return v;
  },

  restoreDefinitionVersion: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git")
      throw new OfError("operation_not_supported", `this factory's definition lives in ${f.git_repo}: revert it there`);
    const id = c.params["versionId"]!;
    const v = await getVersion(kit, id);
    if (!v || v.factory_id !== f.id) throw new OfError("resource_not_found", `definition version ${id} not found`);
    const i = id.indexOf("_");
    const short = i >= 0 && id.length > i + 9 ? id.slice(i + 1, i + 9) : id.slice(0, 8);
    return applyFiles(kit, c, f, { ...v.files }, "Restore version " + short, "that version no longer validates");
  },

  listAgents: (kit, c) => {
    const f = factoryFor(kit, c);
    let as = listAgents(kit, f.id);
    const t = c.query["type"];
    if (t) as = as.filter((a) => a.type.toLowerCase() === t.toLowerCase());
    return listResponse(as, "");
  },

  getAgent: (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    const r =
      first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND name = ?", f.id, name) ??
      first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND id = ?", f.id, name);
    if (!r) throw new OfError("resource_not_found", `agent ${name} not found`);
    return agentOut(JSON.parse(r.data) as AgentConfig);
  },

  listAutomations: (kit, c) => listResponse(listAutomations(kit, factoryFor(kit, c).id), ""),

  getFactoryIntegrations: (kit, c) => {
    const f = factoryFor(kit, c);
    return { factory_id: f.id, integrations: getFactoryIntegrations(kit, f.id) };
  },

  putFactoryIntegrations: async (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    const req = optStringArray(b, "integrations") ?? [];
    for (const t of req)
      if (!INTEGRATION_TYPES.some((x) => x.type === t))
        throw new OfError("invalid_request", `unknown integration ${JSON.stringify(t)}`);
    const providers = INTEGRATION_TYPES.map((x) => x.type).filter((t) => req.includes(t));
    if (providers.includes("linear") && providers.includes("jira")) {
      throw new OfError("invalid_request", "declare at most one issue tracker: linear and jira are mutually exclusive");
    }
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        "integrations of a git-backed factory are declared in its factory.yaml",
      );
    if (f.active_version_id) {
      const files = await activeFiles(kit, f);
      let fy: string;
      try {
        const doc = yamlMapping(files["factory.yaml"] ?? "");
        const existing = new Map<string, unknown>();
        const seq0 = doc.get("integrations", true);
        if (isSeq(seq0))
          for (const item of seq0.items) if (isMap(item)) existing.set(nodeString(item.get("type", true)), item);
        const seq = new YAMLSeq();
        for (const p of providers) {
          if (existing.has(p)) seq.add(existing.get(p));
          else {
            const m = new YAMLMap();
            m.set("type", p);
            seq.add(m);
          }
        }
        doc.set("integrations", seq);
        fy = yamlText(doc);
      } catch (e) {
        throw new OfError("invalid_request", `factory.yaml: ${e instanceof OfError ? e.detail : (e as Error).message}`);
      }
      files["factory.yaml"] = fy;
      await applyFiles(kit, c, f, files, "Update integrations");
    }
    kit.tx(() => {
      const current = getFactoryIntegrations(kit, f.id);
      setFactoryIntegrations(kit, f.id, providers, current);
    });
    return { factory_id: f.id, integrations: getFactoryIntegrations(kit, f.id) };
  },

  getFactorySettings: async (kit, c) => {
    const f = factoryFor(kit, c);
    const out = {
      factory_id: f.id,
      editable: f.source !== "git",
      credential_strategy: "EXECUTOR",
      repositories: [] as unknown[],
      machines: [] as unknown[],
      integration_types: INTEGRATION_TYPES,
    };
    const files = await activeFiles(kit, f);
    if (Object.keys(files).length > 0) {
      const def = compile(files).definition;
      if (def) {
        if (def.credentialStrategy) out.credential_strategy = def.credentialStrategy;
        if (def.repositories) out.repositories = def.repositories;
        for (const n of Object.keys(def.machines ?? {}).sort()) {
          const rs = { ...(def.machines![n] as unknown as JsonObject) };
          if (!rs["name"]) rs["name"] = n;
          out.machines.push(rs);
        }
      }
    }
    return out;
  },

  getAgentSpec: async (kit, c) => (await agentSpec(kit, factoryFor(kit, c), c.params["name"]!)).spec,

  putAgentSpec: async (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    if (f.source === "git") {
      throw new OfError(
        "operation_not_supported",
        `this factory's definition lives in ${f.git_repo}: edit ${agentFile(name)} through a pull request`,
      );
    }
    const u = bodyObject(c.body);
    const { spec, files } = await agentSpec(kit, f, name);
    if (!spec.has_file)
      throw new OfError("operation_not_supported", `agent ${name} has no ${spec.path} in the active definition`);
    let doc: MdDoc;
    try {
      doc = parseMarkdownDoc(files[spec.path]!);
    } catch (e) {
      throw validationError("agent file cannot be parsed", [{ file: spec.path, message: (e as Error).message }]);
    }
    applyAgentSpec(doc, u);
    files[spec.path] = mdString(doc);
    await applyFiles(kit, c, f, files, str(u, "message").trim() || "Update agent " + name);
    return (await agentSpec(kit, getFactoryById(kit, f.id)!, name)).spec;
  },

  getAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    const files = await activeFiles(kit, f);
    const path = automationFile(name);
    const content = files[path];
    if (content === undefined) throw new OfError("resource_not_found", `automation ${name} not found`);
    const au = listAutomations(kit, f.id).find((a) => a.name === name);
    return { name, path, content, editable: f.source !== "git", ...(au ? { automation: au } : {}) };
  },

  putAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        `automations of a git-backed factory change through a pull request in ${f.git_repo}`,
      );
    const b = bodyObject(c.body);
    const name = c.params["name"]!;
    const { files, path } = await automationFiles(kit, f, name, str(b, "content"));
    await applyFiles(kit, c, f, files, str(b, "message").trim() || "Save automation " + name);
    const au = listAutomations(kit, f.id).find((a) => a.name === name);
    return { name, path, content: files[path], editable: true, ...(au ? { automation: au } : {}) };
  },

  deleteAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        `automations of a git-backed factory change through a pull request in ${f.git_repo}`,
      );
    const name = c.params["name"]!;
    const files = await activeFiles(kit, f);
    const prefix = `automations/${name}/`;
    let found = false;
    for (const k of Object.keys(files)) {
      if (k.startsWith(prefix)) {
        delete files[k];
        found = true;
      }
    }
    if (!found) throw new OfError("resource_not_found", `automation ${name} not found`);
    await applyFiles(kit, c, f, files, "Delete automation " + name);
    return noContent();
  },

  validateAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    try {
      const { files } = await automationFiles(kit, f, c.params["name"]!, str(b, "content"));
      return validateFiles(kit, f, files);
    } catch (e) {
      if (e instanceof OfError && Array.isArray(e.extra?.["errors"]))
        return { valid: false, errors: e.extra!["errors"] };
      throw e;
    }
  },

  /** Internal: factories of the team (plugin host). */
  "factories.list": (kit) =>
    listFactories(kit).map((f) => ({ id: f.id, teamId: f.team_id, alias: f.alias, name: f.name })),

  /** Internal: one factory and its repositories (plugin host). Body {factory_id}. */
  "factories.get": (kit, c) => {
    const f = findFactory(kit, str(bodyObject(c.body), "factory_id"));
    if (!f) throw new OfError("resource_not_found", `factory ${str(bodyObject(c.body), "factory_id")} not found`);
    const rt = first<{ data: string }>(kit.sql, "SELECT data FROM factory_runtime WHERE factory_id = ?", f.id);
    const repos = rt ? ((JSON.parse(rt.data) as FactoryRuntime).repositories ?? []) : [];
    return { id: f.id, teamId: f.team_id, alias: f.alias, name: f.name, repositories: repos };
  },

  /** Internal: apply files (git sync, proposals approval). Body {files, message, author, commit_sha}. */
  definitionsApplyFiles: async (kit, c) => {
    const b = bodyObject(c.body);
    const f = findFactory(kit, str(b, "factory_id"));
    if (!f) throw new OfError("resource_not_found", `factory ${str(b, "factory_id")} not found`);
    const r = await applyDefinition(kit, f.id, {
      files: b["files"] as Record<string, string>,
      author: str(b, "author"),
      message: str(b, "message"),
      commitSha: str(b, "commit_sha"),
    });
    if (r.invalid) throw validationError("definition could not be applied", r.version.errors);
    return { version: r.version };
  },
};

export function setup(_kit: TeamKit): void {}
