import { describe, expect, it } from "vitest";
import { Linker, type AuditRecord } from "./link";
import { OAuthProver, fieldString, proverFromIntegration, selectSignIn } from "./prover";
import { FakeFetch, FakeSecrets, FakeStore, NOW, integration, user } from "./testkit";
import type { User } from "@openfactory/core";

function setup(opts: { prover?: boolean; admin?: boolean } = {}) {
  const store = new FakeStore();
  const alice = user({ id: "usr_a", email: "alice@example.com" });
  store.users = [alice, user({ id: "usr_b", email: "bob@example.com", role: opts.admin ? "admin" : "editor" })];
  let current: User | null = alice;
  const audit: AuditRecord[] = [];
  const ff = new FakeFetch();
  const linker = new Linker({
    store,
    key: "k",
    publicUrl: "https://of.example.com/",
    currentUser: async () => current,
    loginUrl: (next) => "/login?next=" + encodeURIComponent(next),
    audit: (r) => void audit.push(r),
    now: () => NOW,
    prover: async () =>
      opts.prover
        ? new OAuthProver({
            authorizeUrl: "https://idp.example.com/authorize",
            tokenUrl: "https://idp.example.com/token",
            userInfoUrl: "https://idp.example.com/me",
            clientId: "cid",
            clientSecret: "sec",
            idField: "id",
            handleField: "login",
            fetch: ff.fetch,
          })
        : null,
  });
  return { store, linker, audit, ff, setUser: (u: User | null) => (current = u) };
}

const tokenOf = (url: string): string => url.slice(url.lastIndexOf("/") + 1);
const formReq = (url: string, csrf: string) =>
  new Request(url, { method: "POST", body: new URLSearchParams({ csrf }) });
const csrfOf = (html: string): string => /name="csrf" value="([^"]+)"/.exec(html)![1]!;

describe("Linker private link without a provider sign-in", () => {
  it("shows a confirmation on GET and links on POST with the CSRF token", async () => {
    const t = setup();
    const url = await t.linker.privateUrl("team_1", "slack", "U1", "alice");
    expect(url.startsWith("https://of.example.com/integrations/link/")).toBe(true);
    const path = new URL(url).pathname;
    const get = (await t.linker.handle(new Request("https://of.example.com" + path)))!;
    expect(get.status).toBe(200);
    expect(get.headers.get("Content-Security-Policy")).toContain("default-src 'none'");
    const html = await get.text();
    expect(html).toContain("alice@example.com");
    expect(t.store.identities.size).toBe(0); // a GET never links
    const bad = (await t.linker.handle(formReq("https://of.example.com" + path, "wrong")))!;
    expect(bad.status).toBe(403);
    const ok = (await t.linker.handle(formReq("https://of.example.com" + path, csrfOf(html))))!;
    expect(ok.status).toBe(200);
    expect(t.store.identities.get("slack|U1")).toEqual({
      user_id: "usr_a",
      provider: "slack",
      external_id: "U1",
      handle: "alice",
    });
    expect(t.audit.map((a) => [a.action, a.outcome, a.method])).toEqual([
      ["account.link", "success", "private_message"],
    ]);
  });

  it("rejects a tampered token, an expired token, a wrong team and a signed-out visitor", async () => {
    const t = setup();
    const url = await t.linker.privateUrl("team_1", "slack", "U1", "");
    const tok = tokenOf(url);
    expect((await t.linker.handle(new Request("https://x/integrations/link/" + tok.slice(0, -2) + "xx")))!.status).toBe(
      400,
    );
    t.setUser(user({ id: "usr_z", team_id: "team_9" }));
    expect((await t.linker.handle(new Request("https://x/integrations/link/" + tok)))!.status).toBe(403);
    t.setUser(null);
    const redirect = (await t.linker.handle(new Request("https://x/integrations/link/" + tok)))!;
    expect(redirect.status).toBe(302);
    expect(redirect.headers.get("Location")).toContain("/login?next=");
    const later = new Linker({
      store: t.store,
      key: "k",
      publicUrl: "https://of.example.com",
      currentUser: async () => user(),
      now: () => new Date(NOW.getTime() + 25 * 3600_000),
    });
    expect((await later.handle(new Request("https://x/integrations/link/" + tok)))!.status).toBe(400);
  });

  it("never replaces a link of another user", async () => {
    const t = setup();
    t.store.identities.set("slack|U1", { user_id: "usr_b", provider: "slack", external_id: "U1", handle: "" });
    const path = new URL(await t.linker.privateUrl("team_1", "slack", "U1", "")).pathname;
    const res = (await t.linker.handle(new Request("https://x" + path)))!;
    expect(res.status).toBe(409);
    expect(await res.text()).toContain("bob@example.com");
  });
});

describe("Linker with a provider sign-in", () => {
  it("proves the account through the callback and rejects another account", async () => {
    const t = setup({ prover: true });
    let acct = 77;
    t.ff.on("/token", () => ({ access_token: "tok" })).on("/me", () => ({ id: acct, login: "al" }));
    const path = new URL(await t.linker.privateUrl("team_1", "github", "77", "")).pathname;
    const html = await (await t.linker.handle(new Request("https://x" + path)))!.text();
    expect(html).toContain("Continue with GitHub");
    const redirect = (await t.linker.handle(formReq("https://x" + path, csrfOf(html))))!;
    expect(redirect.status).toBe(303);
    const loc = new URL(redirect.headers.get("Location")!);
    expect(loc.origin + loc.pathname).toBe("https://idp.example.com/authorize");
    expect(loc.searchParams.get("redirect_uri")).toBe("https://of.example.com/integrations/link-callback");
    const state = loc.searchParams.get("state")!;
    const done = (await t.linker.handle(new Request(`https://x/integrations/link-callback?code=c1&state=${state}`)))!;
    expect(done.status).toBe(200);
    expect(t.store.identities.get("github|77")?.handle).toBe("al");
    expect(t.audit.at(-1)?.method).toBe("provider_sign_in");
    // the second flow returns another account ID
    t.store.identities.clear();
    t.ff.calls.length = 0;
    acct = 99;
    const html2 = await (await t.linker.handle(new Request("https://x" + path)))!.text();
    const loc2 = new URL((await t.linker.handle(formReq("https://x" + path, csrfOf(html2))))!.headers.get("Location")!);
    const mismatch = (await t.linker.handle(
      new Request(`https://x/integrations/link-callback?code=c1&state=${loc2.searchParams.get("state")}`),
    ))!;
    expect(mismatch.status).toBe(403);
    expect(t.store.identities.size).toBe(0);
    expect(t.audit.at(-1)).toMatchObject({ outcome: "failure", reason: "account mismatch" });
  });

  it("rejects a forged state", async () => {
    const t = setup({ prover: true });
    expect(
      (await t.linker.handle(new Request("https://x/integrations/link-callback?state=abc.def&code=c")))!.status,
    ).toBe(400);
  });
});

describe("unlink, link by email and removal", () => {
  it("unlink keeps the identity without a user and audits", async () => {
    const t = setup();
    t.store.identities.set("teams|X", { user_id: "usr_a", provider: "teams", external_id: "X", handle: "h" });
    await t.linker.unlink("teams", "X", "");
    expect(t.store.identities.get("teams|X")?.user_id).toBe("");
    expect(t.audit[0]).toMatchObject({ action: "account.unlink", method: "self", teamId: "team_1" });
  });

  it("links by email only for eligible accounts and never replaces a link", async () => {
    const t = setup();
    const c = { provider: "slack", externalId: "U5", handle: "al", email: "Alice@example.com" };
    for (const bad of [{ guest: true }, { foreign: true }, { bot: true }, { deleted: true }, { email: "" }]) {
      expect(await t.linker.linkByEmail("team_1", { ...c, ...bad })).toBeNull();
    }
    expect(await t.linker.linkByEmail("team_2", c)).toBeNull();
    expect((await t.linker.linkByEmail("team_1", c))?.id).toBe("usr_a");
    t.store.identities.set("slack|U6", { user_id: "usr_b", provider: "slack", external_id: "U6", handle: "" });
    expect(await t.linker.linkByEmail("team_1", { ...c, externalId: "U6" })).toBeNull();
    expect(t.store.identities.get("slack|U6")?.user_id).toBe("usr_b");
  });

  it("only an admin of the team removes a link", async () => {
    const t = setup();
    t.store.identities.set("slack|U1", { user_id: "usr_a", provider: "slack", external_id: "U1", handle: "" });
    const del = () => t.linker.handle(new Request("https://x/integrations/links/slack/U1", { method: "DELETE" }));
    expect((await del())!.status).toBe(403);
    t.setUser(user({ id: "usr_b", role: "Admin" }));
    expect((await del())!.status).toBe(204);
    expect(t.store.identities.get("slack|U1")?.user_id).toBe("");
    expect((await del())!.status).toBe(404);
  });

  it("dashboardHint names the dashboard", () => {
    expect(setup().linker.dashboardHint("slack")).toBe(
      'Sign in to https://of.example.com/integrations and link your Slack account under "Link your account".',
    );
  });
});

describe("prover defaults", () => {
  const secrets = new FakeSecrets({ CS: "client-secret" });
  const rec = (provider: string, config: Record<string, string> = {}) =>
    integration({
      id: "int_1",
      provider,
      config: { oauth_client_id: "cid", oauth_client_secret_ref: "CS", ...config },
    });

  it("builds the authorization URL of each provider", async () => {
    const url = async (provider: string, config?: Record<string, string>) =>
      new URL((await proverFromIntegration(secrets, rec(provider, config)))!.authUrl("st", "https://of/cb"));
    const gh = await url("github");
    expect(gh.origin + gh.pathname).toBe("https://github.com/login/oauth/authorize");
    const slack = await url("slack");
    expect(slack.searchParams.get("scope")).toBe("openid profile");
    const teams = await url("microsoft_teams", { tenant_id: "ten ant" });
    expect(teams.pathname).toBe("/ten%20ant/oauth2/v2.0/authorize");
    const linear = await url("linear");
    expect(linear.searchParams.get("scope")).toBe("read");
    const jira = await url("jira");
    expect(jira.searchParams.get("audience")).toBe("api.atlassian.com");
    expect(
      (await url("github", { oauth_scopes: "a, b", oauth_authorize_url: "https://ghe/auth?x=1" })).searchParams.get(
        "scope",
      ),
    ).toBe("a b");
  });

  it("needs a client ID, and the endpoints of an unknown provider", async () => {
    expect(await proverFromIntegration(secrets, integration({ id: "i", provider: "slack" }))).toBeNull();
    await expect(proverFromIntegration(secrets, rec("other"))).rejects.toThrow(/oauth_authorize_url/);
  });

  it("exchanges Jira with a JSON body and Linear with a GraphQL user-info request", async () => {
    const ff = new FakeFetch();
    ff.on("auth.atlassian.com/oauth/token", () => ({ access_token: "t" })).on("api.atlassian.com/me", () => ({
      account_id: "acc-1",
      name: "J",
    }));
    const j = await (await proverFromIntegration(secrets, rec("jira"), ff.fetch))!.exchange("code", "https://of/cb");
    expect(j).toEqual({ externalId: "acc-1", handle: "J" });
    expect(ff.calls[0]!.headers.get("Content-Type")).toBe("application/json");
    expect(ff.json(0)["client_secret"]).toBe("client-secret");
    const ff2 = new FakeFetch();
    ff2
      .on("oauth/token", () => ({ access_token: "t" }))
      .on("graphql", () => ({ data: { viewer: { id: "u-1", name: "L" } } }));
    const l = await (await proverFromIntegration(secrets, rec("linear"), ff2.fetch))!.exchange("code", "https://of/cb");
    expect(l).toEqual({ externalId: "u-1", handle: "L" });
    expect(ff2.calls[1]!.method).toBe("POST");
    expect(ff2.calls[1]!.headers.get("Authorization")).toBe("Bearer t");
  });

  it("fieldString reads names, dotted paths and numbers", () => {
    expect(fieldString({ a: { b: 5 } }, "a.b")).toBe("5");
    expect(fieldString({ "x.y": "v", x: { y: "w" } }, "x.y")).toBe("v");
    expect(fieldString({ a: true }, "a")).toBe("");
  });

  it("selectSignIn picks by level and refuses ambiguity", () => {
    const a = integration({ id: "a", provider: "slack", config: { oauth_client_id: "x" } });
    const b = integration({ id: "b", provider: "slack", config: { oauth_client_id: "x" }, factory_id: "f1" });
    expect(selectSignIn([a, b], "slack", "")!.id).toBe("a");
    expect(selectSignIn([a, b], "slack", "f1")!.id).toBe("b");
    expect(selectSignIn([b], "slack", "")!.id).toBe("b");
    expect(() => selectSignIn([a, { ...a, id: "c" }], "slack", "")).toThrow(/keep it on one record/);
    expect(selectSignIn([integration({ id: "d", provider: "slack" })], "slack", "")).toBeNull();
  });
});
