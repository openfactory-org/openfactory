// The session planner in the Worker (03 §8.2). Port of internal/engine
// planner.go, runner_plan.go, credentials.go, defaultauth.go, redact.go and
// the runnerContent of internal/server/runnermode.go. Stateless: inputs come
// from the run's WorkDO (work item, repository refs), TeamDO (config snapshot,
// environments, secrets, MCP registry) and the forge resolver. It never runs
// inside a DO transaction.
import {
  builtinSkillsForRun,
  composeSystemPrompt,
  doName,
  partitionOf,
  invalid,
  isOfError,
  METADATA_ENV_PREFIX,
  metadataEnv,
  mergeSkills,
  newId,
  OfError,
  type Skill,
} from "@openfactory/core";
import type { CallMeta, Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { requiredHosts } from "../engine/hosts";
import { parseDuration } from "../engine/sessions";
import type { Run, RunSession, WorkItem } from "../engine/types";
import type { Forge, ForgeResolver } from "../forges/forge";
import { resolverFor } from "../forges/env";
import { expandWith } from "../secrets/expand";
import type { SecretsBuilder, StartConfig, StartConfigBuilder } from "./startconfig";

export const ORCHESTRATION_SERVER = "factory";
export const RUNNER_SETUP_TIMEOUT_S = 1800;
export const RUNNER_EVENT_BATCH_BYTES = 1 << 20;
/** Forge app tokens last one hour; the runner asks again after 50 minutes. */
export const GIT_CREDENTIAL_TTL_MS = 50 * 60_000;
const SELF_IMPROVEMENT = "self_improvement";

export const CREDENTIAL_EXECUTOR = "EXECUTOR";
export const CREDENTIAL_CREATOR = "CREATOR";
export const CREDENTIAL_MANAGED_SECRET = "managedSecret";
export const CREDENTIAL_WORKER_ENVIRONMENT = "workerEnvironment";
export const CREDENTIAL_DEFAULT = "default";
export const CREDENTIAL_NONE = "none";

export interface HarnessCredential {
  source: string;
  secret?: string;
  scope?: string;
  type?: string;
  reason?: string;
  candidates?: string[];
}

export interface McpServer {
  managedId?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
}

export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
}
export interface RepoAtCommit {
  codeForge: string;
  owner: string;
  repo: string;
  ref: string;
}
export interface RepoStart extends RepoAtCommit {
  path?: string;
  resolved?: boolean;
}
export interface Checkout {
  forge: string;
  owner: string;
  name: string;
  path: string;
  ref: string;
  cloneUrl: string;
}

/** Inputs that the run's WorkDO gives the planner. */
export interface PlannerState {
  workItem?: WorkItem;
  repoRefs: RepoAtCommit[];
  startCommits?: RepoStart[];
}

export interface Snapshot {
  factory: { id: string; name?: string };
  runtime: {
    repositories?: RepoRef[] | null;
    skills?: Skill[];
    credential_strategy?: string;
    cloud_providers?: {
      gcp?: {
        projectNumber?: string;
        workloadIdentityFederationPoolId?: string;
        workloadIdentityFederationProviderId?: string;
        serviceAccountEmail?: string;
      };
      audiences?: string[];
    };
  };
  agents: { name: string; type: string; description?: string; spawnable_by?: string[] | null }[];
  integrations?: Record<string, unknown>;
}

export interface Environment {
  id: string;
  repos: string[];
  setup_commands?: string[];
  env_vars?: Record<string, string>;
}

/** The planner's view of the platform. Tests replace it. */
export interface PlannerDeps {
  snapshot(factoryId: string): Promise<Snapshot>;
  state(run: Run): Promise<PlannerState>;
  recordStartCommits(run: Run, list: RepoStart[]): Promise<void>;
  environment(id: string): Promise<Environment | undefined>;
  /** A secret value; factoryId "" reads the team secret. undefined when missing. */
  secret(factoryId: string, name: string): Promise<string | undefined>;
  /** Secrets of one scope with their types. */
  listSecrets(factoryId: string): Promise<{ name: string; type: string; factory_id?: string }[]>;
  resolveManagedMcp(run: Run, managedId: string): Promise<McpServer>;
  forges: ForgeResolver & {
    forgeForUser?(
      l: Parameters<ForgeResolver["forge"]>[0],
      userId: string,
    ): Promise<{ forge: Forge; userToken: boolean }>;
  };
  sandboxUrl: string;
  now(): number;
}

// ---------- pure helpers (ported verbatim) ----------

const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** ParseRepoSpec: "forge:owner/name", "owner/name" (GitHub) or a forge URL. */
export function parseRepoSpec(spec: string): RepoRef {
  let s = spec.trim();
  let forge = "GITHUB";
  const i = s.indexOf(":");
  if (i > 0 && !s.slice(0, i).includes("/")) {
    const f = s.slice(0, i).replaceAll("-", "_").toUpperCase();
    if (f === "HTTPS" || f === "HTTP") {
      try {
        const u = new URL(s);
        s = u.pathname.replace(/^\//, "");
        if (u.host.includes("gitlab")) forge = "GITLAB";
        else if (u.host.includes("dev.azure.com") || u.host.includes("visualstudio.com")) forge = "AZURE_DEVOPS";
      } catch {
        /* keep s */
      }
    } else {
      forge = f;
      s = s.slice(i + 1);
    }
  }
  s = s.replace(/^\/+|\/+$/g, "");
  if (s.endsWith(".git")) s = s.slice(0, -4);
  const j = s.lastIndexOf("/");
  if (j <= 0 || j === s.length - 1) throw invalid(`invalid repository ${JSON.stringify(s)} (want forge:owner/name)`);
  return { codeForge: forge, owner: s.slice(0, j), name: s.slice(j + 1) };
}

export function isCommitSha(s: string): boolean {
  return /^[0-9a-f]{40}$/.test(s);
}

/** EffectiveCredentialStrategy: the agent's, else the factory's, else EXECUTOR. */
export function effectiveCredentialStrategy(agent: string | undefined, factory: string | undefined): string {
  let s = (agent ?? "").trim().toUpperCase();
  if (!s) s = (factory ?? "").trim().toUpperCase();
  return s === CREDENTIAL_CREATOR ? CREDENTIAL_CREATOR : CREDENTIAL_EXECUTOR;
}

/** credentialScope: the repositories of a run by forge, as "owner/name". */
export function credentialScope(refs: RepoRef[], start: RepoAtCommit[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  const seen = new Set<string>();
  const add = (forge: string, owner: string, name: string) => {
    const f = forge.toUpperCase();
    const full = `${owner}/${name}`;
    const k = `${f}|${full.toLowerCase()}`;
    if (seen.has(k)) return;
    seen.add(k);
    (out[f] ??= []).push(full);
  };
  for (const r of refs) add(r.codeForge, r.owner, r.name);
  for (const r of start) add(r.codeForge, r.owner, r.repo);
  return out;
}

/** splitCloneURL: the URL without user info, and the user info. */
export function splitCloneUrl(raw: string): { clean: string; username?: string; password?: string } {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    if (raw.includes("@")) throw new Error("clone URL cannot be parsed and may hold credentials");
    return { clean: raw };
  }
  const username = u.username ? decodeURIComponent(u.username) : undefined;
  const password = u.password ? decodeURIComponent(u.password) : undefined;
  u.username = "";
  u.password = "";
  return {
    clean: u.toString(),
    ...(username !== undefined ? { username } : {}),
    ...(password !== undefined ? { password } : {}),
  };
}

/** applyRepoRefs: pins checkouts to the starting commits; adds missing repositories. */
export async function applyRepoRefs(
  repos: Checkout[],
  refs: RepoAtCommit[],
  cloneUrl: (forge: string, owner: string, name: string) => Promise<string>,
): Promise<Checkout[]> {
  for (const ref of refs) {
    const forge = ref.codeForge.toUpperCase();
    let found = false;
    for (const r of repos) {
      if (
        r.forge.toUpperCase() === forge &&
        r.owner.toLowerCase() === ref.owner.toLowerCase() &&
        r.name.toLowerCase() === ref.repo.toLowerCase()
      ) {
        r.ref = ref.ref;
        found = true;
      }
    }
    if (found) continue;
    let u: string;
    try {
      u = await cloneUrl(forge, ref.owner, ref.repo);
    } catch (e) {
      throw wrap(e, `starting repository ${ref.owner}/${ref.repo}`);
    }
    repos.push({ forge, owner: ref.owner, name: ref.repo, cloneUrl: u, ref: ref.ref, path: ref.repo });
  }
  return repos;
}

const CREDENTIAL_TYPES: Record<string, string[]> = {
  claude: ["anthropic_api_key", "anthropic_oauth_token"],
  codex: ["openai_api_key"],
};
const CREDENTIAL_ENV: Record<string, string[]> = {
  claude: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"],
  codex: ["OPENAI_API_KEY", "CODEX_API_KEY"],
};

const harnessName = (h: string) => (h || "claude").trim().toLowerCase();

/** harness.HasCredentialEnv */
export function hasCredentialEnv(h: string, env: Record<string, string>): boolean {
  return (CREDENTIAL_ENV[harnessName(h)] ?? []).some((k) => !!env[k]);
}

function looksOpenAI(model: string): boolean {
  const m = model.toLowerCase();
  return ["gpt", "o1", "o3", "o4", "openai/", "codex"].some((p) => m.startsWith(p));
}

function openAIEnv(h: string, v: string): Record<string, string> {
  return harnessName(h) === "codex" ? { OPENAI_API_KEY: v, CODEX_API_KEY: v } : { OPENAI_API_KEY: v };
}

/** HarnessAuthEnv: a credential as harness environment variables (model provider keys only; object storage uses R2 over S3 with SigV4). */
export function harnessAuthEnv(h: string, model: string, type: string, value: string): Record<string, string> {
  const v = value.trim();
  switch (type) {
    case "anthropic_api_key":
      return { ANTHROPIC_API_KEY: v };
    case "anthropic_oauth_token":
      return harnessName(h) === "claude" ? { CLAUDE_CODE_OAUTH_TOKEN: v } : {};
    case "openai_api_key":
      return openAIEnv(h, v);
  }
  switch (harnessName(h)) {
    case "codex":
      return openAIEnv(h, v);
    case "claude":
      return { ANTHROPIC_API_KEY: v };
  }
  return looksOpenAI(model) ? openAIEnv(h, v) : { ANTHROPIC_API_KEY: v };
}

/** headerSecrets: a header value and, for "Bearer <token>", the token alone. */
export function headerSecrets(v: string): string[] {
  const out = [v];
  const i = v.indexOf(" ");
  if (i > 0 && v.slice(i + 1)) {
    if (["bearer", "basic", "token"].includes(v.slice(0, i).toLowerCase())) out.push(v.slice(i + 1).trim());
  }
  return out;
}

/** redactList: distinct values of at least 4 characters, longest first. */
export function redactList(values: string[]): string[] {
  const set = new Set<string>();
  for (const v of values) if (v && v.length >= 4) set.add(v);
  return [...set].sort((a, b) => b.length - a.length || (a < b ? -1 : a > b ? 1 : 0));
}

function wrap(e: unknown, ctx: string): Error {
  if (isOfError(e)) return new OfError(e.code, `${ctx}: ${e.detail}`, e.reason ? { reason: e.reason } : undefined);
  return new Error(`${ctx}: ${e instanceof Error ? e.message : String(e)}`);
}

// ---------- the planner ----------

export interface RunnerPlan {
  env: Record<string, string>;
  secretEnv: Record<string, string>;
  harnessAuth: Record<string, string>;
  credential: HarnessCredential;
  repos: Checkout[];
  workDir: string;
  skills: Skill[];
  systemPrompt: string;
  mcpServers: Record<string, McpServer>;
  sessionTokenServers: Set<string>;
  setupCommands: string[];
  envSetupCommands: string[];
  cloud?: { gcp?: { audience: string; project_number: string; service_account_email?: string } };
}

type Cfg = Run["config"] & {
  harness: Run["config"]["harness"] & { auth?: { source?: string; secretName?: string }; reasoningLevel?: string };
  mcp_servers?: Record<string, McpServer>;
  skills?: Skill[];
};

export class Planner {
  constructor(private readonly d: PlannerDeps) {}

  private cfg(r: Run): Cfg {
    return r.config as Cfg;
  }

  private async environment(r: Run): Promise<Environment | undefined> {
    const id = r.config.environment_id;
    if (!id) return undefined;
    const e = await this.d.environment(id);
    if (!e) throw new OfError("resource_not_found", `environment ${JSON.stringify(id)}: environment ${id} not found`);
    return e;
  }

  /** runEnv: environment variables, agent secrets, run metadata and the OF_* run identity. */
  async runEnv(r: Run, envr?: Environment): Promise<Record<string, string>> {
    const env: Record<string, string> = {};
    for (const [k, v] of Object.entries(envr?.env_vars ?? {})) {
      if (k.startsWith(METADATA_ENV_PREFIX)) continue;
      env[k] = v;
    }
    for (const name of r.config.secrets ?? []) {
      if (!ENV_NAME_RE.test(name) || name.startsWith(METADATA_ENV_PREFIX)) continue;
      const v = await this.d.secret(r.factory_id, name);
      if (v === undefined)
        throw new OfError("resource_not_found", `secret ${JSON.stringify(name)}: secret ${name} not found`);
      env[name] = v;
    }
    Object.assign(env, metadataEnv(r.metadata));
    env.OF_RUN_ID = r.id;
    env.OF_FACTORY_ID = r.factory_id;
    if (r.work_item_id) env.OF_WORK_ITEM_ID = r.work_item_id;
    if (r.parent_run_id) env.OF_PARENT_RUN_ID = r.parent_run_id;
    if (r.agent_name) env.OF_AGENT_NAME = r.agent_name;
    if (this.d.sandboxUrl) env.OF_PUBLIC_URL = this.d.sandboxUrl;
    return env;
  }

  /** credentialUser: the person whose forge credential a CREATOR run uses, else "". */
  credentialUser(r: Run, snap: Snapshot, st: PlannerState): string {
    if (
      effectiveCredentialStrategy(r.config.credential_strategy, snap.runtime.credential_strategy) !== CREDENTIAL_CREATOR
    )
      return "";
    if (r.run_as) return r.run_as.kind === "user" && r.run_as.id ? r.run_as.id : "";
    return st.workItem?.creator_id ?? "";
  }

  private async forgeFor(
    r: Run,
    codeForge: string,
    repo: string,
    userId: string,
    scope: string[] | undefined,
  ): Promise<Forge> {
    const l = { teamId: r.team_id, factoryId: r.factory_id, codeForge, repo, ...(scope ? { scope } : {}) };
    if (userId && this.d.forges.forgeForUser) {
      const res = await this.d.forges.forgeForUser(l, userId);
      if (res.userToken) return res.forge;
    }
    return this.d.forges.forge(l);
  }

  /** checkouts: the repositories of a run, each with a clone URL that holds a credential. */
  async checkouts(r: Run, snap: Snapshot, st: PlannerState, envr?: Environment): Promise<Checkout[]> {
    const refs: RepoRef[] = envr ? envr.repos.map(parseRepoSpec) : (snap.runtime.repositories ?? []);
    const user = this.credentialUser(r, snap, st);
    const scope = credentialScope(refs, st.repoRefs);
    const repos: Checkout[] = [];
    for (const ref of refs) {
      const forge = ref.codeForge.toUpperCase();
      const full = `${ref.owner}/${ref.name}`;
      let url: string;
      try {
        const f = await this.forgeFor(r, forge, full, user, scope[forge]);
        url = await f.cloneURL(ref.owner, ref.name);
      } catch (e) {
        throw wrap(e, `repository ${full}`);
      }
      repos.push({ forge, owner: ref.owner, name: ref.name, path: ref.name, ref: "", cloneUrl: url });
    }
    if (st.repoRefs.length > 0) {
      await applyRepoRefs(repos, st.repoRefs, async (forge, owner, name) => {
        const f = await this.forgeFor(r, forge, `${owner}/${name}`, user, scope[forge.toUpperCase()]);
        return f.cloneURL(owner, name);
      });
    }
    return repos;
  }

  /** runSkills: agent, factory, then the built-in skills of the run. */
  runSkills(r: Run, snap: Snapshot, wi: WorkItem | undefined): Skill[] {
    const integrations = Object.keys(snap.integrations ?? {}).sort();
    const forges = (snap.runtime.repositories ?? []).map((x) => x.codeForge);
    const self =
      r.triggered_by === SELF_IMPROVEMENT ||
      (wi?.source as { provider?: string } | undefined)?.provider === SELF_IMPROVEMENT;
    const builtin = builtinSkillsForRun({ agentType: r.config.type, forges, integrations, selfImprovement: self });
    return mergeSkills(this.cfg(r).skills ?? [], snap.runtime.skills ?? [], [], builtin);
  }

  private async expand(r: Run, m: Record<string, string> | undefined): Promise<Record<string, string> | undefined> {
    if (!m) return undefined;
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(m)) {
      out[k] = v.includes("{{")
        ? await expandWith(v, async (n) => (await this.d.secret(r.factory_id, n)) ?? (await this.d.secret("", n)))
        : v;
    }
    return out;
  }

  /** mcpServers: managedId resolved, {{SECRET}} expanded, the orchestration server injected. */
  async mcpServers(r: Run): Promise<Record<string, McpServer>> {
    const out: Record<string, McpServer> = {};
    const base = this.d.sandboxUrl;
    for (const [name, s0] of Object.entries(this.cfg(r).mcp_servers ?? {})) {
      if (name === ORCHESTRATION_SERVER) continue;
      let s: McpServer = { ...s0 };
      if (s.managedId) {
        let got: McpServer;
        try {
          got = await this.d.resolveManagedMcp(r, s.managedId);
        } catch (e) {
          throw wrap(e, `mcp server ${JSON.stringify(name)}: managedId ${JSON.stringify(s.managedId)}`);
        }
        if (!got.url && !got.command)
          throw invalid(
            `mcp server ${JSON.stringify(name)}: managedId ${JSON.stringify(s.managedId)} has no url or command`,
          );
        let agentHeaders = s.headers;
        if (base && got.url?.startsWith(base + "/mcp/")) {
          agentHeaders = Object.fromEntries(
            Object.entries(s.headers ?? {}).filter(([k]) => k.toLowerCase() !== "authorization"),
          );
        }
        const env = { ...(got.env ?? {}), ...(s.env ?? {}) };
        const headers = { ...(got.headers ?? {}), ...(agentHeaders ?? {}) };
        s = {
          ...got,
          ...(Object.keys(env).length ? { env } : {}),
          ...(Object.keys(headers).length ? { headers } : {}),
        };
        delete s.managedId;
        if (!Object.keys(env).length) delete s.env;
        if (!Object.keys(headers).length) delete s.headers;
      }
      if (s.args) s.args = [...s.args];
      try {
        const env = await this.expand(r, s.env);
        if (env) s.env = env;
      } catch (e) {
        throw wrap(e, `mcp server ${JSON.stringify(name)} env`);
      }
      try {
        const headers = await this.expand(r, s.headers);
        if (headers) s.headers = headers;
      } catch (e) {
        throw wrap(e, `mcp server ${JSON.stringify(name)} headers`);
      }
      out[name] = s;
    }
    if (base) out[ORCHESTRATION_SERVER] = { url: `${base}/mcp/run/${r.id}` };
    return out;
  }

  /** sessionTokenServers: control-plane servers lose their Authorization header. */
  sessionTokenServers(mcp: Record<string, McpServer>): Set<string> {
    const out = new Set<string>();
    const base = this.d.sandboxUrl;
    if (!base) return out;
    for (const [name, srv] of Object.entries(mcp)) {
      if (!srv.url?.startsWith(base + "/mcp/")) continue;
      srv.headers = Object.fromEntries(
        Object.entries(srv.headers ?? {}).filter(([k]) => k.toLowerCase() !== "authorization"),
      );
      out.add(name);
    }
    return out;
  }

  /** findDefaultCredential: the one factory or team secret whose type the harness accepts. */
  async findDefaultCredential(r: Run): Promise<HarnessCredential> {
    const none: HarnessCredential = { source: CREDENTIAL_NONE, reason: "no_secret" };
    const types = CREDENTIAL_TYPES[harnessName(r.config.harness?.type ?? "")] ?? [];
    if (types.length === 0) return none;
    const scopes: [string, string][] = r.factory_id
      ? [
          ["factory", r.factory_id],
          ["team", ""],
        ]
      : [["team", ""]];
    const shadowed = new Set<string>();
    for (const [scopeName, fid] of scopes) {
      const list = (await this.d.listSecrets(fid)).filter((s) => (s.factory_id ?? "") === fid);
      const fits = list.filter((s) => !shadowed.has(s.name) && types.includes(s.type));
      for (const s of list) shadowed.add(s.name);
      if (fits.length === 1)
        return { source: CREDENTIAL_DEFAULT, secret: fits[0]!.name, scope: scopeName, type: fits[0]!.type };
      if (fits.length > 1)
        return {
          source: CREDENTIAL_NONE,
          reason: "ambiguous",
          scope: scopeName,
          candidates: fits.map((s) => s.name).sort(),
        };
    }
    return none;
  }

  private async secretAuth(
    r: Run,
    factoryId: string,
    name: string,
  ): Promise<{ auth: Record<string, string>; type: string }> {
    const v = await this.d.secret(factoryId, name);
    if (v === undefined)
      throw new OfError("resource_not_found", `harness auth secret ${JSON.stringify(name)}: secret ${name} not found`);
    const list = await this.d.listSecrets(factoryId);
    const type = list.find((s) => s.name === name && (s.factory_id ?? "") === factoryId)?.type ?? "generic";
    return { auth: harnessAuthEnv(r.config.harness?.type ?? "", r.config.harness?.model ?? "", type, v), type };
  }

  /** resolveHarnessAuth: harness.auth, the run environment, the default credential, or nothing. */
  async resolveHarnessAuth(
    r: Run,
    env: Record<string, string>,
  ): Promise<{ auth: Record<string, string>; credential: HarnessCredential }> {
    const a = this.cfg(r).harness?.auth;
    if (a) {
      if (a.source === CREDENTIAL_WORKER_ENVIRONMENT)
        return { auth: {}, credential: { source: CREDENTIAL_WORKER_ENVIRONMENT } };
      if (!a.secretName || (a.source && a.source !== CREDENTIAL_MANAGED_SECRET))
        return { auth: {}, credential: { source: CREDENTIAL_NONE } };
      const { auth, type } = await this.secretAuth(r, r.factory_id, a.secretName);
      return { auth, credential: { source: CREDENTIAL_MANAGED_SECRET, secret: a.secretName, type } };
    }
    if (hasCredentialEnv(r.config.harness?.type ?? "", env))
      return { auth: {}, credential: { source: CREDENTIAL_NONE, reason: "run_environment" } };
    const cred = await this.findDefaultCredential(r);
    if (cred.source !== CREDENTIAL_DEFAULT) return { auth: {}, credential: cred };
    const { auth } = await this.secretAuth(r, cred.scope === "factory" ? r.factory_id : "", cred.secret!);
    return { auth, credential: cred };
  }

  /** RunnerPlan: the content of a runner session. */
  async plan(r: Run): Promise<RunnerPlan> {
    const [snap, st] = await Promise.all([this.d.snapshot(r.factory_id), this.d.state(r)]);
    const envr = await this.environment(r);
    const env = await this.runEnv(r, envr);
    const { auth, credential } = await this.resolveHarnessAuth(r, env);
    const mcp = await this.mcpServers(r);
    const sessionServers = this.sessionTokenServers(mcp);
    const repos = await this.checkouts(r, snap, st, envr);
    if (repos.length > 0 && !st.startCommits) {
      await this.d.recordStartCommits(
        r,
        repos.map((c) => ({
          codeForge: c.forge,
          owner: c.owner,
          repo: c.name,
          path: c.path,
          ref: c.ref,
          resolved: isCommitSha(c.ref),
        })),
      );
    }
    const skills = this.runSkills(r, snap, st.workItem);
    const cfg = { ...r.config, skills } as unknown as Parameters<typeof composeSystemPrompt>[0];
    const systemPrompt = composeSystemPrompt(
      cfg,
      (st.workItem ?? null) as unknown as Parameters<typeof composeSystemPrompt>[1],
      r as unknown as Parameters<typeof composeSystemPrompt>[2],
      (snap.agents ?? []).map((a) => ({ id: "", ...a, description: a.description ?? "" })) as unknown as Parameters<
        typeof composeSystemPrompt
      >[3],
    );
    const secretNames = new Set(r.config.secrets ?? []);
    const plain: Record<string, string> = {};
    const secretEnv: Record<string, string> = {};
    for (const [k, v] of Object.entries(env)) (secretNames.has(k) ? secretEnv : plain)[k] = v;
    const paths = envr ? [] : (snap.runtime.repositories ?? []).map((x) => x.name);
    const g = snap.runtime.cloud_providers?.gcp;
    return {
      env: plain,
      secretEnv,
      harnessAuth: auth,
      credential,
      repos,
      workDir: paths.length === 1 ? paths[0]! : "",
      skills,
      systemPrompt,
      mcpServers: mcp,
      sessionTokenServers: sessionServers,
      setupCommands: [...(r.config.machine?.setupCommands ?? [])],
      envSetupCommands: [...(envr?.setup_commands ?? [])],
      ...(g?.projectNumber
        ? {
            cloud: {
              gcp: {
                audience: `//iam.googleapis.com/projects/${g.projectNumber}/locations/global/workloadIdentityPools/${g.workloadIdentityFederationPoolId ?? ""}/providers/${g.workloadIdentityFederationProviderId ?? ""}`,
                project_number: g.projectNumber,
                ...(g.serviceAccountEmail ? { service_account_email: g.serviceAccountEmail } : {}),
              },
            },
          }
        : {}),
    };
  }

  /** RunnerGitCredential: the credential of one repository of the run, else forbidden. */
  async gitCredential(
    r: Run,
    host: string,
    path: string,
  ): Promise<{ username: string; password: string; expiresAt: string }> {
    const [snap, st] = await Promise.all([this.d.snapshot(r.factory_id), this.d.state(r)]);
    const envr = await this.environment(r);
    const repos = await this.checkouts(r, snap, st, envr);
    const want = path.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
    for (const c of repos) {
      let u: URL;
      try {
        u = new URL(c.cloneUrl);
      } catch {
        continue;
      }
      const got = u.pathname.replace(/^\//, "").replace(/\.git$/, "");
      if (u.host.toLowerCase() !== host.toLowerCase() || got !== want) continue;
      const pw = u.password ? decodeURIComponent(u.password) : "";
      if (!pw) throw new OfError("not_authorized", `the forge gave no credential for ${got}`);
      return {
        username: decodeURIComponent(u.username),
        password: pw,
        expiresAt: new Date(this.d.now() + GIT_CREDENTIAL_TTL_MS).toISOString(),
      };
    }
    throw new OfError("not_authorized", `${host}/${want} is not a repository of this run`);
  }

  /** RedactFor: the secret values of a run (agent secrets, harness credentials, MCP servers). */
  async redactValues(r: Run): Promise<string[]> {
    const out: string[] = [];
    let env: Record<string, string> = {};
    try {
      env = await this.runEnv(r, await this.environment(r));
      for (const n of r.config.secrets ?? []) if (env[n]) out.push(env[n]!);
    } catch {
      /* best effort */
    }
    try {
      const { auth } = await this.resolveHarnessAuth(r, env);
      for (const [k, v] of Object.entries(auth)) if (!k.toUpperCase().endsWith("_BASE_URL")) out.push(v);
    } catch {
      /* best effort */
    }
    try {
      for (const s of Object.values(await this.mcpServers(r))) {
        for (const v of Object.values(s.headers ?? {})) out.push(...headerSecrets(v));
        for (const v of Object.values(s.env ?? {})) out.push(v);
      }
    } catch {
      /* best effort */
    }
    return redactList(out);
  }
}

// ---------- start config and secrets (runnerContent) ----------

function idleSeconds(env: Env, minutes: number | undefined, set: boolean | undefined): number {
  const e = env as unknown as Record<string, string | undefined>;
  const def = parseDuration(e.OF_IDLE_TIMEOUT_DEFAULT, 10 * 60_000);
  const max = parseDuration(e.OF_IDLE_TIMEOUT_MAX, 60 * 60_000);
  let d = def;
  if (set || (minutes ?? 0) > 0) d = (minutes ?? 0) * 60_000;
  if (d > max) d = max;
  return Math.floor(d / 1000);
}

/** StartConfig from a plan (runnermode.go StartConfig, plus the network policy of the plan). */
export function startConfigOf(env: Env, run: Run, session: RunSession, plan: RunnerPlan): StartConfig {
  const cfg = run.config as Cfg;
  const base = (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  const mcp: Record<string, { url?: string; auth?: string; command?: string; args?: string[] }> = {};
  for (const [name, srv] of Object.entries(plan.mcpServers)) {
    if (srv.url && plan.sessionTokenServers.has(name)) mcp[name] = { url: srv.url, auth: "session_token" };
    else if (srv.url) mcp[name] = { url: srv.url };
    else if (srv.command) mcp[name] = { command: srv.command, ...(srv.args ? { args: srv.args } : {}) };
  }
  const required = requiredHosts(run.config, { controlUrl: base, r2AccountId: env.OF_R2_ACCOUNT_ID ?? "" });
  const np = run.config.network_policy;
  const outEnv = { ...plan.env };
  if (plan.workDir) outEnv.OF_WORKDIR = plan.workDir;
  const maxSession = Math.round(parseDuration(env.OF_MAX_SESSION_DURATION, 8 * 3_600_000) / 1000);
  void session;
  return {
    conversation_id: run.id,
    workspace: {
      repositories: plan.repos.map((r) => ({
        forge: r.forge,
        clone_url: splitCloneUrl(r.cloneUrl).clean,
        path: r.path,
        ref: r.ref,
      })),
      setup_commands: [
        ...plan.setupCommands.map((c) => ({ origin: "machine", command: c })),
        ...plan.envSetupCommands.map((c) => ({ origin: "environment", command: c })),
      ],
      setup_timeout_seconds: RUNNER_SETUP_TIMEOUT_S,
      skills: plan.skills.map((s) => ({ name: s.name, files: { ...(s.files ?? {}), "SKILL.md": s.content } })),
      snapshot: { restore: true, enabled: true, ...(cfg.snapshot === false ? { skip_workspace: true } : {}) },
    },
    harness: {
      type: cfg.harness?.type ?? "claude",
      model: cfg.harness?.model ?? "",
      reasoning_level: cfg.harness?.reasoningLevel ?? cfg.harness?.reasoning_level ?? "",
      system_prompt: plan.systemPrompt,
      resume_session_id: run.session_id ?? "",
      mcp_servers: mcp,
    },
    env: outEnv,
    idle_timeout_seconds: idleSeconds(env, cfg.idle_timeout_minutes, cfg.idle_timeout_set),
    idle_timeout_set: true,
    limits: { max_session_seconds: maxSession, event_batch_bytes: RUNNER_EVENT_BATCH_BYTES },
    ...(plan.cloud ? { cloud: plan.cloud } : {}),
    network_policy: {
      restricted: !!np?.restricted,
      allowed_hosts: np?.restricted ? [...new Set([...(np.allowed_hosts ?? []), ...required])].sort() : [],
      denied_hosts: np?.restricted ? (np.denied_hosts ?? []).filter((h) => !required.includes(h)) : [],
      always_allow: required,
    },
    egress_lock: np?.restricted ? "required" : "off",
    ...(env.OF_RUNNER_VERSION ? { runner_version: env.OF_RUNNER_VERSION } : {}),
  };
}

export type SecretsResponse = Awaited<ReturnType<SecretsBuilder>>;

/** Secrets from a plan (runnermode.go Secrets). */
export function secretsOf(plan: RunnerPlan): SecretsResponse {
  const values: string[] = [...Object.values(plan.secretEnv), ...Object.values(plan.harnessAuth)];
  const mcpHeaders: Record<string, Record<string, string>> = {};
  let mcpEnv: Record<string, Record<string, string>> | undefined;
  for (const [name, srv] of Object.entries(plan.mcpServers)) {
    if (srv.command && srv.env && Object.keys(srv.env).length) {
      (mcpEnv ??= {})[name] = srv.env;
      values.push(...Object.values(srv.env));
    }
    if (!srv.url || !srv.headers || !Object.keys(srv.headers).length || plan.sessionTokenServers.has(name)) continue;
    mcpHeaders[name] = srv.headers;
    for (const v of Object.values(srv.headers)) {
      values.push(v);
      if (v.startsWith("Bearer ")) values.push(v.slice(7));
    }
  }
  return {
    env: plan.secretEnv,
    files: [],
    harness_auth: plan.harnessAuth,
    harness_auth_default: plan.credential.source === CREDENTIAL_DEFAULT && Object.keys(plan.harnessAuth).length > 0,
    mcp_headers: mcpHeaders,
    ...(mcpEnv ? { mcp_env: mcpEnv } : {}),
    redact: redactList(values),
  };
}

// ---------- platform deps ----------

const internalMeta = (team: string, run: Run): CallMeta => {
  const principal: Principal = {
    kind: "run",
    id: run.id,
    runId: run.id,
    teamId: team,
    role: "admin",
    factoryRoles: {},
  };
  return { requestId: newId("req"), principal };
};

function opOut<T>(out: unknown, op: string): T {
  if (typeof out === "object" && out !== null && "$error" in out) {
    const e = (out as { $error: { code: string; detail: string } }).$error;
    throw new OfError(e.code as never, `${op}: ${e.detail}`);
  }
  if (typeof out === "object" && out !== null && "$status" in out) return (out as { body?: unknown }).body as T;
  return out as T;
}

/** PlannerDeps over the DO bindings of env. */
export function platformDeps(env: Env, run: Run): PlannerDeps {
  const team = env.TEAM.getByName(doName.team(run.team_id));
  const work = () => env.WORK.getByName(doName.work(run.team_id, partitionOf(run.id)));
  const meta = internalMeta(run.team_id, run);
  const secretCache = new Map<string, Promise<string | undefined>>();
  const listCache = new Map<string, Promise<{ name: string; type: string; factory_id?: string }[]>>();
  return {
    snapshot: async (fid) => (await team.configSnapshot(fid)) as Snapshot,
    state: async (r) => (await work().plannerState(r.id)) as PlannerState,
    recordStartCommits: async (r, list) => {
      await work().recordStartCommits(r.id, list);
    },
    environment: async (id) => {
      const out = await team.api(meta, "getEnvironment", { id }, null);
      if (typeof out === "object" && out !== null && "$error" in out) {
        if ((out as { $error: { code: string } }).$error.code === "resource_not_found") return undefined;
      }
      return opOut<Environment>(out, "getEnvironment");
    },
    secret: (fid, name) => {
      const k = `${fid}\u0000${name}`;
      let p = secretCache.get(k);
      if (!p) {
        p = (async () => {
          const v = opOut<{ found?: boolean; value?: string }>(
            await team.api(meta, "secretsGet", {}, { factory_id: fid, name }),
            "secretsGet",
          );
          return v?.found ? (v.value ?? "") : undefined;
        })();
        secretCache.set(k, p);
      }
      return p;
    },
    listSecrets: (fid) => {
      let p = listCache.get(fid);
      if (!p) {
        p = (async () =>
          opOut<{ items?: { name: string; type: string; factory_id?: string }[] }>(
            await team.api(meta, "listSecrets", fid ? { "?factory_id": fid } : {}, null),
            "listSecrets",
          )?.items ?? [])();
        listCache.set(fid, p);
      }
      return p;
    },
    resolveManagedMcp: async (r, managedId) =>
      opOut<McpServer>(
        await team.api(
          meta,
          "mcp.resolve",
          {},
          { run: { id: r.id, mcpServers: (r.config as Cfg).mcp_servers ?? {} }, managedId },
        ),
        "mcp.resolve",
      ),
    forges: resolverFor(env) as unknown as PlannerDeps["forges"],
    sandboxUrl: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, ""),
    now: () => Date.now(),
  };
}

let depsFactory: (env: Env, run: Run) => PlannerDeps = platformDeps;
/** Tests replace the platform dependencies of the planner. */
export function setPlannerDeps(fn: (env: Env, run: Run) => PlannerDeps): void {
  depsFactory = fn;
}
export function plannerFor(env: Env, run: Run): Planner {
  return new Planner(depsFactory(env, run));
}

/** The StartConfigBuilder of the planner. */
export const plannedStartConfig: StartConfigBuilder = async (env, run, session) =>
  startConfigOf(env, run, session, await plannerFor(env, run).plan(run));

/** The SecretsBuilder of the planner. */
export const plannedSecrets: SecretsBuilder = async (env, run) => secretsOf(await plannerFor(env, run).plan(run));
