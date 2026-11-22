// The session planner in the Worker. Port of internal/engine
// planner.go, runner_plan.go, credentials.go, defaultauth.go, redact.go and
// the runnerContent of internal/server/runnermode.go. Stateless: inputs come
// from the run's WorkDO (work item, repository refs), TeamDO (config snapshot,
// environments, secrets, MCP registry) and the forge resolver. It never runs
// inside a DO transaction.
//
// The stages live in ./planner/: repositories (repos), harness credentials
// (credentials), MCP servers (mcp), the /runner/v1 responses (render) and
// the platform view (deps). This file composes them per request: planStart
// for the start configuration, planSecrets for the session secrets, and
// gitCredential for one repository.
import {
  builtinSkillsForRun,
  composeSystemPrompt,
  METADATA_ENV_PREFIX,
  metadataEnv,
  mergeSkills,
  OfError,
  type Skill,
} from "@openfactory/core";
import type { Env } from "../contracts/env";
import type { Run, WorkItem } from "../engine/types";
import { findDefaultCredential, headerSecrets, redactList, resolveHarnessAuth } from "./planner/credentials";
import { environmentOf, platformDeps, type Environment, type PlannerDeps, type Snapshot } from "./planner/deps";
import { mcpServers, sessionTokenServers } from "./planner/mcp";
import { secretsOf, startConfigOf, type RunnerPlan, type SecretsPlan } from "./planner/render";
import { checkouts, isCommitSha, repoNamed } from "./planner/repos";
import type { SecretsBuilder, StartConfigBuilder } from "./startconfig";

export * from "./planner/credentials";
export { platformDeps, type Environment, type PlannerDeps, type PlannerState, type Snapshot } from "./planner/deps";
export * from "./planner/mcp";
export * from "./planner/render";
export * from "./planner/repos";

/** Forge app tokens last one hour; the runner asks again after 50 minutes. */
export const GIT_CREDENTIAL_TTL_MS = 50 * 60_000;
const SELF_IMPROVEMENT = "self_improvement";
const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export class Planner {
  constructor(private readonly d: PlannerDeps) {}

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

  /** runSkills: agent, factory, then the built-in skills of the run. */
  runSkills(r: Run, snap: Snapshot, wi: WorkItem | undefined): Skill[] {
    const integrations = Object.keys(snap.integrations ?? {}).sort();
    const forges = (snap.runtime.repositories ?? []).map((x) => x.codeForge);
    const self =
      r.triggered_by === SELF_IMPROVEMENT ||
      (wi?.source as { provider?: string } | undefined)?.provider === SELF_IMPROVEMENT;
    const builtin = builtinSkillsForRun({ agentType: r.config.type, forges, integrations, selfImprovement: self });
    return mergeSkills(r.config.skills ?? [], snap.runtime.skills ?? [], [], builtin);
  }

  findDefaultCredential(r: Run) {
    return findDefaultCredential(this.d, r);
  }

  resolveHarnessAuth(r: Run, env: Record<string, string>) {
    return resolveHarnessAuth(this.d, r, env);
  }

  /** The run environment split into plain and secret variables, the harness credential and the MCP servers. */
  private async secretParts(
    r: Run,
    envr: Environment | undefined,
  ): Promise<SecretsPlan & { env: Record<string, string> }> {
    const all = await this.runEnv(r, envr);
    const { auth, credential } = await resolveHarnessAuth(this.d, r, all);
    const mcp = await mcpServers(this.d, r);
    const sessionServers = sessionTokenServers(this.d.sandboxUrl, mcp);
    const secretNames = new Set(r.config.secrets ?? []);
    const env: Record<string, string> = {};
    const secretEnv: Record<string, string> = {};
    for (const [k, v] of Object.entries(all)) (secretNames.has(k) ? secretEnv : env)[k] = v;
    return { env, secretEnv, harnessAuth: auth, credential, mcpServers: mcp, sessionTokenServers: sessionServers };
  }

  /**
   * The secrets of a session. It reads no factory snapshot and asks no forge
   * for a clone URL: the secrets response holds neither repositories nor
   * prompts.
   */
  async planSecrets(r: Run): Promise<SecretsPlan> {
    const p = await this.secretParts(r, await environmentOf(this.d, r));
    return {
      secretEnv: p.secretEnv,
      harnessAuth: p.harnessAuth,
      credential: p.credential,
      mcpServers: p.mcpServers,
      sessionTokenServers: p.sessionTokenServers,
    };
  }

  /** RunnerPlan: the content of a runner session. Records the start commits once. */
  async planStart(r: Run): Promise<RunnerPlan> {
    const [snap, st] = await Promise.all([this.d.snapshot(r.factory_id), this.d.state(r)]);
    const envr = await environmentOf(this.d, r);
    const sec = await this.secretParts(r, envr);
    const repos = await checkouts(this.d, r, snap, st, envr);
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
    const systemPrompt = composeSystemPrompt(
      { ...r.config, skills },
      st.workItem ?? null,
      r,
      (snap.agents ?? []).map((a) => ({
        id: "",
        ...a,
        description: a.description ?? "",
        spawnable_by: a.spawnable_by ?? null,
      })),
    );
    const paths = envr ? [] : (snap.runtime.repositories ?? []).map((x) => x.name);
    const g = snap.runtime.cloud_providers?.gcp;
    return {
      ...sec,
      repos,
      workDir: paths.length === 1 ? paths[0]! : "",
      skills,
      systemPrompt,
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

  /** planStart under its older name. */
  plan(r: Run): Promise<RunnerPlan> {
    return this.planStart(r);
  }

  /**
   * RunnerGitCredential: the credential of one repository of the run, else
   * not_authorized. The forge mints a clone URL only for the repositories
   * whose name matches the requested path.
   */
  async gitCredential(
    r: Run,
    host: string,
    path: string,
  ): Promise<{ username: string; password: string; expiresAt: string }> {
    const [snap, st] = await Promise.all([this.d.snapshot(r.factory_id), this.d.state(r)]);
    const envr = await environmentOf(this.d, r);
    const want = path.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
    const repos = await checkouts(this.d, r, snap, st, envr, repoNamed(want));
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
      env = await this.runEnv(r, await environmentOf(this.d, r));
      for (const n of r.config.secrets ?? []) if (env[n]) out.push(env[n]!);
    } catch {
      /* best effort */
    }
    try {
      const { auth } = await resolveHarnessAuth(this.d, r, env);
      for (const [k, v] of Object.entries(auth)) if (!k.toUpperCase().endsWith("_BASE_URL")) out.push(v);
    } catch {
      /* best effort */
    }
    try {
      for (const s of Object.values(await mcpServers(this.d, r))) {
        for (const v of Object.values(s.headers ?? {})) out.push(...headerSecrets(v));
        for (const v of Object.values(s.env ?? {})) out.push(v);
      }
    } catch {
      /* best effort */
    }
    return redactList(out);
  }
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
  startConfigOf(env, run, session, await plannerFor(env, run).planStart(run));

/** The SecretsBuilder of the planner. */
export const plannedSecrets: SecretsBuilder = async (env, run) =>
  secretsOf(await plannerFor(env, run).planSecrets(run));
