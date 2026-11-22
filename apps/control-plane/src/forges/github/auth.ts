// GitHub authentication: a personal access token, or a GitHub App that
// authenticates per installation. App JWTs are RS256 with WebCrypto;
// installation tokens are cached per isolate (and optionally in a shared
// TokenCache) until 10 minutes before they expire.
import { pathEscape, statusCode, type ApiClient } from "@openfactory/core";
import { parseRSAPrivateKey, signRS256 } from "../util/jwt";
import { repoPath, wrap } from "./rest";

/** The least life a cached installation token must have left. */
export const TOKEN_MIN_LIFE_MS = 10 * 60_000;

/** Shared installation-token cache (for example TeamDO storage). */
export interface TokenCache {
  get(key: string): Promise<{ token: string; expiresMs: number } | null> | { token: string; expiresMs: number } | null;
  set(key: string, token: string, expiresMs: number): Promise<void> | void;
}

const isolateTokens = new Map<string, { token: string; expiresMs: number }>();
const isolateInstallations = new Map<string, number>();

/** Clears the isolate caches (tests). */
export function resetGitHubCaches(): void {
  isolateTokens.clear();
  isolateInstallations.clear();
}

/** How the client gets the credential of an API or git call. */
export interface GitHubAuthStrategy {
  readonly kind: "pat" | "app";
  /** The scheme in front of the token in the authorization header. */
  readonly scheme: "token " | "Bearer ";
  /**
   * The token for owner/repo. scope lists the repository names of the owner
   * that the token may reach; undefined = no limit.
   */
  token(owner: string, repo: string, scope: readonly string[] | undefined): Promise<string>;
  /** Reports an error of an authenticated API call. */
  callFailed(err: unknown): void;
}

type AuthFailure = ((err: unknown) => void) | undefined;

/** Calls onAuthFailure when GitHub refuses the credential itself (401, 403, 404). */
function reportAuthFailure(onAuthFailure: AuthFailure, err: unknown): void {
  if (!onAuthFailure) return;
  const s = statusCode(err);
  if (s === 401 || s === 403 || s === 404) onAuthFailure(err);
}

/** A personal access token. A 401 on any call reports an auth failure. */
export class PatAuth implements GitHubAuthStrategy {
  readonly kind = "pat";
  readonly scheme = "token ";

  constructor(
    private readonly pat: string,
    private readonly onAuthFailure?: (err: unknown) => void,
  ) {}

  async token(): Promise<string> {
    return this.pat;
  }

  callFailed(err: unknown): void {
    if (statusCode(err) === 401) reportAuthFailure(this.onAuthFailure, err);
  }
}

export interface AppAuthConfig {
  api: ApiClient;
  /** REST root, part of the cache keys. */
  baseURL: string;
  appId: string;
  /** PEM, PKCS#1 or PKCS#8. */
  privateKey: string;
  /** Pins the installation; 0 discovers it per repository owner. */
  installationId?: number;
  now: () => number;
  onAuthFailure?: (err: unknown) => void;
  tokenCache?: TokenCache;
}

/** A GitHub App: installation tokens requested with an App JWT. */
export class AppInstallationAuth implements GitHubAuthStrategy {
  readonly kind = "app";
  readonly scheme = "Bearer ";
  private key: Promise<CryptoKey> | undefined;

  constructor(private readonly cfg: AppAuthConfig) {}

  appKey(): Promise<CryptoKey> {
    this.key ??= parseRSAPrivateKey(this.cfg.privateKey);
    return this.key;
  }

  /** A short-lived JWT that authenticates as the App itself. */
  async appJWT(): Promise<string> {
    const key = await this.appKey();
    const now = Math.floor(this.cfg.now() / 1000);
    return signRS256(key, { iat: now - 60, exp: now + 9 * 60, iss: this.cfg.appId });
  }

  async token(owner: string, repo: string, scope: readonly string[] | undefined): Promise<string> {
    let inst: number;
    try {
      inst = await this.installationId(owner, repo);
    } catch (e) {
      if (statusCode(e) === 401) reportAuthFailure(this.cfg.onAuthFailure, e);
      throw e;
    }
    const scopeKey = scope
      ? scope
          .map((s) => s.toLowerCase())
          .sort()
          .join(",")
      : "*";
    const key = `${this.cfg.baseURL}|${this.cfg.appId}|${inst}|${scopeKey}`;
    const fresh = (t: { token: string; expiresMs: number } | null | undefined) =>
      t && this.cfg.now() < t.expiresMs - TOKEN_MIN_LIFE_MS ? t.token : null;
    const local = fresh(isolateTokens.get(key));
    if (local) return local;
    if (this.cfg.tokenCache) {
      const shared = await this.cfg.tokenCache.get(key);
      const tok = fresh(shared);
      if (tok && shared) {
        isolateTokens.set(key, shared);
        return tok;
      }
    }
    const jwt = await this.appJWT();
    const body: Record<string, unknown> = {};
    if (scope) body["repositories"] = [...scope];
    let resp: { token?: string; expires_at?: string } | undefined;
    try {
      resp = await this.cfg.api.json("POST", `/app/installations/${inst}/access_tokens`, body, "Bearer " + jwt);
    } catch (e) {
      reportAuthFailure(this.cfg.onAuthFailure, e);
      throw wrap("github: create installation token", e);
    }
    if (!resp?.token) throw new Error("github: empty installation token");
    let expiresMs = resp.expires_at ? Date.parse(resp.expires_at) : NaN;
    if (!Number.isFinite(expiresMs)) expiresMs = this.cfg.now() + 55 * 60_000;
    const entry = { token: resp.token, expiresMs };
    isolateTokens.set(key, entry);
    if (this.cfg.tokenCache) await this.cfg.tokenCache.set(key, entry.token, entry.expiresMs);
    return resp.token;
  }

  /** App calls report auth failures only from the token requests. */
  callFailed(): void {}

  private async installationId(owner: string, repo: string): Promise<number> {
    if (this.cfg.installationId) return this.cfg.installationId;
    const key = `${this.cfg.baseURL}|${this.cfg.appId}|${owner.toLowerCase()}`;
    const hit = isolateInstallations.get(key);
    if (hit !== undefined) return hit;
    const jwt = await this.appJWT();
    const path =
      repo === "" ? "/users/" + pathEscape(owner) + "/installation" : repoPath(owner, repo) + "/installation";
    let resp: { id?: number } | undefined;
    try {
      resp = await this.cfg.api.json("GET", path, undefined, "Bearer " + jwt);
    } catch (e) {
      throw wrap(`github: find app installation for ${owner}`, e);
    }
    const id = resp?.id ?? 0;
    isolateInstallations.set(key, id);
    return id;
  }
}

/** The installation_id of an integration record: "" = 0, discover it per owner. */
export function parseInstallationId(s: string | undefined): number {
  const t = (s ?? "").trim();
  if (t === "") return 0;
  if (!/^[+-]?\d+$/.test(t)) throw new Error(`"${t}" is not an integer`);
  const n = Number(t);
  if (!Number.isSafeInteger(n)) throw new Error(`"${t}" is out of range`);
  return n;
}
