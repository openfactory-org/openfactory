import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import { NOW, integration, kit, post, user } from "../testkit";
import { SlackHandler, messageUrl, sign } from "./handler";
import { SlackNameResolver } from "./resolver";
import { ACTION_PREFIX, SlackResponder, buildMessage, mrkdwn } from "./responder";
import { textOfBlocks } from "./client";

const SECRETS = { SLACK_TOKEN: "xoxb-1", SLACK_SIGN: "sign-secret" };
const ts = String(Math.floor(NOW.getTime() / 1000));

function setup(extra: Parameters<typeof kit>[1] = {}) {
  const k = kit(SECRETS, extra);
  k.store.integrations = [
    integration({ id: "int_s", provider: "slack", secret_ref: "SLACK_TOKEN", config: { team_id: "T1", api_app_id: "A1", bot_user_id: "UBOT", signing_secret_ref: "SLACK_SIGN" } }),
  ];
  k.store.users = [user({ id: "usr_1" })];
  k.store.identities.set("slack|U1", { user_id: "usr_1", provider: "slack", external_id: "U1", handle: "al" });
  k.ff
    .on("/users.info", (c) => ({ ok: true, user: { id: new URL(c.url).searchParams.get("user"), name: "al", team_id: "T1", profile: { email: "a@example.com" } } }))
    .on("/conversations.info", () => ({ ok: true, channel: { name: "dev" } }))
    .on("/reactions.add", () => ({ ok: true }))
    .on("/chat.postEphemeral", () => ({ ok: true }))
    .on("/chat.postMessage", () => ({ ok: true, ts: "1.1" }));
  return { k, h: new SlackHandler(k.deps) };
}

const events = async (body: object, over: { ts?: string; secret?: string } = {}) => {
  const text = JSON.stringify(body);
  const t = over.ts ?? ts;
  return post("https://of.example.com/integrations/slack/events", text, {
    "X-Slack-Request-Timestamp": t,
    "X-Slack-Signature": await sign(over.secret ?? "sign-secret", t, text),
  });
};

const envelope = (event: object, extra: object = {}) => ({ type: "event_callback", team_id: "T1", api_app_id: "A1", event_id: "Ev1", event, ...extra });

describe("slack events", () => {
  it("answers url_verification only for a valid signature", async () => {
    const { h } = setup();
    const ok = await h.handleEvents(await events({ type: "url_verification", challenge: "abc" }));
    expect(ok.response.status).toBe(200);
    expect(await ok.response.text()).toBe("abc");
    expect((await h.handleEvents(await events({ type: "url_verification", challenge: "abc" }, { secret: "wrong" }))).response.status).toBe(401);
  });

  it("rejects an old timestamp and a missing signature", async () => {
    const { h } = setup();
    const old = String(Number(ts) - 600);
    expect((await h.handleEvents(await events(envelope({ type: "app_mention" }), { ts: old }))).response.status).toBe(401);
    expect((await h.handleEvents(post("https://x/e", JSON.stringify(envelope({}))))).response.status).toBe(401);
  });

  it("publishes a linked app_mention with attributes and acknowledges with a reaction", async () => {
    const { k, h } = setup();
    const out = await h.handleEvents(await events(envelope({ type: "app_mention", user: "U1", text: "<@UBOT> fix it", ts: "100.5", channel: "C1" })));
    expect(out.response.status).toBe(200);
    expect(k.published).toHaveLength(0); // the work runs after the response
    await out.background!();
    expect(k.published).toHaveLength(1);
    expect(k.published[0]).toMatchObject({
      type: "app_mention",
      delivery_id: "Ev1",
      actor: { external_id: "U1", handle: "al", user_id: "usr_1", is_bot: false },
      source: { kind: "thread", channel: "C1", thread_id: "100.5", url: "https://slack.com/archives/C1/p1005", workspace: "T1" },
      text: "<@UBOT> fix it",
    });
    expect(k.published[0]!.attributes).toMatchObject({ channel_id: "C1", channel_name: "dev", user_name: "al", user_email: "a@example.com", linked: true, actor_is_self: false });
    const reaction = k.ff.calls.find((c) => c.url.endsWith("/reactions.add"))!;
    expect(JSON.parse(reaction.body)).toEqual({ channel: "C1", timestamp: "100.5", name: "eyes" });
    expect(reaction.headers.get("Authorization")).toBe("Bearer xoxb-1");
  });

  it("asks an unlinked user in an ephemeral message with a private link", async () => {
    const { k, h } = setup();
    const out = await h.handleEvents(await events(envelope({ type: "app_mention", user: "U9", text: "hi", ts: "1.0", thread_ts: "0.9", channel: "C1" })));
    await out.background!();
    expect(k.published).toHaveLength(0);
    const eph = k.ff.calls.find((c) => c.url.endsWith("/chat.postEphemeral"))!;
    const b = JSON.parse(eph.body) as { user: string; thread_ts: string; text: string };
    expect(b).toMatchObject({ user: "U9", thread_ts: "0.9" });
    expect(b.text).toContain("https://of.example.com/integrations/link/");
  });

  it("marks thread replies and respects autoRespondToThreadReplies", async () => {
    const { k, h } = setup();
    k.store.integrations[0]!.factory_id = "fac_1";
    k.store.settings["fac_1"] = { slack: { autoRespondToThreadReplies: "false" } };
    const out = await h.handleEvents(await events(envelope({ type: "message", user: "U1", text: "more", ts: "2.0", thread_ts: "1.0", channel: "C1", channel_type: "channel" })));
    await out.background!();
    expect(k.published[0]).toMatchObject({ type: "message_posted", factory_id: "fac_1" });
    expect(k.published[0]!.attributes).toMatchObject({ thread_reply: true, auto_follow_up: false });
  });

  it("ignores edits, own bot posts and unknown workspaces, and dedupes", async () => {
    const { k, h } = setup();
    const run = async (event: object, extra: object = {}) => {
      const out = await h.handleEvents(await events(envelope(event, extra)));
      await out.background?.();
      return out;
    };
    await run({ type: "message", subtype: "message_changed", channel: "C1" }, { event_id: "e1" });
    await run({ type: "message", user: "UBOT", text: "x", ts: "1", channel: "C1", channel_type: "im" }, { event_id: "e2" });
    const unknown = await h.handleEvents(await events(envelope({ type: "message" }, { team_id: "T9" })));
    expect(unknown.response.status).toBe(200); // verified with the secrets of all records, then ignored
    expect(k.published).toHaveLength(0);
    await run({ type: "message", user: "U1", text: "x", ts: "5", channel: "C1", channel_type: "channel" }, { event_id: "e3" });
    await run({ type: "message", user: "U1", text: "x", ts: "5", channel: "C1", channel_type: "channel" }, { event_id: "e3" });
    expect(k.published).toHaveLength(1);
  });

  it("marks a post by another Slack app of this server as self and bot", async () => {
    const { k, h } = setup();
    k.store.integrations.push(integration({ id: "int_2", provider: "slack", config: { team_id: "T2", api_app_id: "A2" } }));
    const out = await h.handleEvents(await events(envelope({ type: "message", user: "U7", bot_id: "B1", app_id: "A2", text: "x", ts: "5", channel: "C1", channel_type: "channel" })));
    await out.background!();
    expect(k.published[0]!.attributes).toMatchObject({ actor_is_self: true, actor_is_bot: true });
  });

  it("builds a reaction_added event from the reacted message", async () => {
    const { k, h } = setup();
    k.ff.on("/conversations.history", () => ({ ok: true, messages: [{ ts: "50.0", thread_ts: "40.0", text: "the message" }] }));
    const out = await h.handleEvents(await events(envelope({ type: "reaction_added", user: "U1", reaction: "rocket", item_user: "U2", item: { type: "message", channel: "C1", ts: "50.0" } })));
    await out.background!();
    expect(k.published[0]).toMatchObject({ type: "reaction_added", text: "the message", source: { thread_id: "40.0", channel: "C1" } });
    expect(k.published[0]!.attributes).toMatchObject({ emoji: "rocket", item_user_id: "U2", item_user_email: "a@example.com" });
  });

  it("links by email when the record opts in", async () => {
    const { k, h } = setup();
    k.store.integrations[0]!.config!["link_by_email"] = "true";
    const out = await h.handleEvents(await events(envelope({ type: "app_mention", user: "U5", text: "hi", ts: "1.0", channel: "C1" })));
    await out.background!();
    expect(k.store.identities.get("slack|U5")?.user_id).toBe("usr_1");
    expect(k.published).toHaveLength(1);
  });
});

describe("slack interactions", () => {
  const interaction = async (value: object, userId = "U1") => {
    const payload = {
      type: "block_actions",
      api_app_id: "A1",
      team: { id: "T1" },
      user: { id: userId, username: "al" },
      response_url: "https://hooks.slack.test/r",
      message: { text: "Approve?", blocks: [{ type: "section" }, { type: "actions" }] },
      actions: [{ action_id: ACTION_PREFIX + "0", value: JSON.stringify(value) }],
    };
    const body = new URLSearchParams({ payload: JSON.stringify(payload) }).toString();
    return post("https://of.example.com/integrations/slack/interactions", body, {
      "Content-Type": "application/x-www-form-urlencoded",
      "X-Slack-Request-Timestamp": ts,
      "X-Slack-Signature": await sign("sign-secret", ts, body),
    });
  };

  it("resolves the inbox item and replaces the buttons", async () => {
    const answers: unknown[][] = [];
    const { k, h } = setup({ work: { onAction: async (...a) => void answers.push(a), openInboxItem: async () => ({ id: "inb_9" }) } });
    k.ff.on("hooks.slack.test", () => ({}));
    const out = await h.handleInteractions(await interaction({ w: "wi_1", o: "Approve" }));
    expect(out.response.status).toBe(200);
    await out.background!();
    expect(answers).toEqual([["inb_9", "Approve", "usr_1"]]);
    const reply = JSON.parse(k.ff.calls.at(-1)!.body) as { replace_original: boolean; blocks: { type: string }[] };
    expect(reply.replace_original).toBe(true);
    expect(reply.blocks.map((b) => b.type)).toEqual(["section", "context"]);
    expect(JSON.stringify(reply.blocks[1])).toContain("*Approve* selected by <@U1>");
  });

  it("refuses an unlinked user with a private link", async () => {
    const answers: unknown[] = [];
    const { k, h } = setup({ work: { onAction: async (...a) => void answers.push(a) } });
    k.ff.on("hooks.slack.test", () => ({}));
    await (await h.handleInteractions(await interaction({ i: "inb_1", o: "Yes" }, "U9"))).background!();
    expect(answers).toEqual([]);
    const reply = JSON.parse(k.ff.calls.at(-1)!.body) as { response_type: string; text: string };
    expect(reply.response_type).toBe("ephemeral");
    expect(reply.text).toContain("/integrations/link/");
  });

  it("tells the user when the request is no longer open and when the answer fails", async () => {
    const t = setup({ work: { onAction: async () => { throw new OfError("conflict", "already resolved"); }, openInboxItem: async () => null } });
    t.k.ff.on("hooks.slack.test", () => ({}));
    await (await t.h.handleInteractions(await interaction({ w: "wi_1", o: "Yes" }))).background!();
    expect(t.k.ff.calls.at(-1)!.body).toContain("already been answered");
    const t2 = setup({ work: { onAction: async () => { throw new OfError("conflict", "already resolved"); } } });
    t2.k.ff.on("hooks.slack.test", () => ({}));
    await (await t2.h.handleInteractions(await interaction({ i: "inb_1", o: "Yes" }))).background!();
    expect(t2.k.ff.calls.at(-1)!.body).toContain("could not be recorded");
  });

  it("rejects a bad signature and ignores other actions", async () => {
    const { h } = setup();
    const bad = post("https://x/i", "payload=%7B%7D", { "X-Slack-Request-Timestamp": ts, "X-Slack-Signature": "v0=00" });
    expect((await h.handleInteractions(bad)).response.status).toBe(401);
  });
});

describe("slack responder", () => {
  const src = { provider: "slack", kind: "thread", channel: "C1", thread_id: "9.9", workspace: "T1" };

  it("posts into the thread with Block Kit and option buttons", async () => {
    const { k } = setup();
    const r = new SlackResponder(k.deps);
    await r.reply("team_1", { work_item_id: "wi_1", inbox_id: "inb_1", source: src, kind: "approval_request", text: "Ship **it**?", options: ["Approve", "Reject"] });
    const b = k.ff.json(0) as { channel: string; thread_ts: string; blocks: { type: string; elements?: { style?: string; value: string; action_id: string }[] }[] };
    expect(b).toMatchObject({ channel: "C1", thread_ts: "9.9" });
    expect(b.blocks.map((x) => x.type)).toEqual(["section", "actions"]);
    expect(b.blocks[1]!.elements![0]).toMatchObject({ style: "primary", action_id: "of_option_0", value: JSON.stringify({ o: "Approve", w: "wi_1", i: "inb_1" }) });
    expect(b.blocks[1]!.elements![1]!.style).toBeUndefined();
    await expect(r.reply("team_1", { work_item_id: "w", source: { ...src, channel: "" }, kind: "update", text: "x" })).rejects.toThrow(/no channel/);
  });

  it("uses the factory of the work item to select a record", async () => {
    const { k } = setup({ work: { factoryOf: async () => "fac_1" } });
    k.store.integrations.push(integration({ id: "int_f", provider: "slack", factory_id: "fac_1", secret_ref: "SLACK_TOKEN", config: { team_id: "T1" } }));
    k.ff.calls.length = 0;
    await new SlackResponder(k.deps).reply("team_1", { work_item_id: "wi_1", source: src, kind: "update", text: "x" });
    expect(k.ff.calls).toHaveLength(1);
  });

  it("converts markdown, splits long text and truncates buttons", () => {
    expect(mrkdwn("# Head\n**b** ~~s~~ [l](https://e.com) a<b & c")).toBe("*Head*\n*b* ~s~ <https://e.com|l> a&lt;b &amp; c");
    const m = buildMessage({ work_item_id: "w", source: src, kind: "update", text: "x\n".repeat(2500), options: Array.from({ length: 30 }, (_, i) => "o".repeat(100) + i) });
    expect(m.blocks!.length).toBe(3);
    const elems = (m.blocks![2] as { elements: { text: { text: string } }[] }).elements;
    expect(elems).toHaveLength(25);
    expect(Array.from(elems[0]!.text.text)).toHaveLength(75);
    expect(buildMessage({ work_item_id: "w", source: src, kind: "update", text: "" }).blocks![0]).toMatchObject({ text: { text: " " } });
  });

  it("reads thread messages and files, and refuses a foreign file host", async () => {
    const { k } = setup();
    k.ff
      .on("/conversations.replies", () => ({
        ok: true,
        messages: [
          { ts: "1.0", user: "U1", text: "root", files: [{ id: "F1", name: "../a.txt", size: 3, url_private_download: "https://files.slack.com/f/1" }] },
          { ts: "1.1", bot_id: "B1", text: "reply", files: [{ id: "F2", name: "evil.txt", size: 3, url_private_download: "https://evil.example.com/x" }, { id: "F3", name: "big", size: 5 << 20, url_private_download: "https://files.slack.com/f/3" }] },
        ],
      }))
      .on("files.slack.com/f/1", () => new Response("abc"));
    const res = await new SlackResponder(k.deps).eventContext({
      id: "e", team_id: "team_1", provider: "slack", type: "app_mention", actor: { provider: "slack", external_id: "U1", handle: "", is_bot: false },
      source: { provider: "slack", kind: "thread", channel: "C1", thread_id: "1.0", workspace: "T1" }, attributes: {}, received_at: "",
    });
    expect(res.files.map((f) => f.path)).toEqual(["thread.json", "files/F1-a.txt"]);
    const thread = JSON.parse(new TextDecoder().decode(res.files[0]!.data)) as { ts: string; bot?: boolean; files?: string[] }[];
    expect(thread[0]).toEqual({ ts: "1.0", user: "U1", text: "root", files: ["context/files/F1-a.txt"] });
    expect(thread[1]!.bot).toBe(true);
    expect(res.omitted.map((o) => o.name)).toEqual(["evil.txt", "big"]);
    expect(res.omitted[0]!.reason).toContain("files.slack.com");
    expect(new TextDecoder().decode(res.files[1]!.data)).toBe("abc");
    const dl = k.ff.calls.find((c) => c.url.endsWith("/f/1"))!;
    expect(dl.headers.get("Authorization")).toBe("Bearer xoxb-1");
    expect(k.ff.calls.some((c) => c.url.includes("evil.example.com"))).toBe(false);
  });

  it("reports a failed thread read as an omitted file", async () => {
    const { k } = setup();
    k.ff.on("/conversations.replies", () => ({ ok: false, error: "missing_scope" }));
    const res = await new SlackResponder(k.deps).eventContext({
      id: "e", team_id: "team_1", provider: "slack", type: "x", actor: { provider: "slack", external_id: "", handle: "", is_bot: false },
      source: { provider: "slack", kind: "thread", channel: "C1", thread_id: "1.0", workspace: "T1" }, attributes: {}, received_at: "",
    });
    expect(res.files).toEqual([]);
    expect(res.omitted[0]).toEqual({ name: "context/thread.json", reason: "slack conversations.replies: missing_scope" });
  });
});

describe("slack name resolver", () => {
  it("resolves channels and users, falls back to public channels", async () => {
    const { k } = setup();
    k.ff
      .on("/conversations.list", (c) => (c.url.includes("private_channel") ? { ok: false, error: "missing_scope" } : { ok: true, channels: [{ id: "C1", name: "Dev" }, { id: "C2", name: "dev2" }] }))
      .on("/users.list", () => ({ ok: true, members: [{ id: "U1", name: "al", profile: { email: "Al@Example.com" } }, { id: "U2", name: "gone", deleted: true }] }));
    const r = new SlackNameResolver(k.deps);
    expect(await r.resolveNames("team_1", "slack", "channels", ["#dev", "Missing"])).toEqual({ "#dev": ["C1"] });
    expect(await r.resolveNames("team_1", "slack", "users", ["al@example.com", "gone"])).toEqual({ "al@example.com": ["U1"] });
    await expect(r.resolveNames("team_1", "slack", "teams", [])).rejects.toThrow(/cannot resolve/);
  });

  it("names the missing scopes", async () => {
    const { k } = setup();
    k.ff.on("/users.list", () => ({ ok: false, error: "missing_scope" }));
    await expect(new SlackNameResolver(k.deps).resolveNames("team_1", "slack", "users", ["x"])).rejects.toThrow(/users:read and users:read.email/);
    const k2 = setup();
    k2.k.ff.on("/conversations.list", () => ({ ok: false, error: "invalid_auth" }));
    await expect(new SlackNameResolver(k2.k.deps).resolveNames("team_1", "slack", "channels", ["x"])).rejects.toThrow(/refused the bot token/);
  });
});

describe("slack helpers", () => {
  it("builds message URLs and extracts block text", () => {
    expect(messageUrl("C1", "1700.25")).toBe("https://slack.com/archives/C1/p170025");
    expect(messageUrl("", "1")).toBe("");
    const t = textOfBlocks([{ type: "section", text: { type: "mrkdwn", text: "one" }, fields: [{ text: "two" }] }], [{ pretext: "pre", fallback: "fb", text: "three" }]);
    expect(t.split("\n").sort()).toEqual(["fb", "one", "pre", "three", "two"]);
  });
});
