// Bot Framework authentication: validation of Bot Connector tokens (OpenID
// metadata + JWKS) and client-credentials tokens for outbound calls.
// Port of teams/auth.go.
import { ApiClient } from "../http";
import { importRsaJwk, parseJwt, type Jwk } from "../jwt";
import type { Fetch } from "../types";

export const DEFAULT_OPENID_METADATA_URL = "https://login.botframework.com/v1/.well-known/openidconfiguration";
export const BOT_FRAMEWORK_ISSUER = "https://api.botframework.com";
export const DEFAULT_TOKEN_TENANT = "botframework.com";
export const DEFAULT_LOGIN_URL = "https://login.microsoftonline.com";
export const BOT_CONNECTOR_SCOPE = "https://api.botframework.com/.default";
export const GRAPH_SCOPE = "https://graph.microsoft.com/.default";
const CLOCK_SKEW_MS = 5 * 60_000;

interface KeyEntry {
  pub: CryptoKey;
  endorsements: string[];
}

export class KeySet {
  private keys: Map<string, KeyEntry> | null = null;
  private fetched = 0;
  constructor(private readonly o: { metadataUrl?: string; fetch?: Fetch; now?: () => number } = {}) {}

  private now(): number {
    return this.o.now ? this.o.now() : Date.now();
  }

  private async refresh(): Promise<void> {
    const c = new ApiClient({ fetch: this.o.fetch, userAgent: "openfactory-teams" });
    let meta: { jwks_uri?: string } | undefined;
    try {
      meta = await c.json("GET", this.o.metadataUrl || DEFAULT_OPENID_METADATA_URL);
    } catch (e) {
      throw new Error(`teams: openid metadata: ${(e as Error).message}`, { cause: e });
    }
    if (!meta?.jwks_uri) throw new Error("teams: openid metadata has no jwks_uri");
    let jwks: { keys?: Jwk[] } | undefined;
    try {
      jwks = await c.json("GET", meta.jwks_uri);
    } catch (e) {
      throw new Error(`teams: jwks: ${(e as Error).message}`, { cause: e });
    }
    const keys = new Map<string, KeyEntry>();
    for (const j of jwks?.keys ?? []) {
      try {
        keys.set(j.kid, { pub: await importRsaJwk(j), endorsements: j.endorsements ?? [] });
      } catch {
        /* skip a key that does not import */
      }
    }
    this.keys = keys;
    this.fetched = this.now();
  }

  private async key(kid: string): Promise<KeyEntry> {
    const known = this.keys?.get(kid);
    const age = this.now() - this.fetched;
    const stale = age > 24 * 3600_000;
    const recent = age < 60_000;
    if (known && !stale) return known;
    if (!known && recent && !stale && this.keys !== null) throw new Error(`teams: unknown signing key "${kid}"`);
    try {
      await this.refresh();
    } catch (e) {
      if (known) return known; // keep a known key if the metadata is unavailable
      throw e;
    }
    const e = this.keys?.get(kid);
    if (!e) throw new Error(`teams: unknown signing key "${kid}"`);
    return e;
  }

  /**
   * Checks an "Authorization: Bearer ..." header from the Bot Connector: the
   * RS256 signature from the published keys, issuer, audience (the bot app
   * ID), lifetime, channel endorsement and the serviceUrl claim.
   */
  async validate(authHeader: string, appIds: string[], channelId: string, serviceUrl: string): Promise<void> {
    if (!authHeader.startsWith("Bearer ") || authHeader.length === 7) throw new Error("teams: missing bearer token");
    const j = parseJwt(authHeader.slice(7));
    const e = await this.key(j.headerString("kid"));
    await j.verifyRS256(e.pub);
    const iss = j.claimString("iss");
    if (iss !== BOT_FRAMEWORK_ISSUER) throw new Error(`teams: unexpected issuer "${iss}"`);
    if (!appIds.some((a) => a !== "" && j.hasAudience(a)))
      throw new Error("teams: token audience does not match the bot app id");
    j.verifyTime(this.now(), CLOCK_SKEW_MS);
    if (e.endorsements.length > 0 && channelId !== "") {
      if (!e.endorsements.some((en) => en.toLowerCase() === channelId.toLowerCase())) {
        throw new Error(`teams: signing key not endorsed for channel "${channelId}"`);
      }
    }
    const su = j.claimString("serviceurl");
    if (
      su !== "" &&
      serviceUrl !== "" &&
      su.replace(/\/+$/, "").toLowerCase() !== serviceUrl.replace(/\/+$/, "").toLowerCase()
    ) {
      throw new Error("teams: serviceUrl claim does not match the activity");
    }
  }
}

export interface TokenSourceOptions {
  appId: string;
  appPassword: string;
  /** "botframework.com" for multi-tenant bots, or the Entra tenant ID of a single-tenant bot. */
  tenant?: string;
  /** Tried when a token request against tenant fails. On success it becomes the tenant. */
  fallbackTenant?: string;
  scope?: string;
  loginUrl?: string;
  fetch?: Fetch;
  now?: () => number;
}

/** Bot Connector access tokens with the OAuth client-credentials grant. */
export class TokenSource {
  private token = "";
  private expires = 0;
  private tenant: string;
  private fallback: string;
  constructor(readonly o: TokenSourceOptions) {
    this.tenant = o.tenant ?? "";
    this.fallback = o.fallbackTenant ?? "";
  }

  get appPassword(): string {
    return this.o.appPassword;
  }

  private now(): number {
    return this.o.now ? this.o.now() : Date.now();
  }

  /** A cached or fresh access token. */
  async getToken(): Promise<string> {
    if (this.token !== "" && this.now() < this.expires) return this.token;
    const tenant = this.tenant || DEFAULT_TOKEN_TENANT;
    const fallback = this.fallback;
    let res: { tok: string; ttlMs: number };
    try {
      res = await this.fetchToken(tenant);
    } catch (err) {
      if (fallback === "" || fallback.toLowerCase() === tenant.toLowerCase()) throw err;
      try {
        res = await this.fetchToken(fallback);
      } catch (ferr) {
        throw new Error(`${(err as Error).message} (tenant ${fallback}: ${(ferr as Error).message})`, { cause: ferr });
      }
      this.tenant = fallback;
      this.fallback = "";
    }
    this.token = res.tok;
    this.expires = this.now() + res.ttlMs - 5 * 60_000;
    return res.tok;
  }

  tokenTenant(): string {
    return this.tenant || DEFAULT_TOKEN_TENANT;
  }

  private async fetchToken(tenant: string): Promise<{ tok: string; ttlMs: number }> {
    const login = (this.o.loginUrl || DEFAULT_LOGIN_URL).replace(/\/+$/, "");
    const c = new ApiClient({ fetch: this.o.fetch, userAgent: "openfactory-teams" });
    const form = new URLSearchParams({
      grant_type: "client_credentials",
      client_id: this.o.appId,
      client_secret: this.o.appPassword,
      scope: this.o.scope || BOT_CONNECTOR_SCOPE,
    });
    let resp: { access_token?: string; expires_in?: number } | undefined;
    try {
      resp = await c.json("POST", `${login}/${encodeURIComponent(tenant)}/oauth2/v2.0/token`, form);
    } catch (e) {
      throw new Error(`teams: bot token (tenant ${tenant}): ${(e as Error).message}`, { cause: e });
    }
    if (!resp?.access_token) throw new Error("teams: empty bot token");
    const ttlMs = resp.expires_in && resp.expires_in > 0 ? resp.expires_in * 1000 : 3600_000;
    return { tok: resp.access_token, ttlMs };
  }
}
