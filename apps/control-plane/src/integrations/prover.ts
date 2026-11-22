// Provider sign-in that proves control of an external account (port of prover.go).
import { encodeQuery, sameIntegrationProvider } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { ApiClient } from "./http";
import { NoIntegrationError } from "./select";
import type { Fetch, IntegrationStore, SecretReader } from "./types";
import { integrationSecret } from "./util";

export interface Prover {
  authUrl(state: string, redirectUri: string): string;
  /** Trades the code for the account ID and a display handle. */
  exchange(code: string, redirectUri: string): Promise<{ externalId: string; handle: string }>;
}

export interface OAuthProverConfig {
  authorizeUrl: string;
  tokenUrl: string;
  userInfoUrl: string;
  clientId: string;
  clientSecret: string;
  scopes?: string[];
  /** Key of the account ID in the user-info document. May be a dotted path. */
  idField: string;
  handleField?: string;
  scopeSeparator?: string;
  authParams?: Record<string, string>;
  tokenJson?: boolean;
  /** Makes the user-info request a POST with this JSON body (a GraphQL query). */
  userInfoBody?: string;
  fetch?: Fetch;
}

export class OAuthProver implements Prover {
  constructor(readonly c: OAuthProverConfig) {}

  authUrl(state: string, redirectUri: string): string {
    const q: Record<string, string> = {
      response_type: "code",
      client_id: this.c.clientId,
      redirect_uri: redirectUri,
      state,
    };
    if (this.c.scopes && this.c.scopes.length > 0) q["scope"] = this.c.scopes.join(this.c.scopeSeparator || " ");
    for (const [k, v] of Object.entries(this.c.authParams ?? {})) q[k] = v;
    const sep = this.c.authorizeUrl.includes("?") ? "&" : "?";
    return this.c.authorizeUrl + sep + encodeQuery(q);
  }

  async exchange(code: string, redirectUri: string): Promise<{ externalId: string; handle: string }> {
    if (code === "") throw new Error("no authorization code");
    const form: Record<string, string> = {
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri,
      client_id: this.c.clientId,
      client_secret: this.c.clientSecret,
    };
    const api = new ApiClient({ fetch: this.c.fetch, timeoutMs: 20_000 });
    let tok: { access_token?: string; error?: string } | undefined;
    try {
      tok = await api.json("POST", this.c.tokenUrl, this.c.tokenJson ? form : new URLSearchParams(form));
    } catch (e) {
      throw new Error(`token request: ${(e as Error).message}`, { cause: e });
    }
    if (!tok?.access_token) throw new Error(`token request: no access token (${tok?.error ?? ""})`);
    const bearer = new ApiClient({
      fetch: this.c.fetch,
      timeoutMs: 20_000,
      authorize: (h) => h.set("Authorization", "Bearer " + tok.access_token),
    });
    let info: Record<string, unknown> | undefined;
    try {
      info = this.c.userInfoBody
        ? await bearer.json("POST", this.c.userInfoUrl, this.c.userInfoBody)
        : await bearer.json("GET", this.c.userInfoUrl);
    } catch (e) {
      throw new Error(`user info: ${(e as Error).message}`, { cause: e });
    }
    const id = fieldString(info ?? {}, this.c.idField);
    if (id === "") throw new Error(`user info: no "${this.c.idField}"`);
    return { externalId: id, handle: fieldString(info ?? {}, this.c.handleField ?? "") };
  }
}

/**
 * A field of a JSON object as a string. A number is written without a
 * fraction. The key is a field name, or a dotted path. A field name wins over
 * a path.
 */
export function fieldString(m: Record<string, unknown>, key: string): string {
  if (key === "") return "";
  let cur: Record<string, unknown> = m;
  let rest = key;
  let val = cur[rest];
  while (val === undefined && rest.includes(".")) {
    const i = rest.indexOf(".");
    const head = rest.slice(0, i);
    const sub = cur[head];
    if (typeof sub !== "object" || sub === null || Array.isArray(sub)) break;
    cur = sub as Record<string, unknown>;
    rest = rest.slice(i + 1);
    val = cur[rest];
  }
  if (typeof val === "string") return val;
  if (typeof val === "number") return String(val);
  return "";
}

export const CONFIG_OAUTH_CLIENT_ID = "oauth_client_id";
export const CONFIG_OAUTH_CLIENT_SECRET_REF = "oauth_client_secret_ref";
export const CONFIG_OAUTH_AUTHORIZE_URL = "oauth_authorize_url";
export const CONFIG_OAUTH_TOKEN_URL = "oauth_token_url";
export const CONFIG_OAUTH_USERINFO_URL = "oauth_userinfo_url";
export const CONFIG_OAUTH_ID_FIELD = "oauth_id_field";
export const CONFIG_OAUTH_HANDLE_FIELD = "oauth_handle_field";
export const CONFIG_OAUTH_SCOPES = "oauth_scopes";

/**
 * Builds the sign-in of a provider from an integration record. GitHub, Slack,
 * Microsoft Teams, Linear and Jira have default endpoints and scopes; the
 * oauth_* keys override them. Returns null when the record has no client ID.
 */
export async function proverFromIntegration(
  secrets: SecretReader | undefined,
  i: Integration,
  fetchFn?: Fetch,
): Promise<Prover | null> {
  const cfg = i.config ?? {};
  if (!cfg[CONFIG_OAUTH_CLIENT_ID]) return null;
  const secret = await integrationSecret(secrets, i, CONFIG_OAUTH_CLIENT_SECRET_REF);
  const p: OAuthProverConfig = {
    authorizeUrl: "",
    tokenUrl: "",
    userInfoUrl: "",
    idField: "",
    clientId: cfg[CONFIG_OAUTH_CLIENT_ID]!,
    clientSecret: secret,
    fetch: fetchFn,
  };
  switch (i.provider.toLowerCase()) {
    case "github": {
      const web = (cfg["web_url"] ?? "").replace(/\/+$/, "") || "https://github.com";
      const api = (cfg["base_url"] ?? "").replace(/\/+$/, "") || "https://api.github.com";
      p.authorizeUrl = web + "/login/oauth/authorize";
      p.tokenUrl = web + "/login/oauth/access_token";
      p.userInfoUrl = api + "/user";
      p.idField = "id";
      p.handleField = "login";
      break;
    }
    case "slack":
      p.authorizeUrl = "https://slack.com/openid/connect/authorize";
      p.tokenUrl = "https://slack.com/api/openid.connect.token";
      p.userInfoUrl = "https://slack.com/api/openid.connect.userInfo";
      p.idField = "https://slack.com/user_id";
      p.handleField = "name";
      p.scopes = ["openid", "profile"];
      break;
    case "microsoft_teams":
    case "teams": {
      const tenant = (cfg["tenant_id"] ?? "").trim() || "organizations";
      const login = "https://login.microsoftonline.com/" + encodeURIComponent(tenant) + "/oauth2/v2.0";
      p.authorizeUrl = login + "/authorize";
      p.tokenUrl = login + "/token";
      p.userInfoUrl = "https://graph.microsoft.com/v1.0/me";
      p.idField = "id";
      p.handleField = "displayName";
      p.scopes = ["openid", "profile", "User.Read"];
      break;
    }
    case "linear":
      p.authorizeUrl = "https://linear.app/oauth/authorize";
      p.tokenUrl = "https://api.linear.app/oauth/token";
      p.userInfoUrl = "https://api.linear.app/graphql";
      p.userInfoBody = '{"query":"{ viewer { id name } }"}';
      p.idField = "data.viewer.id";
      p.handleField = "data.viewer.name";
      p.scopes = ["read"];
      p.scopeSeparator = ",";
      break;
    case "jira":
      p.authorizeUrl = "https://auth.atlassian.com/authorize";
      p.tokenUrl = "https://auth.atlassian.com/oauth/token";
      p.userInfoUrl = "https://api.atlassian.com/me";
      p.idField = "account_id";
      p.handleField = "name";
      p.scopes = ["read:me"];
      p.authParams = { audience: "api.atlassian.com", prompt: "consent" };
      p.tokenJson = true;
      break;
  }
  const over = (key: string, set: (v: string) => void): void => {
    const v = (cfg[key] ?? "").trim();
    if (v !== "") set(v);
  };
  over(CONFIG_OAUTH_AUTHORIZE_URL, (v) => (p.authorizeUrl = v));
  over(CONFIG_OAUTH_TOKEN_URL, (v) => (p.tokenUrl = v));
  over(CONFIG_OAUTH_USERINFO_URL, (v) => (p.userInfoUrl = v));
  over(CONFIG_OAUTH_ID_FIELD, (v) => (p.idField = v));
  over(CONFIG_OAUTH_HANDLE_FIELD, (v) => (p.handleField = v));
  over(CONFIG_OAUTH_SCOPES, (v) => (p.scopes = v.split(/[, ]+/).filter((s) => s !== "")));
  if (!p.authorizeUrl || !p.tokenUrl || !p.userInfoUrl || !p.idField) {
    throw new Error(
      `integration ${i.id}: the ${i.provider} sign-in needs ${CONFIG_OAUTH_AUTHORIZE_URL}, ${CONFIG_OAUTH_TOKEN_URL}, ${CONFIG_OAUTH_USERINFO_URL} and ${CONFIG_OAUTH_ID_FIELD}`,
    );
  }
  return new OAuthProver(p);
}

/**
 * The sign-in of a provider for a team, from the record that selectSignIn
 * selects. An account link belongs to the team, so it selects with no factory.
 * Returns null when no record has a sign-in.
 */
export async function teamProver(
  store: IntegrationStore,
  secrets: SecretReader | undefined,
  teamId: string,
  provider: string,
  fetchFn?: Fetch,
): Promise<Prover | null> {
  const ints = await store.listIntegrations(teamId);
  const i = selectSignIn(ints, provider, "");
  if (!i) return null;
  if ((i.status || "active") !== "active") {
    throw new Error(`${provider}: the integration record ${i.id} has the sign-in and is not active (${i.status})`);
  }
  return proverFromIntegration(secrets, i, fetchFn);
}

/**
 * Selects the record that holds the sign-in of a provider (oauth_client_id).
 * The record of factoryId wins over the team record. With no factory, a team
 * with no team record uses its one factory record. Two records at one level
 * are an error. Never picks by order.
 */
export function selectSignIn(ints: Integration[], provider: string, factoryId: string): Integration | null {
  const fac: Integration[] = [];
  const team: Integration[] = [];
  const other: Integration[] = [];
  for (const i of ints) {
    if (!sameIntegrationProvider(i.provider, provider) || (i.config?.[CONFIG_OAUTH_CLIENT_ID] ?? "").trim() === "") continue;
    const f = i.factory_id ?? "";
    if (f === "") team.push(i);
    else if (factoryId !== "" && f === factoryId) fac.push(i);
    else if (factoryId === "") other.push(i);
  }
  for (const level of [fac, team, other]) {
    if (level.length === 0) continue;
    if (level.length === 1) return level[0]!;
    throw new NoIntegrationError(
      `${provider}: ${level.length} integration records (${level.map((i) => i.id).join(", ")}) have a sign-in (${CONFIG_OAUTH_CLIENT_ID}): keep it on one record`,
    );
  }
  return null;
}
