// Definition versions and their apply (engine.ApplyDefinition): compile and
// preflight run before the write; network work (name resolution, skill
// fetches) happens in the Worker before the call; one transactionSync writes
// the version, resources, runtime, integrations, factory record, schedules,
// webhooks and the limits outbox row. Also the definition routes, the run
// tool propose_definition_change and the git definition sync ports.
import { OfError, newId } from "@openfactory/core";
import {
  compile,
  keyLine,
  type AgentConfig,
  type Automation,
  type CompiledDefinition,
  type Scorer,
  type ValidationError,
} from "@openfactory/definition";
import { parseDocument } from "yaml";

/** The result of compile() of @openfactory/definition. */
export type CompileResult = ReturnType<typeof compile>;
import type { CallMeta } from "../../../contracts/do-messages";
import { actorId } from "../../../auth/roles";
import { bodyObject, str, type JsonObject } from "../../../lib/body";
import { sha256Hex } from "../../../lib/bytes";
import { dest } from "../../../lib/outbox/dest";
import { listResponse, pageFrom, sqlPage } from "../../../lib/page";
import { all, first } from "../../../lib/sql/query";
import { diffFiles, mergeProposal, proposalError } from "../../../scoring/proposals";
import { improveTrackKey, type ImprovementTrack } from "../../../scoring/service";
import { builtinPreflight } from "../preflight";
import { syncSchedules } from "../schedules";
import { syncWebhooks } from "../webhooks";
import type { OpCall, TeamKit, TeamOp } from "../kit";
import {
  activeFiles,
  activeRuntime,
  aliasFromName,
  effectiveState,
  factoryFor,
  factoryView,
  findFactory,
  getFactoryById,
  getFactoryIntegrations,
  getVersion,
  insertVersion,
  listAutomations,
  loadFiles,
  putFactory,
  runtimeCache,
  setFactoryIntegrations,
  storeFiles,
  type DefinitionVersion,
  type FactoryRecord,
  type FactoryRuntime,
  type VersionRow,
} from "./repo";

// ---------- check ----------

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

/**
 * A preflight check of another module (workers, MCP servers, webhooks...). No
 * network I/O; a check may await secret decryption.
 */
export type PreflightCheck = (
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  files: Record<string, string>,
  chk: DefinitionCheck,
) => void | Promise<void>;

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

/**
 * checkDefinition: compile (unless the caller passes the result of compile(files)),
 * the built-in checks (preflight.ts), then the checks of the modules.
 */
export async function checkDefinition(
  kit: TeamKit,
  f: FactoryRecord,
  files: Record<string, string>,
  res: CompileResult = compile(files),
): Promise<DefinitionCheck> {
  const chk: DefinitionCheck = { definition: null, errors: [], warnings: [], skipped: [] };
  let verrs = res.errors;
  if (!res.definition && verrs.length === 0)
    verrs = [{ file: "factory.yaml", message: "definition could not be compiled" }];
  if (verrs.length > 0) {
    chk.errors.push(...verrs);
    return chk;
  }
  const def = res.definition!;
  chk.definition = def;
  builtinPreflight(kit, f, def, files, chk);
  for (const p of kit.preflights) {
    try {
      await p.fn(kit, f, def, files, chk);
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

/**
 * CheckGitSource (api.Options): makes sure that the git branch of a factory
 * compiles and passes the preflight (forges/defsync.ts checkGitSource), with
 * outcome naming what a refusal left as it was. wiring.ts sets it; without
 * it no branch is checked.
 */
export type GitSourceChecker = (kit: TeamKit, f: FactoryRecord, outcome: string) => Promise<void>;

let gitSourceChecker: GitSourceChecker | undefined;

/** Sets the checker; returns the one it replaces (tests restore it). */
export function setGitSourceChecker(fn: GitSourceChecker | undefined): GitSourceChecker | undefined {
  const prev = gitSourceChecker;
  gitSourceChecker = fn;
  return prev;
}

/** The checker that wiring.ts set, or undefined (then no branch is checked). */
export function gitSource(): GitSourceChecker | undefined {
  return gitSourceChecker;
}

/**
 * The git definition sync of a team (defSync in internal/server/defsync.go):
 * a push to the production branch, the check of a pull request, and the
 * periodic sync of every git-backed factory. wiring.ts sets it.
 */
export interface DefSyncRunner {
  push(kit: TeamKit, repo: string, branch: string, sha: string): Promise<void>;
  pullRequest(kit: TeamKit, pr: Record<string, unknown>): Promise<void>;
  syncAll(kit: TeamKit): Promise<void>;
}

let defSyncRunner: DefSyncRunner | undefined;

/** Sets the runner; returns the one it replaces (tests restore it). */
export function setDefSyncRunner(r: DefSyncRunner | undefined): DefSyncRunner | undefined {
  const prev = defSyncRunner;
  defSyncRunner = r;
  return prev;
}

export function defSync(): DefSyncRunner {
  if (!defSyncRunner) throw new OfError("feature_not_available", "git definition sync is not configured");
  return defSyncRunner;
}

export function validationError(detail: string, errs: ValidationError[] | undefined): OfError {
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

/**
 * The webhooks of def with their UIDs: a webhook keeps the UID of the stored
 * webhook of the same name, and a new one gets a random UID (not a stable ID,
 * so a webhook removed and added again gets a new URL).
 */
function webhooksWithIds(kit: TeamKit, factoryId: string, def: CompiledDefinition): unknown[] {
  const byName = new Map(
    all<{ id: string; name: string }>(kit.sql, "SELECT id, name FROM webhooks WHERE factory_id = ?", factoryId).map(
      (r) => [r.name, r.id],
    ),
  );
  return (def.webhooks ?? []).map((w) => ({ ...w, id: w.id || byName.get(w.name) || newId("whk") }));
}

export interface ApplyInput {
  files: Record<string, string>;
  /** compile(files), when the caller already compiled the files. */
  compiled?: CompileResult;
  author?: string;
  message?: string;
  commitSha?: string;
}

export interface ApplyResult {
  version: DefinitionVersion;
  /** Set when the definition is invalid: the stored invalid version has its errors. */
  invalid?: boolean;
}

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
  const chk = await checkDefinition(kit, f0, input.files, input.compiled);
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
    else if (!nf.alias) nf.alias = aliasFromName(nf.name);
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
    syncSchedules(kit, factoryId, m.automations);
    syncWebhooks(kit, factoryId, webhooksWithIds(kit, factoryId, def));
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
    runtimeCache(kit).delete(factoryId);
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
  const hit = runtimeCache(kit).get(f.id);
  if (hit && hit.versionId === vid) return hit.snapshot;
  const snapshot = {
    factory: f,
    version_id: vid,
    runtime: activeRuntime(kit, f.id) ?? ({ version_id: "", repositories: null } satisfies FactoryRuntime),
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
  runtimeCache(kit).set(f.id, { versionId: vid, snapshot });
  return snapshot;
}

/** Input of TeamRpc.proposeDefinition (the run tool propose_definition_change). */
export interface ProposeDefinitionInput {
  factoryId: string;
  /** Path (relative to the definition root) to the full new content. */
  files: Record<string, string>;
  /** Paths to remove. */
  delete?: string[];
  message: string;
  authorRunId: string;
  idempotencyKey: string;
  /** The ID of the review inbox item that the caller creates in the WorkDO of the run. */
  inboxItemId?: string;
  /** The work item of the run: a Self-improvement run gives its review to the reviewer chosen at start. */
  workItemId?: string;
  selfImprovement?: boolean;
}

/**
 * TeamRpc.proposeDefinition (scoring.Service.ProposeDefinitionChange): merges
 * the changes onto the active version, runs the compiler and the preflight,
 * and stores a version with the state "proposed". Idempotent on the tool key.
 * Errors of the check come back as invalid_request so the agent can fix them.
 */
export async function proposeDefinition(
  kit: TeamKit,
  _meta: CallMeta,
  input: ProposeDefinitionInput,
): Promise<{ version: Omit<DefinitionVersion, "files">; diff: string; reviewer_id?: string }> {
  const f = findFactory(kit, input.factoryId);
  if (!f) throw new OfError("resource_not_found", `factory ${input.factoryId} not found`);
  const replay = async (id: string) => {
    const v = (await getVersion(kit, id))!;
    const base = v.base_version_id ? await getVersion(kit, v.base_version_id) : undefined;
    const { files, ...meta } = v;
    return { version: meta, diff: diffFiles(base?.files ?? {}, files) };
  };
  const prev = first<{ id: string }>(
    kit.sql,
    "SELECT id FROM definition_versions WHERE idem_key = ?",
    input.idempotencyKey,
  );
  if (prev) return replay(prev.id);
  const message = (input.message ?? "").trim();
  if (!message) throw new OfError("invalid_request", "message is required");
  if (f.source === "git") {
    throw new OfError(
      "invalid_request",
      `the definition of this factory is in git: open a pull request against ${f.git_repo ?? ""} instead`,
    );
  }
  const base = f.active_version_id ? await getVersion(kit, f.active_version_id) : undefined;
  const tree = mergeProposal(base?.files ?? {}, { files: input.files ?? {}, delete: input.delete ?? [] });
  const chk = await checkDefinition(kit, f, tree.files);
  if (chk.errors.length > 0) throw proposalError(chk.errors);
  let reviewer = "";
  if (input.selfImprovement && input.workItemId) {
    const raw = first<{ value: string }>(
      kit.sql,
      "SELECT value FROM scoring_kv WHERE key = ?",
      improveTrackKey(input.workItemId),
    )?.value;
    if (raw) {
      try {
        reviewer = (JSON.parse(raw) as ImprovementTrack).reviewer_id ?? "";
      } catch {
        reviewer = "";
      }
    }
  }
  const stored = await storeFiles(kit, tree.files);
  const out = kit.tx(() => {
    const again = first<{ id: string }>(
      kit.sql,
      "SELECT id FROM definition_versions WHERE idem_key = ?",
      input.idempotencyKey,
    );
    if (again) return { duplicate: again.id };
    const v: DefinitionVersion = {
      id: newId("defv"),
      factory_id: f.id,
      files: tree.files,
      author: "run:" + input.authorRunId,
      message,
      valid: true,
      created_at: new Date().toISOString(),
      state: "proposed",
      ...(f.active_version_id ? { base_version_id: f.active_version_id } : {}),
      author_run_id: input.authorRunId,
      ...(input.inboxItemId ? { inbox_item_id: input.inboxItemId } : {}),
    };
    insertVersion(kit, v, stored, input.idempotencyKey);
    const { files: _f, ...meta } = v;
    return { version: meta };
  });
  if ("duplicate" in out && out.duplicate) return replay(out.duplicate);
  return { version: out.version!, diff: tree.diff, ...(reviewer ? { reviewer_id: reviewer } : {}) };
}

/** applyFiles: apply edited files as a new version or answer the validation error. */
export async function applyFiles(
  kit: TeamKit,
  c: OpCall,
  f: FactoryRecord,
  files: Record<string, string>,
  message: string,
  detail = "updated definition is invalid",
  compiled?: CompileResult,
): Promise<DefinitionVersion> {
  const r = await applyDefinition(kit, f.id, {
    files,
    author: actorId(c.meta.principal),
    message,
    ...(compiled ? { compiled } : {}),
  });
  if (r.invalid) throw validationError(detail, r.version.errors);
  return r.version;
}

export function summarize(def: CompiledDefinition): JsonObject {
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

export async function validateFiles(
  kit: TeamKit,
  f: FactoryRecord,
  files: Record<string, string> | undefined,
): Promise<JsonObject> {
  if (!files || Object.keys(files).length === 0) throw new OfError("invalid_request", "files is required");
  const chk = await checkDefinition(kit, f, files);
  const valid = chk.errors.length === 0 && chk.definition !== null;
  return {
    valid,
    errors: chk.errors,
    warnings: chk.warnings,
    skipped: chk.skipped,
    ...(valid ? { summary: summarize(chk.definition!) } : {}),
  };
}
// ---------- ops ----------

export const definitionOps: Record<string, TeamOp> = {
  /** Internal (GitHub webhook): a push to a branch. Body {repo, branch, sha}. */
  defsyncPush: async (kit, c) => {
    const b = bodyObject(c.body);
    await defSync().push(kit, str(b, "repo"), str(b, "branch"), str(b, "sha"));
    return { ok: true };
  },
  /** Internal (GitHub webhook): a pull request that changes definition files. Body DefinitionPR. */
  defsyncPullRequest: async (kit, c) => {
    await defSync().pullRequest(kit, bodyObject(c.body));
    return { ok: true };
  },
  /** Internal (GitHub webhook): github.factoryConfigChecks of the active definition ("main" by default). */
  defsyncConfigChecks: async (kit, c) => {
    const f = getFactoryById(kit, str(bodyObject(c.body), "factoryId"));
    if (!f?.active_version_id) return { mode: "main" };
    const src = (await activeFiles(kit, f))["factory.yaml"] ?? "";
    const doc = parseDocument(src).toJS() as { github?: { factoryConfigChecks?: unknown } } | null;
    const mode = doc?.github?.factoryConfigChecks;
    return { mode: typeof mode === "string" && mode !== "" ? mode : "main" };
  },
  /** Internal (cron): syncs every git-backed factory of the team. */
  defsyncAll: async (kit) => {
    await defSync().syncAll(kit);
    return { ok: true };
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
      res,
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
