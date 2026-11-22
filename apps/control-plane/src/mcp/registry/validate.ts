// Port of internal/mcpregistry/validate.go, plus the private-host warning of
// appendix 04 gap 20: a proxy-mode server on a private network cannot be
// reached from Cloudflare.
import {
  MCP_CONNECTION_DIRECT,
  MCP_CONNECTION_PROXY,
  MCP_OAUTH_NOT_CONNECTED,
  OfError,
  type RegisteredMCPServer,
} from "@openfactory/core";

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const PLACEHOLDER_RE = /\{\{\s*[A-Z_][A-Z0-9_]*\s*\}\}/;
const SENSITIVE_RE = /(authorization|token|secret|password|passwd|api[-_]?key|apikey|credential|cookie|private)/i;

const invalid = (msg: string) => new OfError("invalid_request", msg);

/** harness.IsSSEURL: the path ends in /sse. */
export function isSSEURL(raw: string): boolean {
  try {
    return new URL(raw).pathname.replace(/\/+$/, "").endsWith("/sse");
  } catch {
    return false;
  }
}

/** Whether a URL server goes through the control-plane proxy. */
export function proxied(m: Pick<RegisteredMCPServer, "url" | "connection">): boolean {
  return !!m.url && m.connection === MCP_CONNECTION_PROXY;
}

export const SSE_DIRECT_WARNING =
  "the legacy SSE transport cannot use the control-plane proxy: the server is registered with connection direct, so its headers go to the sandbox";

export const PRIVATE_HOST_WARNING =
  "the server's host is on a private network, which the control plane cannot reach: use connection direct, or expose the server through a tunnel";

/**
 * Sets the connection mode of a new url server that names none: proxy, or
 * direct with a warning for a legacy SSE URL. Returns the warning, or "".
 */
export function defaultConnection(m: RegisteredMCPServer): string {
  if ((m.url ?? "").trim() === "" || (m.connection ?? "") !== "") return "";
  if (!m.oauth && isSSEURL((m.url ?? "").trim())) {
    m.connection = MCP_CONNECTION_DIRECT;
    return SSE_DIRECT_WARNING;
  }
  m.connection = MCP_CONNECTION_PROXY;
  return "";
}

/** Warning for a proxied server whose host is private (RFC 1918, .local, loopback, link-local, CGNAT). */
export function privateHostWarning(m: RegisteredMCPServer): string {
  if (!proxied(m)) return "";
  try {
    return isPrivateHost(new URL(m.url!).hostname) ? PRIVATE_HOST_WARNING : "";
  } catch {
    return "";
  }
}

export function isPrivateHost(host: string): boolean {
  const h = host.toLowerCase().replace(/^\[|\]$/g, "");
  if (h === "localhost" || h.endsWith(".localhost") || h.endsWith(".local") || h.endsWith(".internal") || !h.includes(".") && !h.includes(":")) {
    return true;
  }
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])];
    return (
      a === 10 || a === 127 || a === 0 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) ||
      (a === 169 && b === 254) || (a === 100 && b >= 64 && b <= 127)
    );
  }
  if (h.includes(":")) return h === "::1" || h.startsWith("fc") || h.startsWith("fd") || h.startsWith("fe80");
  return false;
}

/**
 * Checks a registry entry and normalizes it (trimmed fields, connection proxy
 * forced for OAuth). An empty connection of a url server becomes direct, or
 * proxy with OAuth. It does not check name uniqueness.
 */
export function validate(m: RegisteredMCPServer): void {
  m.name = (m.name ?? "").trim();
  m.url = (m.url ?? "").trim();
  m.command = (m.command ?? "").trim();
  m.connection = (m.connection ?? "").trim().toLowerCase();
  if (m.connection !== "" && m.connection !== MCP_CONNECTION_PROXY && m.connection !== MCP_CONNECTION_DIRECT) {
    throw invalid(`connection "${m.connection}": want proxy or direct`);
  }
  if (!NAME_RE.test(m.name)) {
    throw invalid(`name "${m.name}" must be 1-64 letters, digits, '.', '_' or '-' and start with a letter or digit`);
  }
  if (m.name.toLowerCase().startsWith("builtin")) {
    throw invalid(`name "${m.name}": names starting with "builtin" are reserved for built-in servers`);
  }
  if ((m.url === "") === (m.command === "")) throw invalid("set exactly one of url and command");
  const args = m.args ?? [];
  const env = m.env ?? {};
  const headers = m.headers ?? {};
  if (m.url !== "") {
    const e = checkURL(m.url, false);
    if (e) throw invalid(`url: ${e}`);
    if (args.length > 0 || Object.keys(env).length > 0) throw invalid("args and env apply only to command servers");
  }
  if (m.command !== "") {
    if (Object.keys(headers).length > 0) throw invalid("headers apply only to url servers");
    if (m.connection === MCP_CONNECTION_PROXY || m.oauth) throw invalid("connection proxy and oauth apply only to url servers");
    m.connection = "";
  }
  if (m.url !== "" && m.connection === "") m.connection = m.oauth ? MCP_CONNECTION_PROXY : MCP_CONNECTION_DIRECT;
  if (m.oauth) {
    if (m.connection === MCP_CONNECTION_DIRECT) {
      throw invalid("an OAuth server needs connection proxy: its tokens stay in the control plane");
    }
    m.connection = MCP_CONNECTION_PROXY;
    const o = m.oauth;
    o.client_id = (o.client_id ?? "").trim();
    o.client_secret_name = (o.client_secret_name ?? "").trim();
    if (o.client_secret_name !== "" && o.client_id === "") throw invalid("oauth.client_secret_name needs oauth.client_id");
    for (const [name, v] of [
      ["authorization_endpoint", o.authorization_endpoint ?? ""],
      ["token_endpoint", o.token_endpoint ?? ""],
    ] as const) {
      if (v === "") continue;
      const e = checkURL(v, true);
      if (e) throw invalid(`oauth.${name}: ${e}`);
    }
    for (const sc of o.scopes ?? []) {
      if (sc.trim() === "" || /[ \t\n]/.test(sc)) throw invalid("oauth.scopes entries must be single non-empty scope values");
    }
    if (!o.status) o.status = MCP_OAUTH_NOT_CONNECTED;
    if (o.client_id === "") delete o.client_id;
    if (o.client_secret_name === "") delete o.client_secret_name;
  }
  if (m.connection === MCP_CONNECTION_PROXY && isSSEURL(m.url)) {
    throw invalid(
      "proxied servers must use the streamable HTTP transport; legacy SSE URLs (ending in /sse) cannot be proxied: set connection direct",
    );
  }
  for (const [k, v] of Object.entries(headers)) checkCredential("headers", k, v);
  for (const [k, v] of Object.entries(env)) checkCredential("env", k, v);
  // Go omits empty fields (omitempty).
  if (m.url === "") delete m.url;
  if (m.command === "") delete m.command;
  if (m.connection === "") delete m.connection;
}

/** Literal values for credential-like keys are refused: reference a secret with {{SECRET}}. */
function checkCredential(field: string, key: string, value: string): void {
  if (key.trim() === "") throw invalid(`${field} keys must be non-empty`);
  if (SENSITIVE_RE.test(key) && !PLACEHOLDER_RE.test(value)) {
    throw invalid(`${field}.${key} looks like a credential: store it as a secret and reference it as {{SECRET_NAME}}`);
  }
}

/** An absolute http(s) URL; with httpsOnly, plain http only for loopback hosts. Returns the error text or "". */
export function checkURL(raw: string, httpsOnly: boolean): string {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return "must be an absolute http or https URL";
  }
  if (u.host === "" || (u.protocol !== "http:" && u.protocol !== "https:")) return "must be an absolute http or https URL";
  if (u.username !== "" || u.password !== "" || /^[a-z]+:\/\/[^/]*@/i.test(raw)) return "must not embed credentials";
  if (httpsOnly && u.protocol === "http:" && !isLoopbackHost(u.hostname)) return "must use https";
  return "";
}

export function isLoopbackHost(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, "");
  if (h === "localhost" || h.endsWith(".localhost")) return true;
  if (h === "::1") return true;
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h);
}
