import { beforeAll, describe, expect, it } from "vitest";
import type { Event } from "@openfactory/core";
import { b64urlEncode, signRS256 } from "../jwt";
import { FakeFetch, NOW, integration, kit, post, user } from "../testkit";
import { KeySet } from "./auth";
import { cleanText, fitThread, threadEntry, type ThreadEntry } from "./context";
import { TeamsHandler, threadRoot, tokenTenants, DEFAULT_SERVICE_URL } from "./handler";
import { TeamsNameResolver } from "./resolver";

const APP = "app-guid";
const SERVICE = "https://smba.test/teams/";
const nowS = Math.floor(NOW.getTime() / 1000);
let pair: CryptoKeyPair;
let jwk: Record<string, unknown>;

beforeAll(async () => {
  pair = (await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"])) as CryptoKeyPair;
  jwk = { ...(await crypto.subtle.exportKey("jwk", pair.publicKey)), kid: "k1" };
});

const token = (over: Record<string, unknown> = {}, header: Record<string, unknown> = { kid: "k1" }) =>
  signRS256(pair.privateKey, header, { iss: "https://api.botframework.com", aud: APP, exp: nowS + 600, serviceurl: SERVICE, ...over });

function setup() {
  const k = kit({ BOT_PW: "bot-password" });
  k.store.integrations = [
    integration({ id: "int_t", provider: "microsoft_teams", secret_ref: "BOT_PW", config: { team_id: "G1", tenant_id: "ten-1", app_id: APP } }),
  ];
  k.store.users = [user({ id: "usr_1" })];
  k.store.identities.set("teams|aad-1", { user_id: "usr_1", provider: "teams", external_id: "aad-1", handle: "" });
  k.ff
    .on("openidconfiguration", () => ({ issuer: "https://api.botframework.com", jwks_uri: "https://login.test/keys" }))
    .on("login.test/keys", () => ({ keys: [{ ...jwk, endorsements: ["msteams"] }] }))
    .on("/oauth2/v2.0/token", () => ({ access_token: "bot-token", expires_in: 3600 }))
    .on("/activities", () => ({ id: "x" }));
  return { k, h: new TeamsHandler(k.deps, { openIdMetadataUrl: "https://login.test/openidconfiguration" }) };
}

const activity = (over: Record<string, unknown> = {}) => ({
  type: "message",
  id: "act-1",
  serviceUrl: SERVICE,
  channelId: "msteams",
  from: { id: "29:u", name: "Al", aadObjectId: "aad-1" },
  recipient: { id: "28:" + APP, name: "Factory" },
  conversation: { id: "19:chan@thread.tacv2", conversationType: "channel", tenantId: "ten-1" },
  text: "<at>Factory</at> fix the bug",
  entities: [{ type: "mention", mentioned: { id: "28:" + APP } }],
  channelData: { team: { aadGroupId: "G1" }, channel: { id: "19:chan@thread.tacv2", name: "dev" }, tenant: { id: "ten-1" } },
  ...over,
});

const send = async (h: TeamsHandler, act: object, tok?: string) => {
  const out = await h.handleMessages(post("https://of.example.com/integrations/teams/messages", JSON.stringify(act), { Authorization: "Bearer " + (tok ?? (await token())) }));
  await out.background?.();
  return out;
};

const sentTexts = (ff: FakeFetch) => ff.calls.filter((c) => c.url.endsWith("/activities") || c.url.includes("/activities")).map((c) => (JSON.parse(c.body) as { text: string }).text);

describe("bot connector token validation", () => {
  const keys = () => {
    const { k } = setup();
    return new KeySet({ metadataUrl: "https://login.test/openidconfiguration", fetch: k.ff.fetch, now: () => NOW.getTime() });
  };

  it("accepts a valid token", async () => {
    await keys().validate("Bearer " + (await token()), [APP], "msteams", SERVICE.replace(/\/$/, ""));
  });

  it("rejects each wrong claim", async () => {
    const ks = keys();
    const bad = async (tok: string, re: RegExp) => expect(ks.validate("Bearer " + tok, [APP], "msteams", SERVICE)).rejects.toThrow(re);
    await bad(await token({ iss: "https://evil" }), /unexpected issuer/);
    await bad(await token({ aud: "other" }), /audience/);
    await bad(await token({ exp: nowS - 3600 }), /expired/);
    await bad(await token({ serviceurl: "https://evil.test/" }), /serviceUrl/);
    await bad(await token({}, { kid: "nope" }), /unknown signing key/);
    await bad((await token()).slice(0, -4) + "AAAA", /invalid signature/);
    await expect(ks.validate("Bearer " + (await token()), [APP], "slack", SERVICE)).rejects.toThrow(/not endorsed/);
    await expect(ks.validate("", [APP], "msteams", SERVICE)).rejects.toThrow(/missing bearer/);
    await expect(ks.validate("Bearer " + (await token()), [], "msteams", SERVICE)).rejects.toThrow(/audience/);
  });

  it("rejects an HS256 token signed with a public value", async () => {
    const hs = `${b64urlEncode(JSON.stringify({ alg: "HS256", kid: "k1" }))}.${b64urlEncode(JSON.stringify({ iss: "https://api.botframework.com", aud: APP, exp: nowS + 60 }))}.${b64urlEncode("sig")}`;
    await expect(keys().validate("Bearer " + hs, [APP], "msteams", SERVICE)).rejects.toThrow(/unexpected alg/);
  });
});

describe("teams activities", () => {
  it("rejects a request without a valid token", async () => {
    const { h } = setup();
    const out = await h.handleMessages(post("https://x/m", JSON.stringify(activity()), { Authorization: "Bearer abc.def.ghi" }));
    expect(out.response.status).toBe(401);
  });

  it("publishes app_mention and message_posted for a linked member and records the service URL", async () => {
    const { k, h } = setup();
    await send(h, activity());
    expect(k.published.map((e: Event) => e.type)).toEqual(["app_mention", "message_posted"]);
    expect(k.published[0]).toMatchObject({
      provider: "teams",
      delivery_id: "act-1:app_mention",
      actor: { external_id: "aad-1", handle: "Al", user_id: "usr_1" },
      source: { kind: "thread", channel: "19:chan@thread.tacv2", thread_id: "act-1", workspace: "G1" },
      text: "fix the bug",
    });
    expect(k.published[0]!.attributes).toMatchObject({ team_id: "G1", tenant_id: "ten-1", channel_name: "dev", user_name: "Al", linked: true, actor_is_self: false });
    expect(k.published[0]!.attributes).not.toHaveProperty("thread_reply");
    expect(k.store.kv.get("teams:service_url:19:chan@thread.tacv2")).toBe(SERVICE);
  });

  it("marks a reply in a thread", async () => {
    const { k, h } = setup();
    await send(h, activity({ id: "act-2", entities: [], conversation: { id: "19:chan@thread.tacv2;messageid=1700", conversationType: "channel", tenantId: "ten-1" } }));
    expect(k.published.map((e) => e.type)).toEqual(["message_posted"]);
    expect(k.published[0]!.source.thread_id).toBe("1700");
    expect(k.published[0]!.attributes).toMatchObject({ thread_reply: true, auto_follow_up: true });
  });

  it("answers sign in, sign out and help in the thread", async () => {
    const { k, h } = setup();
    await send(h, activity({ text: "<at>Factory</at> Sign in." }));
    expect(sentTexts(k.ff)[0]).toContain("link your Microsoft account");
    expect(k.ff.calls.find((c) => c.url.includes("/activities"))!.url).toBe(SERVICE.replace(/\/$/, "") + "/v3/conversations/19%3Achan%40thread.tacv2%3Bmessageid%3Dact-1/activities");
    expect(k.ff.calls.find((c) => c.url.includes("/activities"))!.headers.get("Authorization")).toBe("Bearer bot-token");
    await send(h, activity({ id: "act-3", text: "<at>Factory</at> sign out" }));
    expect(k.store.identities.get("teams|aad-1")?.user_id).toBe("");
    expect(sentTexts(k.ff).at(-1)).toContain("no longer linked");
    await send(h, activity({ id: "act-4", text: "<at>Factory</at> help" }));
    expect(sentTexts(k.ff).at(-1)).toContain("Mention me with a request");
    expect(k.published).toHaveLength(0);
  });

  it("asks an unlinked user to sign in, and a foreign tenant to stop", async () => {
    const { k, h } = setup();
    await send(h, activity({ id: "a1", from: { id: "29:z", name: "Zed", aadObjectId: "aad-9" } }));
    expect(sentTexts(k.ff)[0]).toContain("/integrations");
    expect(sentTexts(k.ff)[0]).not.toContain("/integrations/link/");
    await send(h, activity({ id: "a2", channelData: { team: { aadGroupId: "G1" }, tenant: { id: "other" } }, conversation: { id: "19:c@thread.tacv2", conversationType: "channel", tenantId: "other" } }));
    expect(sentTexts(k.ff).at(-1)).toContain("Microsoft 365 organization");
    expect(k.published).toHaveLength(0);
  });

  it("says when the team is not connected, using the credentials of the receiving bot", async () => {
    const { k, h } = setup();
    await send(h, activity({ id: "a3", channelData: { team: { aadGroupId: "UNKNOWN" } } }));
    expect(sentTexts(k.ff)[0]).toContain("isn’t connected");
    // without a mention nothing is said
    k.ff.calls.length = 0;
    await send(h, activity({ id: "a4", entities: [], channelData: { team: { aadGroupId: "UNKNOWN" } } }));
    expect(sentTexts(k.ff)).toEqual([]);
  });

  it("ignores own bots, private chats and non-message activities, and dedupes", async () => {
    const { k, h } = setup();
    await send(h, activity({ id: "b1", from: { id: "28:" + APP } }));
    await send(h, activity({ id: "b2", conversation: { id: "a:1", conversationType: "personal" } }));
    expect((await send(h, activity({ id: "b3", type: "conversationUpdate" }))).response.status).toBe(200);
    await send(h, activity({ id: "b4" }));
    await send(h, activity({ id: "b4" }));
    expect(k.published.filter((e) => e.delivery_id === "b4:app_mention")).toHaveLength(1);
    expect(k.published).toHaveLength(2);
  });

  it("ignores an activity for a disabled integration", async () => {
    const { k, h } = setup();
    k.store.integrations[0]!.status = "disabled";
    await send(h, activity());
    expect(k.published).toHaveLength(0);
    expect(sentTexts(k.ff)).toEqual([]);
  });
});

describe("teams replies and posting", () => {
  const src = { provider: "teams", kind: "thread", channel: "19:c", thread_id: "77", workspace: "G1" };

  it("replies in the thread with the recorded or default service URL", async () => {
    const { k, h } = setup();
    await h.reply("team_1", { work_item_id: "w", source: src, kind: "question", text: "Which?", options: ["a"] });
    expect(k.ff.calls.find((c) => c.url.includes("/activities"))!.url.startsWith(DEFAULT_SERVICE_URL.replace(/\/$/, ""))).toBe(true);
    k.store.kv.set("teams:service_url:19:c", "https://eu.test/");
    k.ff.calls.length = 0;
    await h.reply("team_1", { work_item_id: "w", source: src, kind: "update", text: "done" });
    const c = k.ff.calls.find((x) => x.url.includes("/activities"))!;
    expect(c.url).toBe("https://eu.test/v3/conversations/19%3Ac%3Bmessageid%3D77/activities");
    expect(JSON.parse(c.body)).toEqual({ type: "message", text: "done", textFormat: "markdown" });
    await expect(h.reply("team_1", { work_item_id: "w", source: { ...src, channel: "" }, kind: "update", text: "x" })).rejects.toThrow(/no channel/);
  });

  it("caches the bot token and requests it from botframework.com with the tenant as fallback", async () => {
    const { k, h } = setup();
    await h.reply("team_1", { work_item_id: "w", source: src, kind: "update", text: "1" });
    await h.reply("team_1", { work_item_id: "w", source: src, kind: "update", text: "2" });
    const tokens = k.ff.calls.filter((c) => c.url.endsWith("/token"));
    expect(tokens).toHaveLength(1);
    expect(tokens[0]!.url).toBe("https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token");
    expect(tokens[0]!.body).toContain("client_secret=bot-password");
  });

  it("falls back to the record tenant when botframework.com refuses", async () => {
    const { k, h } = setup();
    k.ff.on("/botframework.com/oauth2", () => new Response("no", { status: 400 }));
    await h.reply("team_1", { work_item_id: "w", source: src, kind: "update", text: "1" });
    expect(k.ff.calls.filter((c) => c.url.endsWith("/token")).map((c) => c.url.split("/")[3])).toEqual(["botframework.com", "ten-1"]);
  });

  it("chooses the token tenant from app_type", () => {
    const rec = (config: Record<string, string>) => integration({ id: "i", provider: "microsoft_teams", config });
    expect(tokenTenants(rec({ tenant_id: "t", app_type: "SingleTenant" }))).toEqual({ tenant: "t", fallback: "" });
    expect(() => tokenTenants(rec({ app_type: "SingleTenant" }))).toThrow(/requires config tenant_id/);
    expect(tokenTenants(rec({ tenant_id: "t", app_type: "multi-tenant" }))).toEqual({ tenant: "botframework.com", fallback: "" });
    expect(tokenTenants(rec({ tenant_id: "t" }))).toEqual({ tenant: "botframework.com", fallback: "t" });
    expect(() => tokenTenants(rec({ app_type: "x" }))).toThrow(/unknown app_type/);
  });

  it("postMessage honours channel_ids and the recorded service URL", async () => {
    const { k, h } = setup();
    const in_ = k.store.integrations[0]!;
    await expect(h.postMessage(in_, "", "", "x")).rejects.toMatchObject({ code: "invalid_request" });
    await expect(h.postMessage(in_, "19:new", "", "x")).rejects.toMatchObject({ code: "not_authorized" });
    k.store.kv.set("teams:service_url:19:seen", SERVICE);
    await h.postMessage(in_, "19:seen", "5", "hello");
    expect(k.ff.calls.at(-1)!.url).toContain("19%3Aseen%3Bmessageid%3D5");
    in_.config!["channel_ids"] = "19:a, 19:b";
    await expect(h.postMessage(in_, "19:seen", "", "x")).rejects.toMatchObject({ code: "not_authorized" });
    await h.postMessage(in_, "19:a", "", "x");
    expect(k.ff.calls.at(-1)!.url.startsWith(DEFAULT_SERVICE_URL.replace(/\/$/, ""))).toBe(true);
  });
});

describe("teams thread context", () => {
  const ev = (): Event => ({
    id: "e", team_id: "team_1", provider: "teams", type: "app_mention", actor: { provider: "teams", external_id: "a", handle: "", is_bot: false },
    source: { provider: "teams", kind: "thread", channel: "19:c", thread_id: "77", workspace: "G1" }, attributes: { team_id: "G1", tenant_id: "ten-1" }, received_at: "",
  });
  const msg = (id: string, time: string, extra: object = {}) => ({ id, createdDateTime: time, messageType: "message", from: { user: { displayName: "Al", id: "u1" } }, body: { contentType: "html", content: `<p>${id}</p>` }, ...extra });

  it("reads the thread in time order and lists attachments by name", async () => {
    const { k, h } = setup();
    k.ff
      .on("/messages/77", () => msg("77", "2026-01-01T00:00:01Z"))
      .on("/messages/77/replies", () => ({ value: [msg("r2", "2026-01-01T00:00:03Z"), msg("r1", "2026-01-01T00:00:02Z", { attachments: [{ name: "spec.pdf", contentType: "file" }] }), msg("sys", "2026-01-01T00:00:04Z", { messageType: "systemEventMessage" }), msg("del", "2026-01-01T00:00:05Z", { deletedDateTime: "x" })] }));
    const res = await h.eventContext(ev());
    const entries = JSON.parse(new TextDecoder().decode(res.files[0]!.data)) as ThreadEntry[];
    expect(entries.map((e) => e.id)).toEqual(["77", "r1", "r2"]);
    expect(entries[1]).toMatchObject({ text: "r1", user: "Al", user_id: "u1", attachments: [{ name: "spec.pdf", content_type: "file" }] });
    expect(res.omitted).toEqual([]);
    const graph = k.ff.calls.find((c) => c.url.includes("/teams/G1/"))!;
    expect(graph.headers.get("Authorization")).toBe("Bearer bot-token");
    expect(k.ff.calls.find((c) => c.url.endsWith("/token"))!.body).toContain("graph.microsoft.com");
  });

  it("reports a refused read with the permission to add", async () => {
    const { k, h } = setup();
    k.ff.on("/messages/77", () => new Response("denied", { status: 403 }));
    const res = await h.eventContext(ev());
    expect(res.files).toEqual([]);
    expect(res.omitted[0]!.reason).toContain("ChannelMessage.Read.Group");
    await expect(h.eventContext({ ...ev(), attributes: {} })).rejects.toThrow(/no team ID/);
  });

  it("follows nextLink only on the Graph host", async () => {
    const { k, h } = setup();
    const c = await h.graphClient(k.store.integrations[0]!, "");
    const seen: string[] = [];
    k.ff.on("/p1", () => ({ value: [{ n: 1 }], "@odata.nextLink": "https://evil.test/p2" }));
    await h.graphPages(c, "/p1", (items) => {
      seen.push(...items.map((i) => String(i["n"])));
      return true;
    });
    expect(seen).toEqual(["1"]);
    expect(k.ff.calls.some((x) => x.url.includes("evil.test"))).toBe(false);
  });

  it("needs a tenant for Graph", async () => {
    const { k, h } = setup();
    delete k.store.integrations[0]!.config!["tenant_id"];
    await expect(h.graphClient(k.store.integrations[0]!, "")).rejects.toThrow(/needs the Microsoft 365 tenant/);
    expect(await h.graphClient(k.store.integrations[0]!, "from-event")).toBeDefined();
  });

  it("fits a long thread: first message and the newest replies", () => {
    const entries: ThreadEntry[] = Array.from({ length: 300 }, (_, i) => ({ id: String(i), text: "x" }));
    const { data, left } = fitThread(entries);
    const out = JSON.parse(new TextDecoder().decode(data)) as ThreadEntry[];
    expect(out).toHaveLength(200);
    expect(out[0]!.id).toBe("0");
    expect(out[1]!.id).toBe("101");
    expect(left).toBe(100);
    const big: ThreadEntry[] = Array.from({ length: 150 }, (_, i) => ({ id: String(i), text: "y".repeat(5000) }));
    const fit = fitThread(big);
    expect(fit.data.length).toBeLessThanOrEqual(256 << 10);
    expect(JSON.parse(new TextDecoder().decode(fit.data))[0].id).toBe("0");
    expect(fit.left).toBeGreaterThan(0);
  });

  it("helpers: cleanText, threadRoot, threadEntry", () => {
    expect(cleanText("<at>Bot</at> a&nbsp;&amp; b<br>c <b>d</b>&lt;")).toBe("a & b\nc d<");
    expect(threadRoot("19:c;messageid=12", "act")).toEqual({ channel: "19:c", root: "12" });
    expect(threadRoot("19:c;messageid=", "act")).toEqual({ channel: "19:c", root: "act" });
    expect(threadRoot("19:c", "act")).toEqual({ channel: "19:c", root: "act" });
    expect(threadEntry({ id: "", body: {} })).toBeNull();
    expect(threadEntry({ id: "1", from: { application: { displayName: "Bot" } }, body: { content: "t" } })).toEqual({ id: "1", user: "Bot", bot: true, text: "t" });
  });
});

describe("teams name resolver", () => {
  it("resolves channels and users through Graph, scoped by team", async () => {
    const { k, h } = setup();
    k.store.integrations.push(integration({ id: "int_t2", provider: "microsoft_teams", secret_ref: "BOT_PW", config: { team_id: "G2", tenant_id: "ten-1", app_id: APP } }));
    k.ff
      .on("/teams/G1/channels", () => ({ value: [{ id: "ch1", displayName: "Dev" }] }))
      .on("/teams/G2/channels", () => ({ value: [{ id: "ch2", displayName: "dev" }] }))
      .on("/teams/G1/members", () => ({ value: [{ userId: "o1", displayName: "Al", email: "al@example.com" }] }));
    const r = new TeamsNameResolver(h);
    expect(await r.resolveNames("team_1", "teams", "channels", ["#dev"])).toEqual({ "#dev": ["ch1", "ch2"] });
    expect(await r.resolveNames("team_1", "microsoft_teams", "channels", ["dev"], { team_ids: ["g2"] })).toEqual({ dev: ["ch2"] });
    expect(await r.resolveNames("team_1", "teams", "users", ["AL@example.com"], { team_ids: ["G1"] })).toEqual({ "AL@example.com": ["o1"] });
    await expect(r.resolveNames("team_1", "teams", "users", ["x"], { team_ids: ["G9"] })).rejects.toThrow(/no active Teams integration/);
    await expect(r.resolveNames("team_1", "teams", "groups", ["x"])).rejects.toThrow(/cannot resolve/);
  });

  it("names the permission when Graph refuses", async () => {
    const { k, h } = setup();
    k.ff.on("/teams/G1/channels", () => new Response("no", { status: 403 }));
    await expect(new TeamsNameResolver(h).resolveNames("team_1", "teams", "channels", ["x"])).rejects.toThrow(/ChannelSettings.Read.Group/);
  });
});
