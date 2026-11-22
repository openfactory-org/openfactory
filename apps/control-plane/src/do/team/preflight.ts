// TeamDO module: definition preflight checks against the team that the
// factories module does not own. Port of internal/engine/preflight.go
// (webhooks, webhook filters, approvers, models, credentials) and
// internal/engine/principal.go (runAs). Adds the check that workerEnvironment
// auth targets a development process worker.
//
// Not ported: preflightNameFilters (the provider name lookups of trigger
// filters need network calls; names stay as written and are compared on each
// event) and preflightMachines (the compiler rejects macOS and Windows, and
// the registryCredentialSecretName warning is not ported yet).
import { OfError } from "@openfactory/core";
import {
  WEBHOOK_UID_PREFIX,
  harnessModels,
  keyLine,
  validURLTokenSecret,
  type AgentConfig,
  type CompiledDefinition,
  type HarnessConfig,
} from "@openfactory/definition";
import { registeredProviders } from "../dispatch/providers";
import { isApprover, resolveRunAs, type PrincipalUser } from "../../engine/principal";
import { integrationProvider } from "../../forges/registry";
import { bodyObject, str } from "../../lib/body";
import type { Migration } from "../../lib/sql/migrate";
import { all } from "../../lib/sql/query";
import { listUsers } from "./auth";
import { registerPreflight, type DefinitionCheck, type FactoryRecord } from "./factories";
import { getSecretValue } from "./secrets";
import type { TeamKit, TeamOp } from "./index";

export const migrations: readonly Migration[] = [];

const agentFile = (n: string) => `agents/${n}/agent.md`;
const automationFile = (n: string) => `automations/${n}/automation.md`;
const scorerFile = (n: string) => `scorers/${n}/scorer.md`;
const webhookFile = (n: string) => `webhooks/${n}.yaml`;
const q = (s: string) => JSON.stringify(s);

const teamUsers = (kit: TeamKit): PrincipalUser[] => listUsers(kit) as unknown as PrincipalUser[];

// ---------- webhooks ----------

interface StoredWebhook {
  id: string;
  name: string;
  auth_mode: string;
  signature_scheme?: string;
}

function storedWebhooks(kit: TeamKit, factoryId: string): StoredWebhook[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM webhooks WHERE factory_id = ?", factoryId).map(
    (r) => JSON.parse(r.data) as StoredWebhook,
  );
}

/**
 * preflightWebhooks: authMode and signatureScheme cannot change after the
 * first apply, and the secret of a url_token webhook must be usable in a URL.
 */
export async function preflightWebhooks(
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  chk: DefinitionCheck,
): Promise<void> {
  if (!def.webhooks?.length) return;
  const byName = new Map(storedWebhooks(kit, f.id).map((w) => [w.name, w]));
  for (const w of def.webhooks) {
    const file = webhookFile(w.name);
    const prev = byName.get(w.name);
    if (prev) {
      if (prev.auth_mode !== w.auth_mode)
        chk.errors.push({
          file,
          field: "authMode",
          message: `authMode cannot change after the first apply (${prev.auth_mode} to ${w.auth_mode}); remove the webhook, apply, and add a new one`,
        });
      if ((prev.signature_scheme ?? "") !== (w.signature_scheme ?? ""))
        chk.errors.push({
          file,
          field: "signatureScheme",
          message: `signatureScheme cannot change after the first apply (${q(prev.signature_scheme ?? "")} to ${q(w.signature_scheme ?? "")}); remove the webhook, apply, and add a new one`,
        });
    }
    if (w.auth_mode !== "url_token" || !w.secret_name) continue;
    let v: string | undefined;
    try {
      v = await getSecretValue(kit, f.id, w.secret_name);
    } catch {
      continue;
    }
    // A missing secret is reported by the secret check.
    if (v !== undefined && !validURLTokenSecret(v))
      chk.errors.push({
        file,
        field: "secretName",
        message: `the value of secret ${q(w.secret_name)} cannot be used in a URL; a url_token secret accepts only letters, digits, '-', '.', '_' and '~'`,
      });
  }
}

/**
 * webhookFilterIds: the entries of the webhook_ids key of a compiled webhook
 * filter and the key path of the list ("webhook_ids" or "webhook_ids.in").
 */
export function webhookFilterIds(filter: unknown): { field: string; ids: string[] } {
  let v = filter;
  if (typeof v === "string") {
    try {
      v = JSON.parse(v);
    } catch {
      return { field: "", ids: [] };
    }
  }
  const raw = v && typeof v === "object" ? (v as Record<string, unknown>)["webhook_ids"] : undefined;
  const strings = (x: unknown): string[] | undefined =>
    Array.isArray(x) && x.every((e) => typeof e === "string") ? (x as string[]) : undefined;
  const list = strings(raw);
  if (list) return { field: "webhook_ids", ids: list };
  if (raw && typeof raw === "object") {
    const inList = strings((raw as Record<string, unknown>)["in"]);
    if (inList) return { field: "webhook_ids.in", ids: inList };
  }
  return { field: "", ids: [] };
}

/**
 * preflightWebhookFilters: a webhook UID in the webhook_ids filter of a
 * webhook trigger must belong to a stored webhook whose file the definition
 * keeps. The compiler checks names; this checks the entries that look like a UID.
 */
export function preflightWebhookFilters(
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  chk: DefinitionCheck,
): void {
  const names = new Set((def.webhooks ?? []).map((w) => w.name));
  const refs: { file: string; field: string; value: string }[] = [];
  for (const au of def.automations ?? []) {
    (au.triggers ?? []).forEach((t, i) => {
      if (t.provider !== "webhook" || t.filter === undefined || t.filter === null) return;
      const { field, ids } = webhookFilterIds(t.filter);
      ids.forEach((id, j) => {
        if (!names.has(id) && id.startsWith(WEBHOOK_UID_PREFIX))
          refs.push({ file: automationFile(au.name), field: `triggers[${i}].filter.${field}[${j}]`, value: id });
      });
    });
  }
  if (refs.length === 0) return;
  const known = new Set(
    storedWebhooks(kit, f.id)
      .filter((w) => names.has(w.name))
      .map((w) => w.id),
  );
  const hint = [...names].sort().join(", ") || "none; add webhooks/<name>.yaml";
  for (const r of refs) {
    if (!known.has(r.value))
      chk.errors.push({
        file: r.file,
        field: r.field,
        message: `webhook ${q(r.value)} does not exist; known: ${hint}`,
      });
  }
}

// ---------- principals ----------

const CREATOR = "CREATOR";

function effectiveStrategy(agent: string | undefined, factory: string | undefined): string {
  let s = (agent ?? "").trim().toUpperCase();
  if (!s) s = (factory ?? "").trim().toUpperCase();
  return s === CREATOR ? CREATOR : "EXECUTOR";
}

const USER_TOKEN_PROVIDERS = new Set(["github", "gitlab", "azure_devops"]);

/** The forges of the repositories for which userId has no personal token (user_forge_tokens). */
function missingForgeLinks(kit: TeamKit, def: CompiledDefinition, userId: string): string[] {
  const have = new Set(
    all<{ provider: string }>(kit.sql, "SELECT provider FROM user_forge_tokens WHERE user_id = ?", userId).map(
      (r) => r.provider,
    ),
  );
  const missing = new Set<string>();
  for (const r of def.repositories ?? []) {
    const p = integrationProvider(r.codeForge || "GITHUB");
    if (USER_TOKEN_PROVIDERS.has(p) && !have.has(p)) missing.add(p);
  }
  return [...missing];
}

/**
 * preflightRunAs: the runAs of each automation names a principal of the team
 * (an error). A person of an automation whose agent uses credentialStrategy
 * CREATOR has a linked forge account (a warning).
 */
export function preflightRunAs(kit: TeamKit, f: FactoryRecord, def: CompiledDefinition, chk: DefinitionCheck): void {
  const autos = (def.automations ?? []).filter((a) => {
    const v = (a.run_as ?? "").trim();
    return v !== "" && v !== "factory";
  });
  if (autos.length === 0) return;
  let users: PrincipalUser[];
  try {
    users = teamUsers(kit);
  } catch (e) {
    chk.skipped.push({ check: "runAs", reason: "could not read the principals: " + (e as Error).message });
    return;
  }
  const foreman = (def.agents ?? []).find((a) => a.type === "FOREMAN")?.name ?? "";
  for (const a of autos) {
    const runAs = a.run_as ?? "";
    let p;
    try {
      p = resolveRunAs(users, f.id, runAs);
    } catch {
      chk.errors.push({
        file: automationFile(a.name),
        field: "runAs",
        message: `runAs ${q(runAs)} names no principal of the team`,
      });
      continue;
    }
    if (p.type !== "user") continue;
    const cfg = (def.agents ?? []).find((g) => g.name === (a.agent || foreman));
    if (effectiveStrategy(cfg?.credential_strategy, def.credentialStrategy) !== CREATOR) continue;
    if (missingForgeLinks(kit, def, p.id).length > 0)
      chk.warnings.push({
        file: automationFile(a.name),
        field: "runAs",
        message: `runAs ${q(runAs)} has no linked forge account, and the agent uses credentialStrategy CREATOR: its runs fail with external_authentication_required`,
      });
  }
}

/** preflightApprovers: each approver names a person of the team (by email) or a service account (by name). */
export function preflightApprovers(kit: TeamKit, def: CompiledDefinition, chk: DefinitionCheck): void {
  if (!def.approvers?.length) return;
  let users: PrincipalUser[];
  try {
    users = teamUsers(kit);
  } catch (e) {
    chk.skipped.push({ check: "approvers", reason: "could not list the users: " + (e as Error).message });
    return;
  }
  for (const a of def.approvers) {
    if (!users.some((u) => isApprover([a], u)))
      chk.errors.push({
        file: "factory.yaml",
        field: "approvers",
        message: `approver ${q(a)} is not a user email or a service account name of the team`,
      });
  }
}

// ---------- models ----------

const DATED = /-\d{8}$/;

/** knownModel: model is in the list of harness type t. A harness without a list accepts any name. */
export function knownModel(t: string, model: string): boolean {
  const list = harnessModels.models(t);
  if (list.length === 0) return true;
  let m = model.trim().toLowerCase();
  if (m.endsWith("[1m]")) m = m.slice(0, -4);
  return [m, m.replace(DATED, "")].some((c) => list.some((k) => k.id.toLowerCase() === c));
}

/**
 * preflightModels: a warning for a model that is not in the model list of its
 * harness. The field is the key that the file sets: the model shorthand or
 * harness.model.
 */
export function preflightModels(def: CompiledDefinition, files: Record<string, string>, chk: DefinitionCheck): void {
  const seen = new Set<string>();
  const check = (file: string, field0: string, t: string, model: string | undefined) => {
    if (!model || knownModel(t, model)) return;
    const content = files[file] ?? "";
    const field =
      field0 === "harness.model" && keyLine(file, content, "model") > 0 && keyLine(file, content, "harness") === 0
        ? "model"
        : field0;
    const key = `${file}\0${field}\0${model}`;
    if (seen.has(key)) return;
    seen.add(key);
    chk.warnings.push({
      file,
      field,
      message: `model ${q(model)} is not in the model list of the ${harnessModels.normalizeType(t)} harness; check the spelling`,
    });
  };
  for (const a of def.agents ?? []) check(agentFile(a.name), "harness.model", a.harness?.type ?? "", a.harness?.model);
  for (const au of def.automations ?? []) {
    const o = au.overrides;
    if (!o) continue;
    if (o.harness) check(automationFile(au.name), "harness.model", o.harness.type ?? "", o.harness.model);
    if (o.model)
      for (const a of def.agents ?? [])
        if (a.name === au.agent) check(automationFile(au.name), "model", a.harness?.type ?? "", o.model);
  }
  for (const sc of def.scorers ?? []) {
    const h = (sc as { harness?: HarnessConfig }).harness;
    if (h) check(scorerFile(sc.slug), "harness.model", h.type ?? "", h.model);
  }
}

// ---------- credentials ----------

/** Secret types that a harness takes as its model credential (runner-api/planner.ts). */
const CREDENTIAL_TYPES: Record<string, string[]> = {
  claude: ["anthropic_api_key", "anthropic_oauth_token"],
  codex: ["openai_api_key"],
};
/** Variables that hold a model credential in the run environment (harness.HasCredentialEnv). */
const CREDENTIAL_ENV: Record<string, string[]> = {
  claude: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"],
  codex: ["OPENAI_API_KEY", "CODEX_API_KEY"],
};

type DefaultCredential =
  { kind: "default" } | { kind: "ambiguous"; scope: string; candidates: string[] } | { kind: "none" };

/** findDefaultCredential without values: the one factory or team secret whose type fits. */
function findDefaultCredential(kit: TeamKit, factoryId: string, types: string[]): DefaultCredential {
  if (types.length === 0) return { kind: "none" };
  const shadowed = new Set<string>();
  for (const [scope, fid] of [
    ["factory", factoryId],
    ["team", ""],
  ] as const) {
    const list = all<{ name: string; type: string }>(
      kit.sql,
      "SELECT name, type FROM secrets WHERE factory_id = ?",
      fid,
    );
    const fits = list.filter((s) => !shadowed.has(s.name) && types.includes(s.type));
    for (const s of list) shadowed.add(s.name);
    if (fits.length === 1) return { kind: "default" };
    if (fits.length > 1) return { kind: "ambiguous", scope, candidates: fits.map((s) => s.name).sort() };
  }
  return { kind: "none" };
}

/** The variable names of the run environment of an agent: its environment and its secrets. */
function runEnvNames(kit: TeamKit, a: AgentConfig): Set<string> {
  const out = new Set<string>(a.secrets ?? []);
  if (a.environment_id) {
    const r = all<{ data: string }>(kit.sql, "SELECT data FROM environments WHERE id = ?", a.environment_id)[0];
    if (r) {
      const vars = (JSON.parse(r.data) as { env_vars?: Record<string, string> }).env_vars ?? {};
      for (const [k, v] of Object.entries(vars)) if (v !== "") out.add(k);
    }
  }
  return out;
}

const typeList = (types: string[]) =>
  types.length < 2 ? types.join("") : types.slice(0, -1).join(", ") + " or " + types[types.length - 1];

/**
 * preflightCredentials: a warning for each agent without harness.auth that
 * gets no credential: no secret of a type its harness takes, or more than one
 * at the same scope. A warning, because a sandbox can hold a credential of its own.
 */
export function preflightCredentials(
  kit: TeamKit,
  f: FactoryRecord,
  def: CompiledDefinition,
  chk: DefinitionCheck,
): void {
  const found = new Map<string, DefaultCredential>();
  for (const a of def.agents ?? []) {
    if (a.harness?.auth) continue;
    const h = String(harnessModels.normalizeType(a.harness?.type ?? ""));
    const env = runEnvNames(kit, a);
    if ((CREDENTIAL_ENV[h] ?? []).some((k) => env.has(k))) continue;
    const types = CREDENTIAL_TYPES[h] ?? [];
    let cred = found.get(h);
    if (!cred) {
      cred = findDefaultCredential(kit, f.id, types);
      found.set(h, cred);
    }
    let message: string;
    if (cred.kind === "default") continue;
    if (cred.kind === "ambiguous")
      message = `the ${h} harness of agent ${q(a.name)} has no harness.auth, and more than one ${cred.scope} secret fits (${cred.candidates.join(", ")}), so runs get no credential; set harness.auth (for example in agentDefaults) to one of them`;
    else if (types.length === 0)
      message = `the ${h} harness of agent ${q(a.name)} has no credential; set harness.auth (for example in agentDefaults) to a managed secret with the model key before the first run`;
    else
      message = `the ${h} harness of agent ${q(a.name)} has no credential; add a secret of type ${typeList(types)} under Secrets, or set harness.auth (for example in agentDefaults) to a managed secret with the model key, before the first run`;
    chk.warnings.push({ file: agentFile(a.name), field: "harness.auth", message });
  }
}

// ---------- workerEnvironment auth ----------

const BUILTIN_HOSTS = new Set(["", "server", "managed"]);
const PROVIDER_HOSTS = new Set(["cloudflare-sandbox", "daytona", "boatdev", "microvm"]);

/**
 * preflightWorkerEnvironment: harness auth workerEnvironment takes the
 * credential from the environment of a development process worker. A microVM
 * worker gives the VM no host variables and a sandbox provider has none, so
 * the effective workerHost of such an agent or automation must name process
 * workers only. Unknown hosts are left to the worker host check.
 */
export function preflightWorkerEnvironment(kit: TeamKit, def: CompiledDefinition, chk: DefinitionCheck): void {
  const uses: { file: string; field: string; host: string }[] = [];
  const agents = def.agents ?? [];
  for (const a of agents)
    if (a.harness?.auth?.source === "workerEnvironment")
      uses.push({ file: agentFile(a.name), field: "harness.auth.source", host: a.worker_host ?? "" });
  const foreman = agents.find((a) => a.type === "FOREMAN")?.name ?? "";
  for (const au of def.automations ?? []) {
    const o = au.overrides;
    if (!o || (o.harness === undefined && o.worker_host === undefined)) continue;
    const base = agents.find((a) => a.name === (au.agent || foreman));
    const h = o.harness ?? base?.harness;
    if (h?.auth?.source !== "workerEnvironment") continue;
    uses.push({
      file: automationFile(au.name),
      field: o.harness ? "harness.auth.source" : "workerHost",
      host: o.worker_host ?? base?.worker_host ?? "",
    });
  }
  if (uses.length === 0) return;
  const workers = all<{ id: string; name: string; kind: string; labels: string }>(
    kit.sql,
    "SELECT id, name, kind, labels FROM workers WHERE revoked_ms IS NULL",
  );
  const providers = registeredProviders();
  for (const u of uses) {
    let microvm: string[] = [];
    if (u.host.startsWith("pool:")) {
      const label = u.host.slice(5);
      microvm = workers
        .filter((w) => w.kind === "microvm" && label in (JSON.parse(w.labels || "{}") as Record<string, string>))
        .map((w) => w.name);
    } else {
      const w = workers.find((x) => x.id === u.host || x.name === u.host);
      if (w) {
        if (w.kind === "microvm") microvm = [w.name];
      } else if (BUILTIN_HOSTS.has(u.host) || PROVIDER_HOSTS.has(u.host) || providers.includes(u.host)) {
        chk.errors.push({
          file: u.file,
          field: u.field,
          message: `auth.source workerEnvironment works only on development process workers, and workerHost ${q(u.host)} is a sandbox provider; use managedSecret`,
        });
        continue;
      }
    }
    if (microvm.length > 0)
      chk.errors.push({
        file: u.file,
        field: u.field,
        message: `auth.source workerEnvironment works only on development process workers, and workerHost ${q(u.host)} runs microVM workers (${microvm.join(", ")}), which give the VM no host variables; use managedSecret`,
      });
  }
}

// ---------- module ----------

export const ops: Record<string, TeamOp> = {
  /** Internal (intake): the principal that a runAs value names. Body {factoryId, runAs}. */
  "principals.resolveRunAs": (kit, c) => {
    const b = bodyObject(c.body);
    const factoryId = str(b, "factoryId");
    if (!factoryId) throw new OfError("invalid_request", "factoryId is required");
    return resolveRunAs(teamUsers(kit), factoryId, str(b, "runAs"));
  },
};

export function setup(_kit: TeamKit): void {
  registerPreflight("webhooks", (kit, f, def, _files, chk) => preflightWebhooks(kit, f, def, chk));
  registerPreflight("webhook filters", (kit, f, def, _files, chk) => preflightWebhookFilters(kit, f, def, chk));
  registerPreflight("runAs", (kit, f, def, _files, chk) => preflightRunAs(kit, f, def, chk));
  registerPreflight("approvers", (kit, _f, def, _files, chk) => preflightApprovers(kit, def, chk));
  registerPreflight("models", (_kit, _f, def, files, chk) => preflightModels(def, files, chk));
  registerPreflight("credentials", (kit, f, def, _files, chk) => preflightCredentials(kit, f, def, chk));
  registerPreflight("worker environment", (kit, _f, def, _files, chk) => preflightWorkerEnvironment(kit, def, chk));
}
