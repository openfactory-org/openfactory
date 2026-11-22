// The session start configuration (SessionConfigurer.StartConfig). This is a
// minimal planner: harness, system prompt, the run MCP server, setup commands
// and the network policy. The full planner (repositories with credentials,
// skills, environments, secrets) plugs in with setStartConfigBuilder.
import type { SessionDefinitions } from "@openfactory/runner-proto";
import type { Env } from "../contracts/env";
import { requiredHosts } from "../engine/hosts";
import { parseDuration } from "../engine/sessions";
import type { Run, RunSession } from "../engine/types";

type StartResponse = NonNullable<SessionDefinitions["StartResponse"]>;
type SecretsResponse = NonNullable<SessionDefinitions["SecretsResponse"]>;

export type StartConfig = Omit<StartResponse, "run_id" | "session" | "session_token" | "session_token_expires_at">;

/** Throw an OfError invalid_request or resource_not_found for a configuration fault (422 configuration_error). */
export type StartConfigBuilder = (env: Env, run: Run, session: RunSession) => Promise<StartConfig>;
export type SecretsBuilder = (env: Env, run: Run, session: RunSession) => Promise<SecretsResponse>;

/**
 * The denied hosts of a restricted policy without the hosts the session needs.
 * Host names compare without case; required hosts are already lower case.
 */
export function deniedWithoutRequired(
  denied: readonly string[] | null | undefined,
  required: readonly string[],
): string[] {
  return (denied ?? []).filter((h) => !required.includes(h.trim().toLowerCase()));
}

/**
 * The URLs of the MCP servers in a run configuration. Required hosts take
 * their hosts, like engine.RequiredHosts of the Go server with cfg.MCPServers.
 */
export function mcpServerUrlsOf(cfg: Pick<Run["config"], "mcp_servers">): string[] {
  const urls: string[] = [];
  for (const srv of Object.values(cfg.mcp_servers ?? {})) {
    const url = (srv as { url?: unknown } | null)?.url;
    if (typeof url === "string" && url) urls.push(url);
  }
  return urls;
}

const publicUrl = (env: Env) => (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");

export const defaultStartConfig: StartConfigBuilder = async (env, run, session) => {
  const cfg = run.config;
  const base = publicUrl(env);
  const required = requiredHosts(cfg, {
    controlUrl: base,
    r2AccountId: env.OF_R2_ACCOUNT_ID ?? "",
    mcpServerUrls: mcpServerUrlsOf(cfg),
  });
  const np = cfg.network_policy;
  const idle = cfg.idle_timeout_set ? (cfg.idle_timeout_minutes ?? 0) * 60 : 600;
  const maxSession = Math.round(parseDuration(env.OF_MAX_SESSION_DURATION, 8 * 3_600_000) / 1000);
  return {
    conversation_id: run.id,
    env: {
      OF_RUN_ID: run.id,
      OF_FACTORY_ID: run.factory_id,
      ...(run.work_item_id ? { OF_WORK_ITEM_ID: run.work_item_id } : {}),
    },
    harness: {
      type: cfg.harness?.type ?? "claude",
      model: cfg.harness?.model ?? "",
      reasoning_level: cfg.harness?.reasoning_level ?? "",
      resume_session_id: run.session_id ?? "",
      system_prompt: cfg.instructions ?? "",
      mcp_servers: { factory: { url: `${base}/mcp/run/${encodeURIComponent(run.id)}`, auth: "session" } },
    },
    idle_timeout_seconds: idle,
    ...(cfg.idle_timeout_set ? { idle_timeout_set: true } : {}),
    limits: { event_batch_bytes: 1 << 20, max_session_seconds: maxSession },
    network_policy: {
      restricted: !!np?.restricted,
      allowed_hosts: np?.restricted ? [...new Set([...(np.allowed_hosts ?? []), ...required])].sort() : [],
      denied_hosts: np?.restricted ? deniedWithoutRequired(np.denied_hosts, required) : [],
      always_allow: required,
    },
    egress_lock: np?.restricted ? "required" : "off",
    workspace: {
      repositories: [],
      setup_commands: (cfg.machine?.setupCommands ?? []).map((c) => ({ command: c, origin: "machine" })),
      setup_timeout_seconds: 1800,
      skills: [],
      snapshot: { enabled: cfg.snapshot !== false, restore: session.session > 1 },
    },
    runner_version: env.OF_RUNNER_VERSION,
  };
};

export const defaultSecrets: SecretsBuilder = async () => ({
  env: {},
  files: [],
  harness_auth: {},
  mcp_headers: {},
  redact: [],
});

let startBuilder = defaultStartConfig;
let secretsBuilder = defaultSecrets;

export function setStartConfigBuilder(fn: StartConfigBuilder): void {
  startBuilder = fn;
}
export function setSecretsBuilder(fn: SecretsBuilder): void {
  secretsBuilder = fn;
}
export const buildStartConfig: StartConfigBuilder = (env, run, s) => startBuilder(env, run, s);
export const buildSecrets: SecretsBuilder = (env, run, s) => secretsBuilder(env, run, s);
