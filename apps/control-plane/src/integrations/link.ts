// Account links between external identities and Open Factory users (port of
// internal/integrations/link.go): signed link tokens, the confirmation page,
// the provider sign-in proof, link by email and unlink.
import type { ExternalIdentity, User } from "@openfactory/core";
import { providerLabel } from "./shared";
import type { Prover } from "./prover";
import type { EmailCandidate, IntegrationStore, LinkerLike, Logger } from "./types";
import { consoleLogger } from "./types";
import { b64urlDecode, b64urlEncode } from "./jwt";
import { bytesEqual, hmacSha256 } from "./util";

export const LINK_PATH = "/integrations/link/";
export const LINK_START_PATH = "/integrations/link-start/";
export const LINK_CALLBACK_PATH = "/integrations/link-callback";
export const LINKS_PATH = "/integrations/links/";

export const AUDIT_LINK = "account.link";
export const AUDIT_UNLINK = "account.unlink";

/** The external account is linked to another user. A link is never replaced. */
export class LinkedToOtherError extends Error {
  override readonly name = "LinkedToOtherError";
  constructor() {
    super("the external account is linked to another user");
  }
}

export interface AuditRecord {
  action: string;
  outcome: "success" | "failure";
  actorId: string;
  userId: string;
  teamId: string;
  provider: string;
  externalId: string;
  /** provider_sign_in, private_message, email, admin or self. */
  method: string;
  reason: string;
}

export interface LinkerOptions {
  store: IntegrationStore;
  /** Signs link tokens, sign-in states and CSRF tokens. */
  key: string;
  /** OF_PUBLIC_URL. */
  publicUrl: string;
  /** The signed-in user of a dashboard request, or null. */
  currentUser: (req: Request) => Promise<User | null>;
  loginUrl?: (next: string) => string;
  /** The sign-in of a provider for a team, or null. */
  prover?: (teamId: string, provider: string) => Promise<Prover | null>;
  audit?: (rec: AuditRecord) => void | Promise<void>;
  ttlMs?: number;
  log?: Logger;
  now?: () => Date;
}

interface LinkClaims {
  t: string;
  p: string;
  x: string;
  h?: string;
  e: number;
  pm?: boolean;
}

interface StateClaims {
  k?: string;
  u: string;
  t: string;
  p: string;
  e: number;
  n: string;
}

const STATE_TTL_S = 15 * 60;

const esc = (s: string): string =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");

export class Linker implements LinkerLike {
  private readonly log: Logger;
  constructor(private readonly o: LinkerOptions) {
    this.log = o.log ?? consoleLogger;
  }

  private nowS(): number {
    return Math.floor((this.o.now ? this.o.now() : new Date()).getTime() / 1000);
  }

  private configured(): boolean {
    return this.o.key !== "" && this.o.publicUrl !== "";
  }

  private async mac(kind: string, payload: string): Promise<string> {
    return b64urlEncode(await hmacSha256(this.o.key, kind + ":" + payload));
  }

  private async seal(kind: string, v: unknown): Promise<string> {
    const payload = b64urlEncode(JSON.stringify(v));
    return payload + "." + (await this.mac(kind, payload));
  }

  private async open<T>(kind: string, tok: string): Promise<T> {
    const i = tok.indexOf(".");
    if (i < 0) throw new Error("invalid link");
    const payload = tok.slice(0, i);
    const sig = tok.slice(i + 1);
    const want = await this.mac(kind, payload);
    const enc = new TextEncoder();
    if (!bytesEqual(enc.encode(sig), enc.encode(want))) throw new Error("invalid link");
    try {
      return JSON.parse(new TextDecoder().decode(b64urlDecode(payload))) as T;
    } catch {
      throw new Error("invalid link");
    }
  }

  /**
   * A link URL for an external account, to send only in a private message to
   * that account. Returns "" when the linker is not configured.
   */
  async privateUrl(teamId: string, provider: string, externalId: string, handle: string): Promise<string> {
    if (!this.configured()) return "";
    const ttl = this.o.ttlMs && this.o.ttlMs > 0 ? this.o.ttlMs : 24 * 3600_000;
    const claims: LinkClaims = {
      t: teamId,
      p: provider,
      x: externalId,
      e: this.nowS() + Math.floor(ttl / 1000),
      pm: true,
      ...(handle ? { h: handle } : {}),
    };
    return this.o.publicUrl.replace(/\/+$/, "") + LINK_PATH + (await this.seal("link", claims));
  }

  /** The text of a link prompt in a public place: it has no link URL. */
  dashboardHint(provider: string): string {
    const where = this.o.publicUrl ? this.o.publicUrl.replace(/\/+$/, "") + "/integrations" : "the Open Factory dashboard";
    return `Sign in to ${where} and link your ${providerLabel(provider)} account under "Link your account".`;
  }

  private async parse(tok: string): Promise<LinkClaims> {
    const c = await this.open<LinkClaims>("link", tok);
    if (this.nowS() > c.e) throw new Error("this link has expired; request a new one");
    return c;
  }

  private csrf(userId: string, tok: string): Promise<string> {
    return this.mac("csrf", userId + ":" + tok);
  }

  /** Serves the account-link endpoints. Returns null for any other path. */
  async handle(req: Request): Promise<Response | null> {
    const url = new URL(req.url);
    const path = url.pathname;
    if (path.startsWith(LINK_PATH) && path.length > LINK_PATH.length) {
      const tok = decodeURIComponent(path.slice(LINK_PATH.length));
      if (req.method === "GET") return this.serveConfirm(req, tok);
      if (req.method === "POST") return this.serveComplete(req, tok);
    }
    if (path.startsWith(LINK_START_PATH) && req.method === "GET") {
      return this.serveStart(req, decodeURIComponent(path.slice(LINK_START_PATH.length)));
    }
    if (path === LINK_CALLBACK_PATH && req.method === "GET") return this.serveCallback(req, url);
    if (path.startsWith(LINKS_PATH) && req.method === "DELETE") {
      const [provider, ...rest] = path.slice(LINKS_PATH.length).split("/");
      if (provider && rest.length > 0) return this.serveRemove(req, decodeURIComponent(provider), decodeURIComponent(rest.join("/")));
    }
    return null;
  }

  private page(status: number, title: string, message: string, action?: { action: string; csrf: string; button: string }): Response {
    const form = action
      ? `<form method="post" action="${esc(action.action)}"><input type="hidden" name="csrf" value="${esc(action.csrf)}"><button type="submit">${esc(action.button)}</button></form>`
      : "";
    const html = `<!doctype html><html><head><meta charset="utf-8"><title>Open Factory</title>
<meta name="referrer" content="no-referrer">
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem}button{font:inherit;padding:.5rem 1rem}</style></head>
<body><h1>${esc(title)}</h1><p>${esc(message)}</p>
${form}
</body></html>`;
    return new Response(html, {
      status,
      headers: {
        "Content-Type": "text/html; charset=utf-8",
        "Cache-Control": "no-store",
        "X-Frame-Options": "DENY",
        "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'",
      },
    });
  }

  /** The signed-in user, or a response (login redirect or page). */
  private async signedIn(req: Request): Promise<{ user: User } | { res: Response }> {
    let user: User | null = null;
    try {
      user = await this.o.currentUser(req);
    } catch {
      user = null;
    }
    if (user) return { user };
    if (this.o.loginUrl && req.method === "GET") {
      const u = new URL(req.url);
      return { res: new Response(null, { status: 302, headers: { Location: this.o.loginUrl(u.pathname + u.search) } }) };
    }
    return { res: this.page(401, "Sign in required", "Sign in to Open Factory, then open this link again.") };
  }

  private async linkRequest(req: Request, tok: string): Promise<{ claims: LinkClaims; user: User } | { res: Response }> {
    let claims: LinkClaims;
    try {
      claims = await this.parse(tok);
    } catch (e) {
      return { res: this.page(400, "Link failed", (e as Error).message) };
    }
    const s = await this.signedIn(req);
    if ("res" in s) return s;
    if (s.user.team_id !== claims.t) {
      return { res: this.page(403, "Wrong team", "Your Open Factory account is not a member of the team this request belongs to.") };
    }
    return { claims, user: s.user };
  }

  private async proverOf(teamId: string, provider: string): Promise<Prover | null> {
    if (!this.o.prover) return null;
    try {
      return await this.o.prover(teamId, provider);
    } catch (e) {
      this.log.warn("account link: provider sign-in", { provider, err: (e as Error).message });
      return null;
    }
  }

  private async serveConfirm(req: Request, tok: string): Promise<Response> {
    const r = await this.linkRequest(req, tok);
    if ("res" in r) return r.res;
    const { claims, user } = r;
    const conflict = await this.ownerConflict(user, claims.p, claims.x);
    if (conflict) return this.page(conflict.status, "Link not possible", conflict.message);
    const label = providerLabel(claims.p);
    const who = claims.h || claims.x;
    const action = { action: LINK_PATH + tok, csrf: await this.csrf(user.id, tok), button: "" };
    if (await this.proverOf(claims.t, claims.p)) {
      action.button = "Continue with " + label;
      return this.page(
        200,
        `Link your ${label} account`,
        `Link the ${label} account ${who} to ${user.email}? You sign in to ${label} next, to show that you control the account.`,
        action,
      );
    }
    if (claims.pm) {
      action.button = "Link account";
      return this.page(
        200,
        `Link your ${label} account`,
        `Link the ${label} account ${who} to ${user.email}? Continue only if you asked for this link in ${label}.`,
        action,
      );
    }
    return this.page(403, "Link not possible", `This link cannot be proved: ${label} sign-in is not configured for your team. Ask a team admin.`);
  }

  private async serveComplete(req: Request, tok: string): Promise<Response> {
    const r = await this.linkRequest(req, tok);
    if ("res" in r) return r.res;
    const { claims, user } = r;
    let csrf = "";
    try {
      csrf = String((await req.formData()).get("csrf") ?? "");
    } catch {
      csrf = "";
    }
    const enc = new TextEncoder();
    if (!bytesEqual(enc.encode(csrf), enc.encode(await this.csrf(user.id, tok)))) {
      return this.page(403, "Link failed", "The request is not valid. Open the link again.");
    }
    const p = await this.proverOf(claims.t, claims.p);
    if (p) return this.redirectToProvider(p, { k: tok, u: user.id, t: claims.t, p: claims.p });
    if (!claims.pm) return this.page(403, "Link not possible", "This link cannot be proved. Ask a team admin.");
    return this.finish(user, claims.p, claims.x, claims.h ?? "", "private_message");
  }

  private async serveStart(req: Request, provider: string): Promise<Response> {
    const s = await this.signedIn(req);
    if ("res" in s) return s.res;
    const p = await this.proverOf(s.user.team_id, provider);
    if (!p) {
      return this.page(404, "Link not possible", `${providerLabel(provider)} sign-in is not configured for your team. Ask a team admin.`);
    }
    return this.redirectToProvider(p, { u: s.user.id, t: s.user.team_id, p: provider });
  }

  private redirectUri(): string {
    return this.o.publicUrl.replace(/\/+$/, "") + LINK_CALLBACK_PATH;
  }

  private async redirectToProvider(p: Prover, st: Omit<StateClaims, "n" | "e">): Promise<Response> {
    const n = crypto.getRandomValues(new Uint8Array(16));
    const full: StateClaims = { ...st, n: b64urlEncode(n), e: this.nowS() + STATE_TTL_S };
    return new Response(null, { status: 303, headers: { Location: p.authUrl(await this.seal("state", full), this.redirectUri()) } });
  }

  private async serveCallback(req: Request, url: URL): Promise<Response> {
    let st: StateClaims;
    try {
      st = await this.open<StateClaims>("state", url.searchParams.get("state") ?? "");
      if (this.nowS() > st.e) throw new Error("expired");
    } catch {
      return this.page(400, "Link failed", "The sign-in is not valid or has expired. Start the link again.");
    }
    const s = await this.signedIn(req);
    if ("res" in s) return s.res;
    const user = s.user;
    if (user.id !== st.u || user.team_id !== st.t) {
      return this.page(403, "Link failed", "The sign-in was started by another Open Factory account.");
    }
    if (url.searchParams.get("error")) return this.page(400, "Link failed", "The sign-in was not completed.");
    const p = await this.proverOf(st.t, st.p);
    if (!p) return this.page(400, "Link failed", "The sign-in of this provider is no longer configured.");
    let id = "";
    let handle = "";
    try {
      ({ externalId: id, handle } = await p.exchange(url.searchParams.get("code") ?? "", this.redirectUri()));
    } catch (e) {
      this.log.warn("account link: sign-in exchange", { provider: st.p, err: (e as Error).message });
    }
    if (id === "") return this.page(502, "Link failed", "The sign-in could not be checked. Try again later.");
    if (st.k) {
      let claims: LinkClaims;
      try {
        claims = await this.parse(st.k);
      } catch (e) {
        return this.page(400, "Link failed", (e as Error).message);
      }
      if (claims.x !== id || claims.p !== st.p) {
        await this.auditRec({
          action: AUDIT_LINK,
          outcome: "failure",
          actorId: user.id,
          userId: user.id,
          teamId: user.team_id,
          provider: st.p,
          externalId: claims.x,
          method: "provider_sign_in",
          reason: "account mismatch",
        });
        return this.page(403, "Link failed", `You signed in to ${providerLabel(st.p)} with a different account than the one in the link.`);
      }
      if (handle === "") handle = claims.h ?? "";
    }
    return this.finish(user, st.p, id, handle, "provider_sign_in");
  }

  private async finish(user: User, provider: string, externalId: string, handle: string, method: string): Promise<Response> {
    try {
      await this.link(user, provider, externalId, handle, method);
    } catch (e) {
      if (e instanceof LinkedToOtherError) {
        const c = await this.ownerConflict(user, provider, externalId);
        return this.page(c?.status ?? 409, "Link not possible", c?.message ?? "The account is linked to another user.");
      }
      this.log.error("link account", { provider, err: (e as Error).message });
      return this.page(500, "Link failed", "The account could not be linked. Try again later.");
    }
    return this.page(
      200,
      "Account linked",
      `Your ${providerLabel(provider)} account ${handle || externalId} is now linked to ${user.email}. Send your request again.`,
    );
  }

  /** A message and status when the external account is linked to another user. */
  private async ownerConflict(user: User, provider: string, externalId: string): Promise<{ message: string; status: number } | null> {
    const x = await this.o.store.findExternalIdentity(provider, externalId);
    if (!x || x.user_id === "" || x.user_id === user.id) return null;
    let owner = "another Open Factory account";
    const u = await this.o.store.getUser(x.user_id);
    if (u && u.team_id === user.team_id) owner = u.email;
    return {
      message: `This ${providerLabel(provider)} account is already linked to ${owner}. A link is never replaced: ask a team admin to remove the old link first.`,
      status: 409,
    };
  }

  private async link(user: User, provider: string, externalId: string, handle: string, method: string): Promise<void> {
    const rec: AuditRecord = {
      action: AUDIT_LINK,
      outcome: "success",
      actorId: user.id,
      userId: user.id,
      teamId: user.team_id,
      provider,
      externalId,
      method,
      reason: "",
    };
    try {
      await this.createLink({ user_id: user.id, provider, external_id: externalId, handle });
    } catch (e) {
      rec.outcome = "failure";
      rec.reason = (e as Error).message;
      await this.auditRec(rec);
      throw e;
    }
    await this.auditRec(rec);
  }

  private async createLink(x: ExternalIdentity): Promise<void> {
    if (!(await this.o.store.linkExternalIdentity(x))) throw new LinkedToOtherError();
  }

  private async auditRec(rec: AuditRecord): Promise<void> {
    if (this.o.audit) {
      await this.o.audit(rec);
      return;
    }
    this.log.info("audit", { ...rec });
  }

  private async serveRemove(req: Request, provider: string, externalId: string): Promise<Response> {
    const s = await this.signedIn(req);
    if ("res" in s) return s.res;
    const user = s.user;
    const x = await this.o.store.findExternalIdentity(provider, externalId);
    if (!x || x.user_id === "") return errorJson(404, "not_found", "no link for this account");
    const owner = await this.o.store.getUser(x.user_id);
    if (!owner || owner.team_id !== user.team_id || user.role.toLowerCase() !== "admin") {
      return errorJson(403, "not_authorized", "only an admin of the team can remove a link");
    }
    try {
      await this.unlink(provider, externalId, user.id);
    } catch {
      return errorJson(500, "internal", "remove the link");
    }
    return new Response(null, { status: 204 });
  }

  /** Removes the link of an external account (the identity is kept with no user) and writes an audit event. actorId "" means the account itself. */
  async unlink(provider: string, externalId: string, actorId: string): Promise<void> {
    const x = await this.o.store.findExternalIdentity(provider, externalId);
    if (!x || x.user_id === "") return;
    const rec: AuditRecord = {
      action: AUDIT_UNLINK,
      outcome: "success",
      actorId,
      userId: x.user_id,
      teamId: "",
      provider,
      externalId,
      method: actorId === "" ? "self" : "admin",
      reason: "",
    };
    const u = await this.o.store.getUser(x.user_id);
    if (u) rec.teamId = u.team_id;
    await this.o.store.upsertExternalIdentity({ ...x, user_id: "" });
    await this.auditRec(rec);
  }

  /**
   * Links an external account to the team member with the same email address.
   * The caller checks the opt-in setting. Never links a guest, a user of
   * another workspace, a bot or a deleted account, and never replaces a link.
   */
  async linkByEmail(teamId: string, c: EmailCandidate): Promise<User | null> {
    if (c.email === "" || c.guest || c.foreign || c.bot || c.deleted) return null;
    const u = await this.o.store.getUserByEmail(c.email);
    if (!u || u.team_id !== teamId) return null;
    const rec: AuditRecord = {
      action: AUDIT_LINK,
      outcome: "success",
      actorId: "",
      userId: u.id,
      teamId,
      provider: c.provider,
      externalId: c.externalId,
      method: "email",
      reason: "",
    };
    try {
      await this.createLink({ user_id: u.id, provider: c.provider, external_id: c.externalId, handle: c.handle });
    } catch (e) {
      rec.outcome = "failure";
      rec.reason = (e as Error).message;
      await this.auditRec(rec);
      if (e instanceof LinkedToOtherError) return null;
      throw e;
    }
    await this.auditRec(rec);
    return u;
  }
}

function errorJson(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ error: { code, message } }), { status, headers: { "Content-Type": "application/json" } });
}
