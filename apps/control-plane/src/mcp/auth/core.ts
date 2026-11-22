// Port of internal/mcpauth: constants, records and pure helpers of the
// OAuth 2.1 authorization server of the Factory MCP browser sign-in.
//
//   GET  /.well-known/oauth-protected-resource[/mcp]  RFC 9728
//   GET  /.well-known/oauth-authorization-server      RFC 8414
//   POST /oauth/register                              RFC 7591 (public clients)
//   GET  /oauth/authorize, POST /oauth/authorize      consent; PKCE S256
//   POST /oauth/token                                 code or refresh token
//   POST /oauth/revoke                                RFC 7009
//
// One sign-in is a Grant. Access tokens (ofm_) live 1 hour, refresh tokens
// (ofr_) 30 days and rotate at each use. Only SHA-256 hashes of tokens and
// codes are stored, in TeamDO (team:default; ofm_ tokens carry no team).
import { b64url, randomBytes } from "../../lib/bytes";

export const ACCESS_TOKEN_PREFIX = "ofm_";
export const REFRESH_TOKEN_PREFIX = "ofr_";
export const SCOPE = "mcp";

export const ACCESS_TTL_MS = 60 * 60 * 1000;
export const REFRESH_TTL_MS = 30 * 24 * 60 * 60 * 1000;
export const CODE_TTL_MS = 5 * 60 * 1000;
export const REQUEST_TTL_MS = 10 * 60 * 1000;
export const TOUCH_EVERY_MS = 5 * 60 * 1000;

export const RESOURCE_METADATA_PATH = "/.well-known/oauth-protected-resource";
export const SERVER_METADATA_PATH = "/.well-known/oauth-authorization-server";
export const REGISTER_PATH = "/oauth/register";
export const AUTHORIZE_PATH = "/oauth/authorize";
export const TOKEN_PATH = "/oauth/token";
export const REVOKE_PATH = "/oauth/revoke";
export const LOGIN_PATH = "/login";

/** The protected Factory MCP endpoints. */
export const FACTORY_MCP_PATH = "/mcp";
export const FACTORY_MCP_ALIAS_PATH = "/api/v1/mcp/factory";
export const RESOURCE_PATHS = [FACTORY_MCP_PATH, FACTORY_MCP_ALIAS_PATH] as const;

export const MAX_REDIRECT_URIS = 10;

/** A registered OAuth client (an MCP client on a developer machine). */
export interface Client {
  client_id: string;
  client_name?: string;
  redirect_uris: string[];
  created_at: string;
}

/** One MCP sign-in: a user allowed a client. */
export interface Grant {
  id: string;
  user_id: string;
  team_id: string;
  client_id: string;
  client_name: string;
  created_at: string;
  last_used_at?: string;
  /** End of the current refresh token. */
  expires_at: string;
  revoked_at?: string;
}

/** A pending authorization request between the consent page and the answer. */
export interface AuthRequest {
  id: string;
  client_id: string;
  redirect_uri: string;
  state?: string;
  code_challenge: string;
  resource?: string;
  user_id: string;
  expires: string;
}

/** An issued authorization code. */
export interface CodeRecord {
  client_id: string;
  redirect_uri: string;
  code_challenge: string;
  user_id: string;
  team_id: string;
  expires: string;
}

/** prefix + base64url(32 random bytes). */
export function randomToken(prefix: string): string {
  return prefix + b64url(randomBytes(32));
}

export function isLoopback(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, "");
  if (h.toLowerCase() === "localhost") return true;
  if (/^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h)) return true;
  return h === "::1" || /^(0{0,4}:){7}0{0,3}1$/.test(h);
}

interface ParsedURL {
  scheme: string;
  host: string;
  hostname: string;
  path: string;
  rawQuery: string;
  fragment: boolean;
}

/** A small url.Parse for absolute URLs, including private-use schemes. */
function parseURL(raw: string): ParsedURL | null {
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):(.*)$/s.exec(raw);
  if (!m) return null;
  const scheme = m[1]!;
  let rest = m[2]!;
  const hashAt = rest.indexOf("#");
  const fragment = hashAt >= 0;
  if (fragment) rest = rest.slice(0, hashAt);
  const qAt = rest.indexOf("?");
  const rawQuery = qAt >= 0 ? rest.slice(qAt + 1) : "";
  if (qAt >= 0) rest = rest.slice(0, qAt);
  let host = "";
  let path = rest;
  if (rest.startsWith("//")) {
    const slash = rest.indexOf("/", 2);
    host = slash >= 0 ? rest.slice(2, slash) : rest.slice(2);
    path = slash >= 0 ? rest.slice(slash) : "";
    const at = host.lastIndexOf("@");
    if (at >= 0) host = host.slice(at + 1);
  }
  let hostname = host;
  if (hostname.startsWith("[")) {
    const end = hostname.indexOf("]");
    hostname = end >= 0 ? hostname.slice(1, end) : hostname;
  } else {
    const colon = hostname.lastIndexOf(":");
    if (colon >= 0) hostname = hostname.slice(0, colon);
  }
  return { scheme, host, hostname, path, rawQuery, fragment };
}

/**
 * Accepts https URLs, http URLs on a loopback host, and private-use schemes
 * of native apps (RFC 8252). Returns the error text, or "".
 */
export function checkRedirectURI(raw: string): string {
  const u = parseURL(raw);
  if (!u || u.scheme === "" || u.fragment) return "redirect_uri must be an absolute URL without a fragment";
  switch (u.scheme.toLowerCase()) {
    case "https":
      if (u.host === "") return "redirect_uri must have a host";
      break;
    case "http":
      if (!isLoopback(u.hostname)) return "an http redirect_uri must use a loopback host (localhost, 127.0.0.1 or [::1])";
      break;
    case "javascript":
    case "data":
    case "file":
    case "vbscript":
      return `redirect_uri scheme ${u.scheme} is not allowed`;
  }
  return "";
}

/** A loopback URI matches on any port (RFC 8252, section 7.3). */
export function redirectAllowed(c: Client, redirect: string): boolean {
  for (const reg of c.redirect_uris) {
    if (reg === redirect) return true;
    const a = parseURL(reg);
    const b = parseURL(redirect);
    if (!a || !b) continue;
    if (
      a.scheme === "http" && b.scheme === "http" && isLoopback(a.hostname) && a.hostname === b.hostname &&
      a.path === b.path && a.rawQuery === b.rawQuery
    ) {
      return true;
    }
  }
  return false;
}

/** The scheme://host of a redirect URI, for the consent page. */
export function redirectHost(redirect: string): string {
  const u = parseURL(redirect);
  return u && u.host !== "" ? `${u.scheme}://${u.host}` : redirect;
}

/** Go url.Values.Encode: keys sorted, QueryEscape (space as +). */
export function encodeValues(v: Record<string, string>): string {
  return Object.keys(v)
    .sort()
    .map((k) => `${qe(k)}=${qe(v[k]!)}`)
    .join("&");
}

function qe(s: string): string {
  return encodeURIComponent(s)
    .replace(/%20/g, "+")
    .replace(/[!'()*]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase());
}

export function withQuery(raw: string, v: Record<string, string>): string {
  return raw + (raw.includes("?") ? "&" : "?") + encodeValues(v);
}

export function errorRedirect(redirect: string, state: string, iss: string, code: string, desc: string): string {
  const v: Record<string, string> = { error: code, error_description: desc, iss };
  if (state) v.state = state;
  return withQuery(redirect, v);
}

// ---------- results of the DO operations ----------

export interface PageData {
  title: string;
  message?: string;
  consent?: boolean;
  requestId?: string;
  clientName?: string;
  userLabel?: string;
  redirectHost?: string;
  action?: string;
}

/** What a Worker endpoint answers. Plain objects, so they cross RPC. */
export type Outcome =
  | { kind: "page"; status: number; page: PageData }
  | { kind: "redirect"; location: string }
  | { kind: "json"; status: number; body: unknown };

export const oauthError = (status: number, code: string, desc: string): Outcome => ({
  kind: "json",
  status,
  body: { error: code, error_description: desc },
});

// ---------- consent page ----------

function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&#34;", "'": "&#39;" })[c]!);
}

/** The consent and message page (html/template of handlers.go, with escaping). */
export function renderPage(d: PageData): string {
  const body = d.consent
    ? `<p><strong>${esc(d.clientName ?? "")}</strong> asks to use Factory MCP as <strong>${esc(d.userLabel ?? "")}</strong>.</p>
<p>It can do what you can do in your factories: list and read work items, send work, send messages to foremen and close work items.</p>
<p class="muted">After you allow it, your browser goes back to <code>${esc(d.redirectHost ?? "")}</code>. You can end this sign-in at any time in the dashboard, on the MCP servers page.</p>
<form method="post" action="${esc(d.action ?? AUTHORIZE_PATH)}">
<input type="hidden" name="request_id" value="${esc(d.requestId ?? "")}">
<div class="row"><button type="submit" name="decision" value="deny">Deny</button><button class="primary" type="submit" name="decision" value="allow">Allow</button></div>
</form>`
    : `<p>${esc(d.message ?? "")}</p>`;
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(d.title)} · Open Factory</title>
<style>
:root{--bg:#f7f7f8;--surface:#fff;--border:#e2e4e8;--text:#1b1d22;--muted:#5d6370;--accent:#3355dd;--accent-contrast:#fff;color-scheme:light}
@media (prefers-color-scheme: dark){:root{--bg:#111317;--surface:#181b20;--border:#2c313a;--text:#e8eaee;--muted:#a0a6b2;--accent:#7d97ff;--accent-contrast:#0d1020;color-scheme:dark}}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:24px;max-width:440px;width:100%}
h1{font-size:1.25rem;margin:0 0 12px}p{margin:0 0 12px}.muted{color:var(--muted);font-size:.9rem}
.row{display:flex;gap:8px;margin-top:20px}button{flex:1;font:inherit;padding:10px 12px;border-radius:8px;border:1px solid var(--border);background:var(--surface);color:var(--text);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-contrast)}code{font-size:.9em}
</style></head><body><main>
<h1>${esc(d.title)}</h1>
${body}
</main></body></html>`;
}
