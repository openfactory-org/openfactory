// A plan as the /runner/v1 responses: the start configuration and the
// session secrets (runnermode.go StartConfig and Secrets).
import type { Skill } from "@openfactory/core";
import type { Env } from "../../contracts/env";
import { requiredHosts } from "../../engine/hosts";
import { parseDuration } from "../../engine/sessions";
import type { Run, RunSession } from "../../engine/types";
import { deniedWithoutRequired, mcpServerUrlsOf, type SecretsBuilder, type StartConfig } from "../startconfig";
import { CREDENTIAL_DEFAULT, redactList, type HarnessCredential } from "./credentials";
import type { McpServer } from "./mcp";
import { splitCloneUrl, type Checkout } from "./repos";

export const RUNNER_SETUP_TIMEOUT_S = 1800;
export const RUNNER_EVENT_BATCH_BYTES = 1 << 20;

/** The secret part of a plan: what GET /runner/v1/session/secrets returns. */
export interface SecretsPlan {
  secretEnv: Record<string, string>;
  harnessAuth: Record<string, string>;
  credential: HarnessCredential;
  mcpServers: Record<string, McpServer>;
  sessionTokenServers: Set<string>;
}

/** The content of a runner session: what POST /runner/v1/session/start returns. */
export interface RunnerPlan extends SecretsPlan {
  env: Record<string, string>;
  repos: Checkout[];
  workDir: string;
  skills: Skill[];
  systemPrompt: string;
  setupCommands: string[];
  envSetupCommands: string[];
  cloud?: { gcp?: { audience: string; project_number: string; service_account_email?: string } };
}

function idleSeconds(env: Env, minutes: number | undefined, set: boolean | undefined): number {
  const def = parseDuration(env.OF_IDLE_TIMEOUT_DEFAULT, 10 * 60_000);
  const max = parseDuration(env.OF_IDLE_TIMEOUT_MAX, 60 * 60_000);
  let d = def;
  if (set || (minutes ?? 0) > 0) d = (minutes ?? 0) * 60_000;
  if (d > max) d = max;
  return Math.floor(d / 1000);
}

/** StartConfig from a plan (runnermode.go StartConfig, plus the network policy of the plan). */
export function startConfigOf(env: Env, run: Run, session: RunSession, plan: RunnerPlan): StartConfig {
  const cfg = run.config;
  const base = (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  const mcp: Record<string, { url?: string; auth?: string; command?: string; args?: string[] }> = {};
  for (const [name, srv] of Object.entries(plan.mcpServers)) {
    if (srv.url && plan.sessionTokenServers.has(name)) mcp[name] = { url: srv.url, auth: "session_token" };
    else if (srv.url) mcp[name] = { url: srv.url };
    else if (srv.command) mcp[name] = { command: srv.command, ...(srv.args ? { args: srv.args } : {}) };
  }
  const required = requiredHosts(run.config, {
    controlUrl: base,
    r2AccountId: env.OF_R2_ACCOUNT_ID ?? "",
    mcpServerUrls: mcpServerUrlsOf(run.config),
  });
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
      reasoning_level: cfg.harness?.reasoningLevel ?? "",
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
      denied_hosts: np?.restricted ? deniedWithoutRequired(np.denied_hosts, required) : [],
      always_allow: required,
    },
    egress_lock: np?.restricted ? "required" : "off",
    ...(env.OF_RUNNER_VERSION ? { runner_version: env.OF_RUNNER_VERSION } : {}),
  };
}

export type SecretsResponse = Awaited<ReturnType<SecretsBuilder>>;

/** Secrets from a plan (runnermode.go Secrets). */
export function secretsOf(plan: SecretsPlan): SecretsResponse {
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
