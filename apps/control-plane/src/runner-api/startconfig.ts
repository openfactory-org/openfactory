// Types and helpers of the session start configuration
// (SessionConfigurer.StartConfig). The planner (./planner.ts) builds it.
import type { SessionDefinitions } from "@openfactory/runner-proto";
import type { Env } from "../contracts/env";
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
