// Ports of internal/forge tests: forgeutil (mentions, HMAC, JWT, selection),
// github (client with a fetch fake, scope, events, webhook intake) and the
// forge responder.
import { beforeEach, describe, expect, it } from "vitest";
import { OfError, type Event, type Integration } from "@openfactory/core";
import { b64, utf8 } from "../lib/bytes";
import { extractMentions, hasMention, formatOutbound, splitRepo, truncate } from "./util/text";
import { hmacSha256Hex, verifyHmacSha256Hex } from "./util/webhook";
import { JWT, decodePEM, parseRSAPrivateKey, pkcs1ToPkcs8, rsaPublicKey, signRS256, signHS256 } from "./util/jwt";
import { DeliveryAmbiguousError, DeliveryUnverifiedError, intakeVerified, isNoIntegration, repoOwner, selectIntegration } from "./util/select";
import { GitHubClient, resetGitHubCaches, deriveWebURL } from "./github/client";
import { Normalizer, aliasFromLabels, checkName, isDefinitionPath, normalizeRepo } from "./github/events";
import { handleGitHubWebhook, type GitHubWebhookDeps } from "./github/webhook";
import { ForgeResponder, Resolver, forgeFromProvider, integrationProvider } from "./registry";
import { parseGitRepo, renderCheck, sync, truncateDesc } from "./defsync";
import { FakeForge, FakeResolver } from "../../test/fakes/forge";

describe("forgeutil text", () => {
  const cases: [string, string[]][] = [
    ["hey @alice can you look", ["alice"]],
    ["cc @acme/backend and @openfactory[bot].", ["acme/backend", "openfactory[bot]"]],
    ["```\n@ignored\n```\nand `@inline` but @real", ["real"]],
    ["> @quoted said\n@me", ["me"]],
    ["mail bob@example.com", []],
    ["@a @A @b", ["a", "b"]],
  ];
  for (const [text, want] of cases) it(`mentions ${JSON.stringify(text)}`, () => expect(extractMentions(text)).toEqual(want));
  it("helpers", () => {
    expect(hasMention("ping @OpenFactory[bot]", "openfactory")).toBe(true);
    expect(splitRepo("group/sub/proj")).toEqual(["group/sub", "proj"]);
    expect(truncate("héllo", 3)).toBe("hé…");
    expect(formatOutbound({ kind: "question", text: " Pick? ", options: ["a", "b"] })).toBe("**Question from the factory**\n\nPick?\n\nReply with one of:\n- `a`\n- `b`");
  });
  it("HMAC", async () => {
    const sig = await hmacSha256Hex(utf8("s3cret"), "body");
    expect(await verifyHmacSha256Hex(utf8("s3cret"), utf8("body"), "sha256=" + sig)).toBe(true);
    expect(await verifyHmacSha256Hex(utf8("other"), utf8("body"), sig)).toBe(false);
    expect(await verifyHmacSha256Hex(new Uint8Array(), utf8("body"), sig)).toBe(false);
  });
});

/** Generates an RSA key and returns its PKCS#1 PEM (the GitHub App key format). */
async function rsaPKCS1(): Promise<{ pem: string; pub: CryptoKey; jwk: JsonWebKey }> {
  const kp = (await crypto.subtle.generateKey(
    { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
    true,
    ["sign", "verify"],
  )) as CryptoKeyPair;
  const p8 = new Uint8Array((await crypto.subtle.exportKey("pkcs8", kp.privateKey)) as ArrayBuffer);
  // PrivateKeyInfo: SEQUENCE { INTEGER 0, AlgorithmIdentifier, OCTET STRING pkcs1 }
  let i = 1;
  const len = (): number => {
    const b = p8[i++]!;
    if (b < 0x80) return b;
    let n = 0;
    for (let k = 0; k < (b & 0x7f); k++) n = (n << 8) | p8[i++]!;
    return n;
  };
  len();
  i += 3; // INTEGER 0
  i += 1;
  const algLen = len();
  i += algLen; // AlgorithmIdentifier
  i += 1; // OCTET STRING tag
  const n = len();
  const pkcs1 = p8.subarray(i, i + n);
  const lines = b64(pkcs1).match(/.{1,64}/g)!.join("\n");
  const jwk = (await crypto.subtle.exportKey("jwk", kp.publicKey)) as JsonWebKey;
  return { pem: `-----BEGIN RSA PRIVATE KEY-----\n${lines}\n-----END RSA PRIVATE KEY-----\n`, pub: kp.publicKey, jwk };
}

describe("JWT", () => {
  it("RS256 with a PKCS#1 key round-trips", async () => {
    const { pem, jwk } = await rsaPKCS1();
    expect(decodePEM(pem)?.type).toBe("RSA PRIVATE KEY");
    const key = await parseRSAPrivateKey(pem);
    const exp = Math.floor(Date.now() / 1000) + 60;
    const tok = await signRS256(key, { iss: "x", aud: ["a", "b"], exp }, { kid: "k1" });
    const j = JWT.parse(tok);
    await j.verifyRS256(await rsaPublicKey({ kty: "RSA", kid: "k1", n: jwk.n!, e: jwk.e! }));
    expect(j.hasAudience("b")).toBe(true);
    expect(j.hasAudience("c")).toBe(false);
    expect(j.headerString("kid")).toBe("k1");
    expect(() => j.verifyTime(Date.now() + 120_000, 0)).toThrow(/expired/);
  });
  it("HS256", async () => {
    const tok = await signHS256(utf8("k"), { exp: 1 });
    await JWT.parse(tok).verifyHS256(utf8("k"));
    await expect(JWT.parse(tok).verifyHS256(utf8("x"))).rejects.toThrow(/invalid signature/);
    expect(() => JWT.parse("a.b")).toThrow(/malformed/);
  });
  it("rejects garbage keys", async () => {
    await expect(parseRSAPrivateKey("nope")).rejects.toThrow(/no PEM block/);
  });
});

const rec = (id: string, extra: Partial<Integration> = {}): Integration => ({
  id,
  team_id: "team_1",
  provider: "github",
  name: id,
  config: { org: "acme" },
  created_at: "",
  ...extra,
});

describe("selection rule", () => {
  it("owners per provider", () => {
    expect(repoOwner("github", "acme/api")).toBe("acme");
    expect(repoOwner("gitlab", "acme/platform/api")).toBe("acme");
    expect(repoOwner("azure_devops", "proj/repo")).toBe("");
    expect(repoOwner("azure_devops", "org/proj/repo")).toBe("org");
    expect(repoOwner("git", "a/b")).toBe("");
  });
  it("factory record first, then team record, never another factory", () => {
    const ints = [rec("team"), rec("fac", { factory_id: "f1" }), rec("other", { factory_id: "f2" })];
    expect(selectIntegration(ints, "github", "f1", "acme").id).toBe("fac");
    expect(selectIntegration(ints, "github", "f3", "ACME").id).toBe("team");
    expect(selectIntegration(ints, "github", "", "acme").id).toBe("team");
    try {
      selectIntegration(ints, "github", "", "globex");
      throw new Error("want error");
    } catch (e) {
      expect(isNoIntegration(e)).toBe(true);
      expect((e as Error).message).toMatch(/owner "globex"/);
    }
    expect(() => selectIntegration([rec("a"), rec("b")], "github", "", "acme")).toThrow(/2 records \(a, b\) serve the owner/);
  });
  it("intake verified across teams", async () => {
    const t1 = rec("t1");
    const t2 = rec("t2", { team_id: "team_2" });
    expect((await intakeVerified([t1, t2], "github", ["acme"], async (i) => i.id === "t2")).id).toBe("t2");
    await expect(intakeVerified([t1, t2], "github", ["acme"], async () => true)).rejects.toBeInstanceOf(DeliveryAmbiguousError);
    await expect(intakeVerified([t1, t2], "github", ["acme"], async () => false)).rejects.toBeInstanceOf(DeliveryUnverifiedError);
    const e = (await intakeVerified([t1], "github", ["globex"], async () => true).catch((x: unknown) => x)) as DeliveryUnverifiedError;
    expect(e.noIntegration).toBe(true);
  });
  it("forge names", () => {
    expect(integrationProvider("azure-devops")).toBe("azure_devops");
    expect(integrationProvider("")).toBe("github");
    expect(forgeFromProvider("azuredevops")).toBe("AZURE_DEVOPS");
  });
});

// ---------- GitHub fake ----------

interface FakeGH {
  fetch: (input: Request | string | URL, init?: RequestInit) => Promise<Response>;
  bodies: Map<string, unknown>;
  auth: Map<string, string>;
  calls: string[];
}

function fakeGitHub(): FakeGH {
  const bodies = new Map<string, unknown>();
  const auth = new Map<string, string>();
  const calls: string[] = [];
  const ok = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { "content-type": "application/json" } });
  const fetchFn = async (input: Request | string | URL, init?: RequestInit): Promise<Response> => {
    const u = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    const method = init?.method ?? "GET";
    const key = `${method} ${u.pathname}`;
    calls.push(key + u.search);
    auth.set(key, new Headers(init?.headers).get("authorization") ?? "");
    if (init?.body) bodies.set(key, JSON.parse(String(init.body)));
    switch (true) {
      case key === "POST /app/installations/42/access_tokens":
        return ok({ token: "ghs_inst", expires_at: new Date(Date.now() + 3600_000).toISOString() }, 201);
      case key.startsWith("POST /app/installations/"):
        return ok({ message: "Not Found" }, 404);
      case key === "GET /repos/acme/api":
        return ok({ default_branch: "trunk" });
      case key === "POST /repos/acme/api/issues/7/comments":
        return ok({ id: 1 }, 201);
      case key === "POST /repos/acme/api/check-runs":
        return ok({ id: 99 }, 201);
      case key === "PATCH /repos/acme/api/check-runs/99":
        return ok({ id: 99 });
      case key === "POST /repos/acme/api/statuses/abc":
        return ok({}, 201);
      case key === "POST /repos/acme/api/pulls":
        return ok({ number: 5, html_url: "https://github.com/acme/api/pull/5", state: "open", draft: true, head: { ref: "f" } }, 201);
      case key === "GET /repos/acme/api/commits/trunk":
        return ok({ sha: "c0ffee", commit: { tree: { sha: "tree1" } } });
      case key === "GET /repos/acme/api/git/trees/tree1":
        return ok({
          tree: [
            { path: "defs/factory.yaml", type: "blob", sha: "b1", size: 5 },
            { path: "defs/agents/a.md", type: "blob", sha: "b2", size: 2 },
            { path: "README.md", type: "blob", sha: "b3", size: 1 },
            { path: "defs/agents", type: "tree", sha: "t2" },
          ],
        });
      case key === "GET /repos/acme/api/git/blobs/b1":
        return ok({ content: btoa("name:\n"), encoding: "base64" });
      case key === "GET /repos/acme/api/git/blobs/b2":
        return ok({ content: "hi", encoding: "utf-8" });
      case key === "GET /repos/acme/api/pulls/7/files":
        return ok([{ filename: "a.go", patch: "@@ -1 +1 @@\n-a\n+b" }, { filename: "new.go", previous_filename: "old.go" }]);
      case key === "GET /repos/acme/api/pulls/7/comments":
        return ok([
          { id: 10, body: "root", path: "a.go", line: 3, created_at: "2026-01-01T00:00:00Z", user: { login: "alex" } },
          { id: 11, in_reply_to_id: 10, body: "reply", path: "a.go", line: null, original_line: 3, user: { login: "sam" } },
          { id: 12, body: "other", path: "b.go", line: 1, user: { login: "x" } },
        ]);
      case key === "GET /user":
        return ok({ login: " octocat " });
    }
    return ok({ message: "unexpected " + key }, 500);
  };
  return { fetch: fetchFn, bodies, auth, calls };
}

describe("GitHubClient", () => {
  let pem = "";
  beforeEach(async () => {
    resetGitHubCaches();
    if (!pem) pem = (await rsaPKCS1()).pem;
  });

  it("derives the web URL", () => {
    expect(deriveWebURL("https://api.github.com")).toBe("https://github.com");
    expect(deriveWebURL("https://ghe.example.com/api/v3")).toBe("https://ghe.example.com");
  });

  it("app installation tokens are scoped to the repositories of the owner", async () => {
    const f = fakeGitHub();
    const c = await GitHubClient.create({ baseURL: "https://gh.test", appId: "123", privateKey: pem, installationId: 42, fetch: f.fetch });
    const s = c.scoped(["acme/api", "acme/web", "other/x"]);
    expect(c.scoped(["acme/web", "ACME/api", "other/x"])).toBe(s);
    expect(await s.cloneURL("acme", "api")).toBe("https://x-access-token:ghs_inst@gh.test/acme/api.git");
    expect((f.bodies.get("POST /app/installations/42/access_tokens") as { repositories: string[] }).repositories).toEqual(["api", "web"]);
    expect(f.auth.get("POST /app/installations/42/access_tokens")).toMatch(/^Bearer ey/);
    await expect(s.cloneURL("acme", "billing")).rejects.toMatchObject({ code: "not_authorized" });
    await expect(s.token("nobody", "")).rejects.toMatchObject({ code: "not_authorized" });
    const p = new GitHubClient({ baseURL: "https://gh.test", token: "ghp_pat", fetch: f.fetch });
    await expect(p.scoped(["acme/api"]).cloneURL("acme", "billing")).rejects.toMatchObject({ code: "not_authorized" });
    expect(c.scoped([])).toBe(c);
    // The token is cached: no second token request.
    await s.token("acme", "api");
    expect(f.calls.filter((x) => x.startsWith("POST /app/installations")).length).toBe(1);
  });

  it("auth failure callback fires for a missing installation", async () => {
    const f = fakeGitHub();
    const got: unknown[] = [];
    const c = await GitHubClient.create({ baseURL: "https://gh.test", appId: "123", privateKey: pem, installationId: 7, fetch: f.fetch, onAuthFailure: (e) => got.push(e) });
    await expect(c.token("acme", "api")).rejects.toThrow(/create installation token/);
    expect(got.length).toBe(1);
  });

  it("REST operations with a personal token", async () => {
    const f = fakeGitHub();
    const c = new GitHubClient({ baseURL: "https://gh.test", token: "ghp_pat", fetch: f.fetch });
    expect(await c.defaultBranch("acme", "api")).toBe("trunk");
    expect(f.auth.get("GET /repos/acme/api")).toBe("token ghp_pat");
    await c.comment("acme", "api", 7, "hello");
    expect(f.bodies.get("POST /repos/acme/api/issues/7/comments")).toEqual({ body: "hello" });
    await c.setCommitStatus("acme", "api", "abc", { context: "ctx", state: "", description: "x".repeat(200) });
    const st = f.bodies.get("POST /repos/acme/api/statuses/abc") as Record<string, string>;
    expect(st.state).toBe("pending");
    expect(Array.from(st.description!).length).toBe(140);
    const pr = await c.createPullRequest({ owner: "acme", repo: "api", head: "f", base: "", title: "t", body: "b", draft: true });
    expect(pr).toMatchObject({ forge: "GITHUB", number: 5, state: "draft", branch: "f" });
    expect((f.bodies.get("POST /repos/acme/api/pulls") as { base: string }).base).toBe("trunk");
    const { files, sha } = await c.getFiles("acme", "api", "", "defs/");
    expect(sha).toBe("c0ffee");
    expect(files).toEqual({ "factory.yaml": "name:\n", "agents/a.md": "hi" });
    expect(await c.listPullRequestFiles("acme", "api", 7)).toEqual(["a.go", "new.go", "old.go"]);
    expect(await c.fileDiff("acme", "api", 7, "/a.go")).toEqual({ path: "a.go", patch: "@@ -1 +1 @@\n-a\n+b" });
    await expect(c.fileDiff("acme", "api", 7, "new.go")).rejects.toMatchObject({ code: "resource_not_found" });
    const th = await c.reviewThread("acme", "api", 7, "10");
    expect(th.path).toBe("a.go");
    expect(th.comments!.map((x) => [x.id, x.author, x.line])).toEqual([
      ["10", "alex", 3],
      ["11", "sam", 3],
    ]);
    await expect(c.replyInThread("acme", "api", 7, "x", "b")).rejects.toMatchObject({ code: "invalid_request" });
    expect(await c.login()).toBe("octocat");
  });

  it("an App publishes check runs and updates the run in progress", async () => {
    const f = fakeGitHub();
    const c = await GitHubClient.create({ baseURL: "https://gh.test", appId: "123", privateKey: pem, installationId: 42, fetch: f.fetch });
    await c.publishCheckRun("acme", "api", { name: "openfactory/definition", headSha: "abc", title: "Validating" });
    await c.publishCheckRun("acme", "api", { name: "openfactory/definition", headSha: "abc", status: "completed", conclusion: "success", title: "ok", summary: "s" });
    expect(f.calls).toContain("PATCH /repos/acme/api/check-runs/99");
    const body = f.bodies.get("PATCH /repos/acme/api/check-runs/99") as Record<string, unknown>;
    expect(body).toMatchObject({ status: "completed", conclusion: "success", output: { title: "ok", summary: "s" } });
    expect(body["name"]).toBeUndefined();
    await c.setCommitStatus("acme", "api", "abc", { context: "c", state: "error" });
    expect((f.bodies.get("POST /repos/acme/api/check-runs") as Record<string, unknown>)["conclusion"]).toBe("action_required");
    await expect(c.login()).rejects.toThrow(/no user login/);
  });
});

// ---------- events ----------

describe("GitHub normalizer", () => {
  const norm = new Normalizer({ appSlug: "openfactory", now: () => Date.parse("2026-01-01T00:00:00Z") });
  it("issue opened with labels and a mention", () => {
    const body = JSON.stringify({
      action: "opened",
      repository: { full_name: "acme/api" },
      sender: { id: 5, login: "alex", type: "User" },
      issue: { number: 3, title: "Bug", body: "please @openfactory fix", html_url: "u", user: { login: "alex" }, labels: [{ name: "factory:ops" }], assignees: [] },
    });
    const evs = norm.normalize("issues", "d1", body);
    expect(evs.map((e) => e.type)).toEqual(["issue_created", "issue_labeled", "issue_mentioned"]);
    const e = evs[0]!;
    expect(e.delivery_id).toBe("d1:issue_created");
    expect(e.action_key).toBe("github:d1");
    expect(e.attributes).toMatchObject({ repo: "acme/api", alias: "ops", added_labels: ["factory:ops"], actor_is_bot: false, actor_is_self: false });
    expect(e.text).toBe("Bug\n\nplease @openfactory fix");
    expect(evs[2]!.attributes!["mentioned"]).toEqual(["openfactory"]);
  });
  it("review comment carries the thread and self detection", () => {
    const body = JSON.stringify({
      action: "created",
      repository: { full_name: "acme/api" },
      sender: { id: 9, login: "openfactory[bot]", type: "Bot" },
      pull_request: { number: 8, title: "T", html_url: "u", head: { ref: "f", sha: "s" }, base: { ref: "main" }, user: { login: "x" }, additions: 3 },
      comment: { id: 22, in_reply_to_id: 20, body: "ok", user: { login: "openfactory[bot]" } },
    });
    const [e] = norm.normalize("pull_request_review_comment", "d2", body);
    expect(e!.source.thread_ref).toBe("20");
    expect(e!.actor).toMatchObject({ is_bot: true, is_self: true });
    expect(e!.attributes).toMatchObject({ pr_number: 8, base_branch: "main", additions: 3, thread_ref: "20" });
  });
  it("push, status, CI and ignored deliveries", () => {
    const push = norm.normalize("push", "", JSON.stringify({ ref: "refs/heads/main", after: "abc", repository: { full_name: "acme/api" }, commits: [{ added: ["a"], modified: ["a", "b"] }] }));
    expect(push[0]!.attributes).toMatchObject({ branch: "main", paths: ["a", "b"], head_sha: "abc" });
    expect(push[0]!.action_key).toBeUndefined();
    expect(norm.normalize("push", "", JSON.stringify({ ref: "refs/heads/main", after: "0000", repository: {} }))).toEqual([]);
    expect(norm.normalize("status", "", JSON.stringify({ state: "pending", repository: {} }))).toEqual([]);
    const ws = norm.normalize("workflow_run", "x", JSON.stringify({ action: "completed", repository: { full_name: "a/b" }, workflow_run: { id: 1, name: "CI", conclusion: "failure", pull_requests: [{ number: 4, base: { ref: "main" } }] } }));
    expect(ws[0]!.attributes).toMatchObject({ linked_pr_count: 1, pr_number: 4, conclusion: "failure", workflow: "CI" });
    expect(norm.normalize("check_run", "x", JSON.stringify({ action: "rerequested", check_run: { id: 1, app: { slug: "other" } } }))).toEqual([]);
  });
  it("helpers", () => {
    expect(aliasFromLabels(["bug", "Factory:Ops "])).toBe("Ops");
    expect(normalizeRepo("https://github.com/Acme/X.git")).toBe("acme/x");
    expect(normalizeRepo("git@github.com:acme/x")).toBe("acme/x");
    expect(checkName("defs/")).toBe("openfactory/definition (defs)");
    expect(isDefinitionPath("defs", "defs/agents/a.md")).toBe(true);
    expect(isDefinitionPath("", "src/x")).toBe(false);
  });
});

// ---------- webhook intake ----------

describe("GitHub webhook receiver", () => {
  const secretFor: Record<string, string> = { WH: "whsec" };
  let published: Event[];
  let statuses: [string, string, string][];
  let records: Integration[];
  const deps = (over: Partial<GitHubWebhookDeps> = {}): GitHubWebhookDeps => ({
    teams: async () => ["team_1"],
    integrations: {
      list: async () => records,
      setStatus: async (id, status, reason) => {
        statuses.push([id, status, reason]);
        return true;
      },
    },
    secrets: { get: async (_t, _f, n) => secretFor[n] },
    publish: async (_t, evs) => {
      published.push(...evs);
    },
    ...over,
  });
  beforeEach(() => {
    published = [];
    statuses = [];
    records = [rec("int_gh", { config: { org: "acme", webhook_secret_ref: "WH", installation_id: "42", app_id: "123", app_slug: "openfactory" } })];
  });
  const deliver = async (event: string, secret: string, body: unknown, d = deps()) => {
    const text = JSON.stringify(body);
    const sig = "sha256=" + (await hmacSha256Hex(utf8(secret), text));
    return handleGitHubWebhook(new Request("https://cp.test/integrations/github/webhook", { method: "POST", body: text, headers: { "x-github-event": event, "x-github-delivery": "d1", "x-hub-signature-256": sig } }), d);
  };
  it("verifies, normalizes, routes by alias and publishes", async () => {
    const fr = new FakeResolver();
    const res = await deliver(
      "issues",
      "whsec",
      { action: "labeled", repository: { full_name: "acme/api" }, sender: { id: 1, login: "a" }, issue: { number: 1, labels: [{ name: "factory:ops" }] }, label: { name: "factory:ops" } },
      deps({ resolver: fr, factories: { byAlias: async (_t, a) => (a === "ops" ? { id: "fac_ops", alias: "ops" } : null), list: async () => [] } }),
    );
    expect(res.status).toBe(202);
    expect(await res.json()).toEqual({ status: "accepted", events: 1 });
    expect(published.length).toBe(1);
    expect(published[0]).toMatchObject({ team_id: "team_1", factory_id: "fac_ops", type: "issue_labeled" });
  });
  it("rejects bad signatures and answers ping", async () => {
    expect((await deliver("issues", "bad", { repository: { full_name: "acme/api" } })).status).toBe(401);
    expect((await deliver("ping", "whsec", { hook: { app_id: 123 } })).status).toBe(200);
    expect((await deliver("ping", "bad", { hook: { app_id: 123 } })).status).toBe(401);
    const r = await handleGitHubWebhook(new Request("https://cp.test/x", { method: "POST", body: "{}" }), deps());
    expect(r.status).toBe(400);
  });
  it("installation events set the status of the team's records", async () => {
    records.push(rec("int_fac", { factory_id: "fac_1", config: { org: "acme", installation_id: "42" } }));
    const res = await deliver("installation", "whsec", { action: "deleted", installation: { id: 42 } });
    expect(await res.json()).toEqual({ status: "accepted", integrations: 2 });
    expect(statuses).toEqual([
      ["int_gh", "disabled", "the app was removed"],
      ["int_fac", "disabled", "the app was removed"],
    ]);
    expect(published).toEqual([]);
  });
  it("unchanged definitions pass the definition check at once", async () => {
    const fr = new FakeResolver();
    fr.forgeFor("GITHUB").prFiles = ["src/a.go"];
    const prs: unknown[] = [];
    await deliver(
      "pull_request",
      "whsec",
      { action: "opened", repository: { full_name: "acme/defs" }, sender: { id: 1, login: "a" }, pull_request: { number: 2, head: { sha: "h", ref: "f" }, base: { ref: "main" } } },
      deps({
        resolver: fr,
        factories: { byAlias: async () => null, list: async () => [{ id: "f1", source: "git", git_repo: "github.com/acme/defs" }] },
        onDefinitionPR: async (pr) => {
          prs.push(pr);
        },
      }),
    );
    const st = fr.forgeFor("GITHUB").callsOf("setCommitStatus");
    expect(st.length).toBe(1);
    expect(st[0]!.args[3]).toMatchObject({ context: "openfactory/definition", state: "success" });
    expect(prs).toEqual([]);
  });
});

// ---------- responder, resolver, defsync ----------

describe("ForgeResponder", () => {
  const msg = (kind: string, source: Record<string, unknown>, extra: Record<string, unknown> = {}) =>
    ({ work_item_id: "wi", text: "hi", kind, source: { provider: "github", ...source }, ...extra }) as never;
  it("routes replies to threads, issues and pull requests", async () => {
    const fr = new FakeResolver();
    const r = new ForgeResponder(fr, "github");
    const f = fr.forgeFor("GITHUB");
    await r.reply("t", msg("update", { kind: "pull_request", repo: "acme/api", number: 3, thread_ref: "9" }));
    expect(f.callsOf("replyInThread").length).toBe(1);
    f.unsupported.add("replyInThread");
    await r.reply("t", msg("update", { kind: "pull_request", repo: "acme/api", number: 3 }, { thread_ref: "9" }));
    expect(f.callsOf("comment").length).toBe(1);
    await r.reply("t", msg("update", { kind: "issue", repo: "acme/api", number: 4 }));
    expect(f.callsOf("commentIssue").length).toBe(1);
    await r.reply("t", msg("update", { kind: "push", repo: "acme/api" }));
    expect(fr.lookups.length).toBe(3);
    await expect(r.setIssueState("t", { provider: "github", kind: "issue", repo: "a/b", number: 1 }, "done")).rejects.toMatchObject({ code: "operation_not_supported" });
  });
});

describe("Resolver", () => {
  it("selects, refuses disabled records and the generic git forge", async () => {
    const ints = [rec("i1", { secret_ref: "PAT" }), rec("i2", { config: { org: "off" }, status: "disabled", status_reason: "removed" }), rec("g", { provider: "git", config: {} })];
    const r = new Resolver({ integrations: { list: async (_t, p) => ints.filter((x) => !p || x.provider === p) }, secrets: { get: async () => "ghp_x" } });
    const f = await r.forge({ teamId: "team_1", codeForge: "GITHUB", repo: "acme/api", scope: ["acme/api", "other/x"] });
    expect(f.name()).toBe("GITHUB");
    await expect(r.forge({ teamId: "team_1", codeForge: "GITHUB", repo: "off/x" })).rejects.toMatchObject({ code: "integration_disabled" });
    await expect(r.forge({ teamId: "team_1", codeForge: "GIT", repo: "x/y" })).rejects.toMatchObject({ code: "operation_not_supported" });
    await expect(r.forge({ teamId: "team_1", codeForge: "GITLAB", repo: "x/y" })).rejects.toMatchObject({ code: "invalid_request" });
    await expect(r.forgeForUser({ teamId: "team_1", codeForge: "GITHUB", repo: "acme/api" }, "u1")).resolves.toMatchObject({ userToken: false });
    const ru = new Resolver({ integrations: { list: async () => ints }, secrets: { get: async () => "x" }, userTokens: { get: async () => undefined } });
    await expect(ru.forgeForUser({ teamId: "team_1", codeForge: "GITHUB", repo: "acme/api" }, "u1")).rejects.toMatchObject({ code: "external_authentication_required" });
  });
  it("plugin forges need their plugin Worker", async () => {
    const r = new Resolver({ integrations: { list: async () => [rec("gl", { provider: "gitlab" })] }, secrets: { get: async () => "x" } });
    const e = (await r.forge({ teamId: "team_1", codeForge: "GITLAB", repo: "acme/x" }).catch((x: unknown) => x)) as OfError;
    expect(e.code).toBe("feature_not_available");
    expect(e.message).not.toMatch(/openfactory-plugin/);
  });
});

describe("defsync", () => {
  it("parses git repositories", () => {
    expect(parseGitRepo("acme/defs")).toEqual({ codeForge: "GITHUB", owner: "acme", repo: "defs" });
    expect(parseGitRepo("https://gitlab.example.com/g/sub/defs.git")).toEqual({ codeForge: "GITLAB", owner: "g/sub", repo: "defs" });
    expect(parseGitRepo("https://dev.azure.com/org/proj/_git/repo")).toEqual({ codeForge: "AZURE_DEVOPS", owner: "org/proj", repo: "repo" });
    expect(parseGitRepo("git.example.org/a/b").codeForge).toBe("GIT");
    expect(() => parseGitRepo("x")).toThrow(/owner\/repository/);
  });
  it("syncs a new commit once and records invalid commits", async () => {
    const fr = new FakeResolver();
    fr.forgeFor("GITHUB").files = { "factory.yaml": "name: x\n" };
    let last = "";
    const applied: string[] = [];
    const deps = {
      resolver: fr,
      publicURL: "https://cp.test",
      lastSha: { get: async () => last, set: async (_f: unknown, s: string) => void (last = s) },
      apply: async (_f: unknown, _files: unknown, m: string) => {
        applied.push(m);
        return { versionId: "v1" };
      },
      check: async () => ({ valid: true, errors: [], warnings: [], skipped: [] }),
    };
    const f = { id: "f1", team_id: "t", source: "git", git_repo: "acme/defs" };
    await sync(deps, f);
    await sync(deps, f);
    expect(applied).toEqual(["Sync main @ 0123456"]);
  });
  it("renders checks", () => {
    expect(renderCheck({ valid: false, errors: [{ file: "a.yaml", line: 2, field: "x", message: "bad" }], warnings: [], skipped: [] }).title).toBe("1 error(s): a.yaml:2 x: bad");
    expect(renderCheck({ valid: true, errors: [], warnings: [], skipped: [], diff: { empty: false, added: 1, removed: 0, changed: 2, summary: "+ agent a" } }).title).toBe(
      "Definition is valid: 1 added, 0 removed, 2 changed",
    );
    expect(truncateDesc("a  b " + "x".repeat(200)).endsWith("...")).toBe(true);
  });
});

void FakeForge;
