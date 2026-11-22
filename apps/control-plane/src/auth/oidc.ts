// OIDC login provider (authorization code + PKCE S256, nonce, ID token
// verification with jose). Port of internal/auth/oidc.go. Discovery is lazy
// and cached per isolate.
import { createRemoteJWKSet, jwtVerify, type JWTPayload } from "jose";
import type { Env } from "../contracts/env";
import { b64url, randomToken, sha256, timingSafeEqual, utf8 } from "../lib/bytes";
import type { Identity } from "../do/team/auth";
import { normalizeRole, validRole } from "./roles";

export const OIDC_CALLBACK_PATH = "/auth/oidc/callback";
export const OIDC_FLOW_TTL_MS = 10 * 60_000;

export interface OIDCConfig {
  issuer: string;
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
  scopes: string[];
  allowedDomains: string[];
  groupsClaim: string;
  adminGroups: string[];
  editorGroups: string[];
  defaultRole: string;
  displayName: string;
  requireVerifiedEmail: boolean;
}

/** SplitList: comma- or whitespace-separated, empties dropped. */
export function splitList(s: string | undefined): string[] {
  return (s ?? "").split(/[,\s]+/).filter(Boolean);
}

export class LoginFailure extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(`login failed: ${code}: ${message}`);
  }
}

/**
 * Reads the OIDC settings. OF_OIDC_ISSUER_URL, OF_OIDC_CLIENT_ID and the
 * secret OF_OIDC_CLIENT_SECRET are in Env; the other OF_OIDC_* names are
 * optional extra vars. Returns null when no issuer is set. Throws a list of
 * problems for an invalid configuration.
 */
export function oidcConfigFromEnv(env: Env): OIDCConfig | null {
  const e = env as unknown as Record<string, string | undefined>;
  const get = (k: string) => (e[k] ?? "").trim();
  const issuer = get("OF_OIDC_ISSUER_URL") || get("OF_OIDC_ISSUER");
  if (!issuer) return null;
  const pub = get("OF_PUBLIC_URL").replace(/\/+$/, "");
  const cfg: OIDCConfig = {
    issuer,
    clientId: get("OF_OIDC_CLIENT_ID"),
    clientSecret: get("OF_OIDC_CLIENT_SECRET"),
    redirectUrl: get("OF_OIDC_REDIRECT_URL") || (pub ? pub + OIDC_CALLBACK_PATH : ""),
    scopes: splitList(get("OF_OIDC_SCOPES")),
    allowedDomains: splitList(get("OF_OIDC_ALLOWED_DOMAINS")).map((d) => d.replace(/^@/, "").toLowerCase()),
    groupsClaim: get("OF_OIDC_GROUPS_CLAIM") || "groups",
    adminGroups: splitList(get("OF_OIDC_ADMIN_GROUPS")),
    editorGroups: splitList(get("OF_OIDC_EDITOR_GROUPS")),
    defaultRole: get("OF_OIDC_DEFAULT_ROLE").toLowerCase() || "user",
    displayName: get("OF_OIDC_DISPLAY_NAME") || "Single sign-on",
    requireVerifiedEmail: !/^(0|f|false)$/i.test(get("OF_OIDC_REQUIRE_VERIFIED_EMAIL")),
  };
  const errs: string[] = [];
  if (!cfg.clientId) errs.push("OF_OIDC_CLIENT_ID is required");
  try {
    const u = new URL(cfg.redirectUrl);
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error();
  } catch {
    errs.push("OF_OIDC_REDIRECT_URL (or OF_PUBLIC_URL) must be an absolute http(s) URL");
  }
  if (cfg.scopes.length === 0) cfg.scopes = ["openid", "profile", "email"];
  else if (!cfg.scopes.includes("openid")) cfg.scopes.unshift("openid");
  if (validRole(cfg.defaultRole)) cfg.defaultRole = normalizeRole(cfg.defaultRole);
  else errs.push(`OF_OIDC_DEFAULT_ROLE "${cfg.defaultRole}" must be admin, editor or user`);
  if (errs.length) throw new Error(errs.join("; "));
  return cfg;
}

export const roleManaged = (c: OIDCConfig) => c.adminGroups.length > 0 || c.editorGroups.length > 0;

/** MapRole: admin group, else editor group, else the default role. Exact, case-sensitive. */
export function mapRole(c: OIDCConfig, groups: string[]): string {
  if (groups.some((g) => c.adminGroups.includes(g))) return "admin";
  if (groups.some((g) => c.editorGroups.includes(g))) return "editor";
  return c.defaultRole;
}

/** claimStrings: array of strings, or one comma/space-separated string. */
export function claimStrings(v: unknown): string[] {
  if (Array.isArray(v)) return v.filter((x): x is string => typeof x === "string" && x !== "");
  if (typeof v === "string") return splitList(v);
  return [];
}

/** identity: claims to Identity with the domain and verified-email rules. */
export function identityFromClaims(c: OIDCConfig, sub: string, claims: Record<string, unknown>): Identity {
  const ev = claims["email_verified"];
  let verified = ev === true || (typeof ev === "string" && ev.toLowerCase() === "true");
  const email = typeof claims["email"] === "string" ? (claims["email"] as string).trim() : "";
  if (!c.requireVerifiedEmail && email !== "") verified = true;
  if (c.allowedDomains.length > 0) {
    const at = email.toLowerCase().indexOf("@");
    const domain = at < 0 ? "" : email.toLowerCase().slice(at + 1);
    if (at < 0 || !c.allowedDomains.includes(domain)) {
      throw new LoginFailure("domain_not_allowed", `email "${email}" is not in an allowed domain`);
    }
    if (!verified) throw new LoginFailure("email_not_verified", `email "${email}" is not verified`);
  }
  const groups = claimStrings(claims[c.groupsClaim]);
  return {
    provider: "oidc",
    subject: sub,
    email,
    emailVerified: verified,
    name: typeof claims["name"] === "string" ? (claims["name"] as string) : "",
    handle: typeof claims["preferred_username"] === "string" ? (claims["preferred_username"] as string) : "",
    groups,
    role: mapRole(c, groups),
    roleManaged: roleManaged(c),
    defaultRole: c.defaultRole,
  };
}

interface Discovery {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  jwks_uri: string;
}

const discoveries = new Map<string, Promise<{ d: Discovery; jwks: ReturnType<typeof createRemoteJWKSet> }>>();

export function discover(issuer: string, fetcher: typeof fetch = fetch) {
  let p = discoveries.get(issuer);
  if (!p) {
    p = (async () => {
      const res = await fetcher(issuer.replace(/\/+$/, "") + "/.well-known/openid-configuration", {
        signal: AbortSignal.timeout(15_000),
      });
      if (!res.ok) throw new Error(`oidc discovery: ${res.status}`);
      const d = (await res.json()) as Discovery;
      if (d.issuer !== issuer)
        throw new Error(
          `oidc: issuer did not match the issuer returned by provider, expected "${issuer}" got "${d.issuer}"`,
        );
      return { d, jwks: createRemoteJWKSet(new URL(d.jwks_uri)) };
    })();
    p.catch(() => discoveries.delete(issuer));
    discoveries.set(issuer, p);
  }
  return p;
}

export interface OIDCFlow {
  s: string;
  n: string;
  v: string;
  r: string;
}

/** Starts a login: the flow to save and the authorization URL. */
export async function startLogin(c: OIDCConfig, returnTo: string): Promise<{ flow: OIDCFlow; url: string }> {
  const { d } = await discover(c.issuer);
  const flow: OIDCFlow = { s: randomToken(32), n: randomToken(32), v: randomToken(32), r: returnTo };
  const challenge = b64url(await sha256(utf8(flow.v)));
  const u = new URL(d.authorization_endpoint);
  u.searchParams.set("client_id", c.clientId);
  u.searchParams.set("redirect_uri", c.redirectUrl);
  u.searchParams.set("response_type", "code");
  u.searchParams.set("scope", c.scopes.join(" "));
  u.searchParams.set("state", flow.s);
  u.searchParams.set("nonce", flow.n);
  u.searchParams.set("code_challenge", challenge);
  u.searchParams.set("code_challenge_method", "S256");
  return { flow, url: u.toString() };
}

/** Finishes a login (exchange): verifies state, code, ID token, nonce and at_hash. */
export async function finishLogin(
  c: OIDCConfig,
  query: URLSearchParams,
  flow: OIDCFlow,
  nowMs?: number,
): Promise<Identity> {
  const err = query.get("error");
  if (err) {
    throw new LoginFailure(
      err === "access_denied" ? "access_denied" : "provider_error",
      `identity provider returned ${err}: ${query.get("error_description") ?? ""}`,
    );
  }
  const state = query.get("state") ?? "";
  if (state === "" || !timingSafeEqual(state, flow.s)) throw new LoginFailure("invalid_state", "state mismatch");
  const code = query.get("code") ?? "";
  if (!code) throw new LoginFailure("provider_error", "callback without code");
  let disc;
  try {
    disc = await discover(c.issuer);
  } catch (e) {
    throw new LoginFailure("provider_unavailable", (e as Error).message);
  }
  const form = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    redirect_uri: c.redirectUrl,
    code_verifier: flow.v,
  });
  const headers: Record<string, string> = {
    "content-type": "application/x-www-form-urlencoded",
    accept: "application/json",
  };
  if (c.clientSecret) {
    headers["authorization"] =
      "Basic " + btoa(`${encodeURIComponent(c.clientId)}:${encodeURIComponent(c.clientSecret)}`);
  } else form.set("client_id", c.clientId);
  const res = await fetch(disc.d.token_endpoint, {
    method: "POST",
    headers,
    body: form,
    signal: AbortSignal.timeout(15_000),
  });
  if (!res.ok) throw new LoginFailure("provider_error", `token exchange: ${res.status}`);
  const tok = (await res.json()) as { id_token?: string; access_token?: string };
  if (!tok.id_token) throw new LoginFailure("provider_error", "token response without id_token");
  let payload: JWTPayload;
  try {
    const opts: Parameters<typeof jwtVerify>[2] = { issuer: c.issuer, audience: c.clientId };
    if (nowMs !== undefined) opts.currentDate = new Date(nowMs);
    ({ payload } = await jwtVerify(tok.id_token, disc.jwks, opts));
  } catch (e) {
    throw new LoginFailure("provider_error", `verify id_token: ${(e as Error).message}`);
  }
  const nonce = typeof payload["nonce"] === "string" ? (payload["nonce"] as string) : "";
  if (nonce === "" || !timingSafeEqual(nonce, flow.n))
    throw new LoginFailure("nonce_mismatch", "id_token nonce does not match");
  const atHash = payload["at_hash"];
  if (typeof atHash === "string" && atHash !== "") {
    const h = await sha256(utf8(tok.access_token ?? ""));
    if (b64url(h.subarray(0, 16)) !== atHash)
      throw new LoginFailure("provider_error", "at_hash: access token hash does not match value in ID token");
  }
  return identityFromClaims(c, String(payload.sub ?? ""), payload as Record<string, unknown>);
}

/** SafeReturnTo: only local absolute paths. */
export function safeReturnTo(p: string | null | undefined): string {
  if (!p || !p.startsWith("/") || p.startsWith("//") || p.startsWith("/\\") || /[\r\n]/.test(p)) return "/";
  return p;
}
