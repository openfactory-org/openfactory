// TeamDO module: definition preflight checks against the team that the
// factories module does not own. Port of internal/engine/preflight.go
// (webhooks, webhook filters, approvers, models, credentials) and
// internal/engine/principal.go (runAs). Adds the check that workerEnvironment
// auth targets a development process worker.
//
// preflightMachines keeps only the registryCredentialSecretName warning: the
// compiler rejects macOS and Windows machines. preflightNameFilters resolves
// filter names with the name resolver that wiring.ts sets (setNameResolver).
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
import { listIntegrations } from "./integrations";
import {
  agentFile,
  automationFile,
  registerPreflight,
  scorerFile,
  webhookFile,
  type DefinitionCheck,
  type FactoryRecord,
} from "./factories";
import { getSecretValue } from "./secrets";
import { isProviderHost } from "./workers";
import type { TeamKit, TeamOp } from "./index";

export const migrations: readonly Migration[] = [];

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
      } else if (isProviderHost(u.host, providers)) {
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

// ---------- machines ----------

/**
 * preflightMachines: a machine that sets registryCredentialSecretName. Only
 * microVM workers pull images at run time and use it; the sandbox providers
 * ignore it. A named machine is checked in its file, an inline machine of an
 * agent in the agent file.
 */
export function preflightMachines(def: CompiledDefinition, chk: DefinitionCheck): void {
  const check = (file: string, field: string, m: { registryCredentialSecretName?: string }) => {
    const name = m.registryCredentialSecretName;
    if (name)
      chk.warnings.push({
        file,
        field: field + ".registryCredentialSecretName",
        message: `registryCredentialSecretName ${q(name)} has an effect only on microVM workers: the sandbox providers ignore it and use the image of their snapshot or deploy`,
      });
  };
  const machines = def.machines ?? {};
  for (const n of Object.keys(machines).sort()) check(`machines/${n}.yaml`, "platform", machines[n]!);
  for (const a of def.agents ?? []) {
    if (a.machine && !(a.machine.name in machines)) check(agentFile(a.name), "machine", a.machine);
  }
}

// ---------- name filters ----------

/** Resolves names in automation filters to provider IDs (core.NameResolver). */
export interface FilterNameResolver {
  resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
    scope?: Record<string, string[]>,
  ): Promise<Record<string, string[]>>;
}

let nameResolverFor: ((kit: TeamKit) => FilterNameResolver | undefined) | undefined;

/**
 * SetNameResolver: the resolver that maps the names in automation filters to
 * provider IDs. Without one, the preflight reports name filters as warnings
 * and the names are compared on each event. Returns the previous one.
 */
export function setNameResolver(
  fn: ((kit: TeamKit) => FilterNameResolver | undefined) | undefined,
): ((kit: TeamKit) => FilterNameResolver | undefined) | undefined {
  const prev = nameResolverFor;
  nameResolverFor = fn;
  return prev;
}

/**
 * The name filters of each provider. The order matters: a key is resolved
 * after the keys that give it scope (teams before states and issues).
 */
const NAME_ALIASES: Record<string, [alias: string, native: string][]> = {
  linear: [
    ["teams", "team_ids"],
    ["projects", "project_ids"],
    ["states", "state_ids"],
    ["issues", "issue_ids"],
  ],
  slack: [
    ["channels", "channel_ids"],
    ["users", "user_ids"],
    ["itemUsers", "item_user_ids"],
  ],
  jira: [
    ["projects", "project_keys"],
    ["statuses", "status_ids"],
  ],
  // Teams triggers name the Graph team in team_ids; channel and user names are looked up in that team.
  microsoft_teams: [
    ["channels", "channel_ids"],
    ["users", "user_ids"],
  ],
};

/** Native filter keys that limit the lookup of other names (the team of a project, state, issue or channel). */
const SCOPE_KEYS = ["team_ids"];

/** A plain list of names (or issue numbers) of a filter, or undefined. */
function filterNames(v: unknown): string[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out: string[] = [];
  for (const x of v) {
    if (typeof x === "string") out.push(x);
    else if (typeof x === "number") out.push(String(Math.trunc(x)));
    else return undefined;
  }
  return out;
}

/** The IDs of a native filter key: a plain list, or the "in" list of a matcher object. */
function scopeIds(v: unknown): string[] | undefined {
  const ids = filterNames(v);
  if (ids) return ids;
  if (v && typeof v === "object" && "in" in v) return filterNames((v as { in: unknown }).in);
  return undefined;
}

function singular(alias: string): string {
  switch (alias) {
    case "statuses":
      return "status";
    case "itemUsers":
      return "user";
    case "issues":
      return "issue";
  }
  return alias.replace(/s$/, "");
}

/**
 * preflightNameFilters (engine/preflight.go): resolves the names of
 * automation filters to provider IDs. A name that matches no object or more
 * than one is an error. A resolved filter replaces its trigger filter in def
 * (applyResolvedFilters), so an apply stores the IDs. Names that cannot be
 * checked now stay as written, with a warning.
 */
export async function preflightNameFilters(kit: TeamKit, def: CompiledDefinition, chk: DefinitionCheck): Promise<void> {
  const resolver = nameResolverFor?.(kit);
  let connected: Set<string> | undefined;
  for (const au of def.automations ?? []) {
    const file = automationFile(au.name);
    for (const [ti, tr] of (au.triggers ?? []).entries()) {
      const aliases = NAME_ALIASES[tr.provider];
      let m: Record<string, unknown> | undefined;
      let raw = tr.filter;
      if (typeof raw === "string") {
        try {
          raw = JSON.parse(raw) as unknown;
        } catch {
          continue;
        }
      }
      if (raw && typeof raw === "object" && !Array.isArray(raw)) m = { ...(raw as Record<string, unknown>) };
      if (!aliases || !m) continue;
      const scope: Record<string, string[]> = {};
      for (const k of SCOPE_KEYS) {
        const ids = k in m ? scopeIds(m[k]) : undefined;
        if (ids?.length) scope[k] = ids;
      }
      let rewrote = false;
      for (const [alias, native] of aliases) {
        if (!(alias in m)) continue;
        const names = filterNames(m[alias]);
        if (!names?.length) continue;
        const field = `triggers[${ti}].filter.${alias}`;
        connected ??= new Set(listIntegrations(kit).map((i) => i.provider.toLowerCase()));
        let reason = "";
        if (!connected.has(tr.provider)) reason = `the ${tr.provider} integration is not connected`;
        else if (!resolver) reason = `this server cannot list ${tr.provider} objects`;
        if (reason) {
          chk.warnings.push({
            file,
            field,
            message: `names are not checked now, ${reason}; they are compared with the current names on each event`,
          });
          continue;
        }
        let ids: Record<string, string[]>;
        try {
          ids = await resolver!.resolveNames(kit.teamId(), tr.provider, alias, names, { ...scope });
        } catch (e) {
          chk.warnings.push({
            file,
            field,
            message: `names are not checked now: ${(e as Error).message}; they are compared with the current names on each event`,
          });
          continue;
        }
        const resolved: string[] = [];
        let good = true;
        for (const n of names) {
          const got = ids[n] ?? [];
          if (got.length === 1) resolved.push(got[0]!);
          else {
            good = false;
            chk.errors.push({
              file,
              field,
              message:
                got.length === 0
                  ? `no ${tr.provider} ${singular(alias)} is named ${q(n)}`
                  : `${got.length} ${tr.provider} ${alias} are named ${q(n)}; use the ID in ${native}`,
            });
          }
        }
        if (!good) continue;
        scope[native] = resolved;
        if (native in m) continue; // both keys set: keep the names as written
        m[native] = resolved;
        delete m[alias];
        rewrote = true;
      }
      if (rewrote) tr.filter = m;
    }
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
  registerPreflight("machines", (_kit, _f, def, _files, chk) => preflightMachines(def, chk));
  registerPreflight("name filters", (kit, _f, def, _files, chk) => preflightNameFilters(kit, def, chk));
}
