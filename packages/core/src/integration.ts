// Integration records, the MCP server registry and webhook deliveries. Port
// of internal/core (types.go). Field names are the Go JSON names.

import type { Timestamp } from "./common";
import { BUILTIN_MCP_PREFIX, BUILTIN_MCP_PROVIDERS } from "./definition";
import { cut } from "./gostd";

// ---------- Integrations ----------

/** Integration statuses. */
export const INTEGRATION_ACTIVE = "active";
export const INTEGRATION_DISABLED = "disabled";
export const INTEGRATION_AUTH_FAILED = "auth_failed";
export const INTEGRATION_STATUSES = [INTEGRATION_ACTIVE, INTEGRATION_DISABLED, INTEGRATION_AUTH_FAILED] as const;
export type IntegrationStatus = (typeof INTEGRATION_STATUSES)[number];

export interface Integration {
  id: string;
  team_id: string;
  provider: string;
  name: string;
  config: Record<string, string> | null;
  secret_ref?: string;
  factory_id?: string;
  status?: IntegrationStatus;
  status_reason?: string;
  status_updated_at?: Timestamp;
  created_at: Timestamp;
}

// ---------- MCP server registry ----------

/** Values of RegisteredMCPServer.connection. */
export const MCP_CONNECTION_PROXY = "proxy";
export const MCP_CONNECTION_DIRECT = "direct";
export const MCP_CONNECTIONS = [MCP_CONNECTION_PROXY, MCP_CONNECTION_DIRECT] as const;
export type MCPConnection = (typeof MCP_CONNECTIONS)[number];

/** MCP OAuth connection states. */
export const MCP_OAUTH_NOT_CONNECTED = "not_connected";
export const MCP_OAUTH_CONNECTED = "connected";
export const MCP_OAUTH_ERROR = "error";
export const MCP_OAUTH_STATUSES = [MCP_OAUTH_NOT_CONNECTED, MCP_OAUTH_CONNECTED, MCP_OAUTH_ERROR] as const;
export type MCPOAuthStatus = (typeof MCP_OAUTH_STATUSES)[number];

export interface MCPOAuthConfig {
  scopes?: string[];
  client_id?: string;
  client_secret_name?: string;
  authorization_endpoint?: string;
  token_endpoint?: string;
  status: MCPOAuthStatus;
  connected_by?: string;
  connected_at?: Timestamp;
  error?: string;
}

/** An entry of a team's MCP server registry. */
export interface RegisteredMCPServer {
  id: string;
  team_id: string;
  name: string;
  description?: string;
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  connection?: MCPConnection;
  oauth?: MCPOAuthConfig;
  created_by?: string;
  created_at: Timestamp;
  updated_at: Timestamp;
}

// ---------- Webhook deliveries ----------

/** The status of a delivery log entry for a repeated delivery. */
export const WEBHOOK_DELIVERY_DEDUPLICATED = "deduplicated";
export const WEBHOOK_DELIVERY_STATUSES = ["accepted", "rejected", WEBHOOK_DELIVERY_DEDUPLICATED] as const;
export type WebhookDeliveryStatus = (typeof WEBHOOK_DELIVERY_STATUSES)[number];

export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  delivery_id?: string;
  status: WebhookDeliveryStatus;
  reason?: string;
  headers?: Record<string, string>;
  body?: unknown;
  run_ids?: string[];
  received_at: Timestamp;
}

// ---------- Functions ----------

/** The integration record provider of a built-in server ("teams" -> "microsoft_teams"). */
export function builtinIntegrationProvider(provider: string): string {
  return provider === "teams" ? "microsoft_teams" : provider;
}

function normProvider(v: string): string {
  const s = v.trim().replaceAll("-", "_").toLowerCase();
  return s === "teams" ? "microsoft_teams" : s;
}

/** Reports whether two provider names name the same integration. */
export function sameIntegrationProvider(a: string, b: string): boolean {
  return normProvider(a) === normProvider(b);
}

/**
 * The provider of a built-in managedId ("builtin/linear" -> "linear"). known is
 * false for an id that is not built-in or names an unknown provider.
 */
export function builtinMCPProvider(managedId: string): { provider: string; known: boolean } {
  if (!managedId.startsWith(BUILTIN_MCP_PREFIX)) return { provider: "", known: false };
  const p = managedId.slice(BUILTIN_MCP_PREFIX.length);
  return { provider: p, known: BUILTIN_MCP_PROVIDERS.includes(p) };
}

/** Reports whether runs reach a registered server through the control-plane proxy. */
export function mcpServerProxied(m: { url?: string; connection?: string }): boolean {
  return !!m.url && m.connection === MCP_CONNECTION_PROXY;
}

/** The status of an integration record; empty means active. */
export function integrationStatus(i: Pick<Integration, "status">): IntegrationStatus {
  return i.status ? i.status : INTEGRATION_ACTIVE;
}

/** Reports whether an integration record can be used. */
export function integrationActive(i: Pick<Integration, "status">): boolean {
  return integrationStatus(i) === INTEGRATION_ACTIVE;
}

/** The config key of a chat or tracker record that names its workspace, or "". */
export function integrationWorkspaceKey(provider: string): string {
  if (sameIntegrationProvider(provider, "slack") || sameIntegrationProvider(provider, "microsoft_teams"))
    return "team_id";
  if (sameIntegrationProvider(provider, "linear")) return "organization_id";
  if (sameIntegrationProvider(provider, "jira")) return "site";
  return "";
}

/**
 * The form of a workspace value that the selection rule compares. For Jira it
 * is the host name, or the cloud ID of an OAuth API address.
 */
export function normalizeWorkspace(provider: string, v: string): string {
  let s = v.trim();
  if (s === "" || !sameIntegrationProvider(provider, "jira")) return s;
  if (!s.includes("://")) s = "https://" + s;
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return "";
  }
  const host = u.host.toLowerCase();
  if (host === "api.atlassian.com" && u.pathname.startsWith("/ex/jira/")) {
    return cut(u.pathname.slice("/ex/jira/".length), "/")[0].toLowerCase();
  }
  return host;
}

/** The normalized workspace of a chat or tracker record, or "". */
export function integrationWorkspace(i: Pick<Integration, "provider" | "config">): string {
  const k = integrationWorkspaceKey(i.provider);
  if (k === "") return "";
  return normalizeWorkspace(i.provider, i.config?.[k] ?? "");
}
