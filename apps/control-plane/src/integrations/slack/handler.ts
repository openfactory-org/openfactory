// Slack Events API and interactivity receiver: URL verification, v0 request
// signing, mentions, messages, DMs, reactions, member joins and approval
// buttons. Port of slack/handler.go.
//
// Integration records (provider "slack"), one per workspace and factory:
//   config team_id (workspace, necessary), api_app_id, bot_user_id,
//   signing_secret_ref, link_by_email; secret_ref: the bot token (xoxb-).
import { integrationWorkspace, newId } from "@openfactory/core";
import type { Actor, Event, Integration, SourceRef } from "@openfactory/core";
import { EVENT_ATTR as A, EVENT_PROVIDER } from "@openfactory/core";
import { intake } from "../select";
import { autoRespondToThreadReplies, member, openInboxItem, privateUrl } from "../shared";
import type { IntegrationDeps, Logger, WebhookOutcome } from "../types";
import { consoleLogger } from "../types";
import { Dedupe, constantTimeEqual, errMessage, hmacSha256Hex, integrationSecret, equalFold, setCapped } from "../util";
import { handleIntegrationWebhook } from "../webhook";
import { SlackClient, respondUrl, textOfBlocks, type UserInfo } from "./client";
import { ACTION_PREFIX, type ActionValue } from "./responder";

export const PROVIDER = EVENT_PROVIDER.Slack;
export const EVENTS_PATH = "/integrations/slack/events";
export const INTERACTIONS_PATH = "/integrations/slack/interactions";
/** Bounds the age of signed requests (replay protection). */
export const DEFAULT_TIMESTAMP_TOLERANCE_MS = 5 * 60_000;
/** The config key of the opt-in that links a Slack account by e-mail address. */
export const CONFIG_LINK_BY_EMAIL = "link_by_email";

const SIGNATURE_FAILED = { code: "invalid_signature", message: "Slack request signature verification failed" };

/** The v0 signature of a request. */
export async function sign(secret: string, timestamp: string, body: string): Promise<string> {
  return "v0=" + (await hmacSha256Hex(secret, `v0:${timestamp}:${body}`));
}

interface EventEnvelope {
  type?: string;
  challenge?: string;
  team_id?: string;
  api_app_id?: string;
  event_id?: string;
  event_time?: number;
  event?: SlackEvent;
}

interface SlackEvent {
  type?: string;
  subtype?: string;
  user?: string;
  bot_id?: string;
  app_id?: string;
  text?: string;
  ts?: string;
  thread_ts?: string;
  channel?: string;
  channel_type?: string;
  event_ts?: string;
  blocks?: unknown[];
  attachments?: unknown[];
  bot_profile?: { app_id?: string } | null;
  reaction?: string;
  item_user?: string;
  item?: { type?: string; channel?: string; ts?: string } | null;
  inviter?: string;
}

interface Interaction {
  type?: string;
  api_app_id?: string;
  team?: { id?: string };
  user?: { id?: string; username?: string };
  channel?: { id?: string };
  message?: { ts?: string; thread_ts?: string; text?: string; blocks?: Record<string, unknown>[] };
  response_url?: string;
  actions?: { action_id: string; value: string }[];
}

/** A permalink-style URL for a message. */
export function messageUrl(channel: string, ts: string): string {
  if (channel === "" || ts === "") return "";
  return "https://slack.com/archives/" + channel + "/p" + ts.replaceAll(".", "");
}

const escapeText = (s: string): string => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

const CACHE_MS = 10 * 60_000;

export class SlackHandler {
  private readonly log: Logger;
  private readonly dedupe = new Dedupe(3600_000);
  private readonly users = new Map<string, { u: UserInfo; expires: number }>();
  private readonly channels = new Map<string, { name: string; expires: number }>();

  constructor(
    readonly deps: IntegrationDeps,
    private readonly opts: { apiBaseUrl?: string; toleranceMs?: number } = {},
  ) {
    this.log = deps.log ?? consoleLogger;
  }

  private now(): Date {
    return this.deps.now ? this.deps.now() : new Date();
  }

  private async checkSignature(secret: string, req: Request, body: string): Promise<boolean> {
    const ts = req.headers.get("X-Slack-Request-Timestamp") ?? "";
    const sig = req.headers.get("X-Slack-Signature") ?? "";
    if (secret === "" || ts === "" || !sig.startsWith("v0=")) return false;
    if (!/^-?\d+$/.test(ts)) return false;
    const age = this.now().getTime() - Number(ts) * 1000;
    const tol = this.opts.toleranceMs ?? DEFAULT_TIMESTAMP_TOLERANCE_MS;
    if (age > tol || age < -tol) return false;
    return constantTimeEqual(await sign(secret, ts, body), sig);
  }

  /** Checks the request against the secret of the matching record, or (no record, such as url_verification) of every Slack record. */
  private async verify(req: Request, body: string, integ: Integration | null): Promise<boolean> {
    let cands: Integration[];
    if (integ) cands = [integ];
    else {
      try {
        cands = (await this.deps.store.listIntegrations("")).filter((i) => equalFold(i.provider, PROVIDER));
      } catch {
        cands = [];
      }
    }
    for (const in_ of cands) {
      if (!in_.config?.["signing_secret_ref"]) continue;
      try {
        const sec = await integrationSecret(this.deps.secrets, in_, "signing_secret_ref");
        if (await this.checkSignature(sec, req, body)) return true;
      } catch (e) {
        this.log.warn("load signing secret", { integration: in_.id, err: errMessage(e) });
      }
    }
    return false;
  }

  /** The record of an event or interaction: records whose team_id is the workspace; records that name the app (api_app_id) win. */
  private async findIntegration(appId: string, teamId: string) {
    const exact = (i: Integration): boolean => appId !== "" && equalFold(i.config?.["api_app_id"] ?? "", appId);
    const all = (await this.deps.store.listIntegrations("")).filter((i) => equalFold(i.provider, PROVIDER));
    const named = all.filter((i) => exact(i) && equalFold(integrationWorkspace(i), teamId));
    let match = (i: Integration): boolean => (i.config?.["api_app_id"] ?? "") === "" || exact(i);
    if (named.length > 0) match = exact;
    return intake(this.deps.store, PROVIDER, teamId, match);
  }

  private async client(in_: Integration): Promise<SlackClient> {
    const tok = await integrationSecret(this.deps.secrets, in_);
    return new SlackClient(this.opts.apiBaseUrl ?? "", tok, this.deps.fetch);
  }

  async handleEvents(req: Request): Promise<WebhookOutcome> {
    return handleIntegrationWebhook<EventEnvelope>(req, {
      parse: (body) => JSON.parse(body) as EventEnvelope,
      invalidPayload: "payload is not valid JSON",
      // url_verification names no workspace: verify checks it against every Slack record.
      select: async (env) =>
        env.type === "url_verification"
          ? { integration: null, error: null }
          : this.findIntegration(env.api_app_id ?? "", env.team_id ?? ""),
      verify: (r, body, _env, found) => this.verify(r, body, found.integration),
      unauthorized: SIGNATURE_FAILED,
      accept: (env, found) => {
        if (env.type === "url_verification") {
          return {
            response: new Response(env.challenge ?? "", { status: 200, headers: { "Content-Type": "text/plain" } }),
          };
        }
        if (env.type !== "event_callback") return { response: new Response(null, { status: 200 }) };
        if (found.error || !found.integration) {
          this.log.info("event ignored", { app: env.api_app_id, team: env.team_id, err: found.error?.message });
          return { response: new Response(null, { status: 200 }) };
        }
        if (this.dedupe.isSeen(env.event_id ?? "")) return { response: new Response(null, { status: 200 }) };
        const in_ = found.integration;
        // Slack expects an answer within 3 seconds: the work runs after the response.
        return {
          response: new Response(null, { status: 200 }),
          background: async () => {
            try {
              await this.handleEvent(in_, env);
            } catch (e) {
              this.log.warn("handle slack event", { event_id: env.event_id, err: errMessage(e) });
            }
          },
        };
      },
    });
  }

  private async userInfo(c: SlackClient | null, id: string): Promise<UserInfo | null> {
    if (id === "" || !c) return null;
    const cu = this.users.get(id);
    if (cu && this.now().getTime() < cu.expires) return cu.u;
    try {
      const u = await c.userInfo(id);
      setCapped(this.users, id, { u, expires: this.now().getTime() + CACHE_MS });
      return u;
    } catch (e) {
      this.log.debug("users.info", { user: id, err: errMessage(e) });
      return null;
    }
  }

  private async channelName(c: SlackClient | null, id: string): Promise<string> {
    if (id === "" || !c || id.startsWith("D")) return "";
    const cn = this.channels.get(id);
    if (cn && this.now().getTime() < cn.expires) return cn.name;
    try {
      const name = await c.channelName(id);
      setCapped(this.channels, id, { name, expires: this.now().getTime() + CACHE_MS });
      return name;
    } catch (e) {
      this.log.debug("conversations.info", { channel: id, err: errMessage(e) });
      return "";
    }
  }

  /** Whether the event was posted by any Slack app registered on this server. */
  private async isOwnApp(ev: SlackEvent): Promise<boolean> {
    if (!ev.bot_id && !ev.app_id && !ev.bot_profile) return false;
    try {
      const all = (await this.deps.store.listIntegrations("")).filter((i) => equalFold(i.provider, PROVIDER));
      return all.some((i) => this.isSelf(i, ev));
    } catch {
      return false;
    }
  }

  private isSelf(in_: Integration, ev: SlackEvent): boolean {
    const appId = in_.config?.["api_app_id"] ?? "";
    const bu = in_.config?.["bot_user_id"] ?? "";
    if (bu !== "" && ev.user === bu) return true;
    if (appId === "") return false;
    return ev.app_id === appId || ev.bot_profile?.app_id === appId;
  }

  private async handleEvent(in_: Integration, env: EventEnvelope): Promise<void> {
    const ev = env.event;
    if (!ev) throw new Error("decode event: no event");
    if (this.isSelf(in_, ev)) return; // posts by the receiving app are ignored
    let c: SlackClient | null = null;
    try {
      c = await this.client(in_);
    } catch (e) {
      this.log.warn("slack bot token unavailable; continuing without Web API enrichment", {
        integration: in_.id,
        err: errMessage(e),
      });
    }
    const factoryId = in_.factory_id ?? "";
    let typ: string;
    let requireLink = false;
    let channel = ev.channel ?? "";
    let threadTs = ev.thread_ts || ev.ts || "";
    let text = ((ev.text ?? "") + "\n" + textOfBlocks(ev.blocks, ev.attachments)).trim();
    const actorId = ev.user ?? "";
    const attrs: Record<string, unknown> = {};
    switch (ev.type) {
      case "app_mention":
        if (ev.subtype && ev.subtype !== "thread_broadcast" && ev.subtype !== "file_share") return;
        typ = "app_mention";
        requireLink = true;
        break;
      case "message":
        switch (ev.subtype ?? "") {
          case "":
          case "file_share":
          case "thread_broadcast":
            break;
          default:
            return; // edits, deletions, joins and other system messages
        }
        switch (ev.channel_type) {
          case "im":
            typ = "message_im";
            requireLink = true;
            break;
          case "mpim":
            typ = "message_mpim";
            requireLink = true;
            break;
          default:
            typ = "message_posted";
        }
        break;
      case "reaction_added": {
        if (!ev.item || ev.item.type !== "message") return;
        typ = "reaction_added";
        channel = ev.item.channel ?? "";
        threadTs = ev.item.ts ?? "";
        text = "";
        attrs[A.Emoji] = ev.reaction ?? "";
        attrs[A.ItemUserID] = ev.item_user ?? "";
        const iu = await this.userInfo(c, ev.item_user ?? "");
        if (iu) {
          attrs[A.ItemUserName] = iu.name;
          attrs[A.ItemUserEmail] = iu.profile?.email ?? "";
        }
        if (c) {
          try {
            const m = await c.getMessage(channel, threadTs);
            text = ((m.text ?? "") + "\n" + textOfBlocks(m.blocks, m.attachments)).trim();
            if (m.thread_ts) threadTs = m.thread_ts;
          } catch (e) {
            this.log.debug("fetch reacted message", { err: errMessage(e) });
          }
        }
        break;
      }
      case "member_joined_channel":
        typ = "member_joined_channel";
        threadTs = "";
        text = "";
        break;
      default:
        return;
    }

    const user = await this.userInfo(c, actorId);
    const isBot = !!ev.bot_id || !!ev.app_id || !!user?.is_bot;
    const actor: Actor = { provider: PROVIDER, external_id: actorId, handle: user?.name ?? "", is_bot: isBot };

    // Who can start work: mentions and DMs need a Slack account linked to an active team member.
    let mem = null;
    try {
      mem = await member(this.deps.store, in_.team_id, PROVIDER, actorId);
    } catch (e) {
      this.log.warn("membership lookup", { user: actorId, err: errMessage(e) });
    }
    const email = user?.profile?.email ?? "";
    if (!mem && user && email !== "" && !isBot && linkByEmail(in_) && this.deps.linker) {
      const ws = in_.config?.["team_id"] ?? "";
      try {
        mem = await this.deps.linker.linkByEmail(in_.team_id, {
          provider: PROVIDER,
          externalId: actorId,
          handle: user.name,
          email,
          guest: !!user.is_restricted || !!user.is_ultra_restricted,
          foreign: !!user.is_stranger || (ws !== "" && user.team_id !== ws) || (ws === "" && !user.team_id),
          bot: !!user.is_bot,
          deleted: !!user.deleted,
        });
      } catch (e) {
        this.log.warn("link by email", { user: actorId, err: errMessage(e) });
      }
    }
    if (mem) actor.user_id = mem.id;
    if (requireLink && !mem) {
      await this.promptLink(c, in_, channel, ev, actor);
      return;
    }

    const ws = integrationWorkspace(in_);
    let source: SourceRef = {
      provider: PROVIDER,
      kind: "thread",
      channel,
      thread_id: threadTs,
      url: messageUrl(channel, threadTs),
      workspace: ws,
    };
    if (typ === "member_joined_channel") {
      source = {
        provider: PROVIDER,
        kind: "channel",
        channel,
        external_id: `${channel}:${actorId}:${ev.event_ts ?? ""}`,
        workspace: ws,
      };
    }
    attrs[A.ChannelID] = channel;
    attrs[A.ChannelType] = ev.channel_type ?? "";
    attrs[A.UserID] = actorId;
    attrs[A.ActorIsBot] = isBot;
    attrs[A.ActorIsSelf] = await this.isOwnApp(ev);
    attrs[A.Linked] = !!mem;
    if (user) {
      attrs[A.UserName] = user.name;
      attrs[A.UserEmail] = email;
    }
    const cname = await this.channelName(c, channel);
    if (cname !== "") attrs[A.ChannelName] = cname;
    if (ev.type === "message" && ev.thread_ts && ev.thread_ts !== ev.ts) {
      attrs[A.ThreadReply] = true;
      attrs[A.AutoFollowUp] = await autoRespondToThreadReplies(this.deps.store, factoryId, "slack");
    }
    if (text !== "") attrs[A.Text] = text;
    const out: Event = {
      id: newId("evt"),
      team_id: in_.team_id,
      factory_id: factoryId,
      provider: PROVIDER,
      type: typ,
      delivery_id: env.event_id ?? "",
      actor,
      source,
      attributes: attrs,
      payload: ev,
      text,
      received_at: this.now().toISOString(),
    };
    await this.deps.publish([out]);
    // Acknowledge explicit requests.
    if (requireLink && c) {
      try {
        await c.addReaction(channel, ev.ts ?? "", "eyes");
      } catch (e) {
        this.log.debug("acknowledge reaction", { err: errMessage(e) });
      }
    }
  }

  private async promptLink(
    c: SlackClient | null,
    in_: Integration,
    channel: string,
    ev: SlackEvent,
    actor: Actor,
  ): Promise<void> {
    if (!c) return;
    let msg =
      "To start work from Slack, link your Slack account to an active member of this factory's Open Factory team, then send your request again.";
    // An ephemeral message is private to the user, so it can carry the URL.
    const u = await privateUrl(this.deps.linker, in_.team_id, PROVIDER, actor.external_id, actor.handle);
    if (u !== "")
      msg = `To start work from Slack, <${u}|link your Slack account> to your Open Factory account, then send your request again.`;
    try {
      await c.postEphemeral({
        channel,
        user: actor.external_id,
        text: msg,
        ...(ev.thread_ts ? { thread_ts: ev.thread_ts } : {}),
      });
    } catch (e) {
      this.log.warn("post link prompt", { err: errMessage(e) });
    }
  }

  // ---------- Interactivity ----------

  async handleInteractions(req: Request): Promise<WebhookOutcome> {
    return handleIntegrationWebhook<Interaction>(req, {
      parse: (body) => JSON.parse(new URLSearchParams(body).get("payload") ?? "") as Interaction,
      invalidPayload: "invalid interaction payload",
      select: (in_) => this.findIntegration(in_.api_app_id ?? "", in_.team?.id ?? ""),
      verify: (r, body, _in, found) => this.verify(r, body, found.integration),
      unauthorized: SIGNATURE_FAILED,
      accept: (in_, found) => {
        const ok = new Response(null, { status: 200 });
        if (
          found.error ||
          !found.integration ||
          in_.type !== "block_actions" ||
          !in_.actions ||
          in_.actions.length === 0
        ) {
          return { response: ok };
        }
        const act = in_.actions[0]!;
        if (!act.action_id.startsWith(ACTION_PREFIX)) return { response: ok };
        let v: ActionValue;
        try {
          v = JSON.parse(act.value) as ActionValue;
        } catch {
          return { response: ok };
        }
        const team = found.integration.team_id;
        return { response: ok, background: () => this.handleAction(team, in_, v) };
      },
    });
  }

  private async respond(url: string, body: Record<string, unknown>): Promise<void> {
    if (url === "") return;
    try {
      await respondUrl(this.deps.fetch, url, body);
    } catch (e) {
      this.log.warn("slack response_url", { err: errMessage(e) });
    }
  }

  private async handleAction(teamId: string, in_: Interaction, v: ActionValue): Promise<void> {
    const userId = in_.user?.id ?? "";
    const url = in_.response_url ?? "";
    let mem = null;
    try {
      mem = await member(this.deps.store, teamId, PROVIDER, userId);
    } catch (e) {
      this.log.warn("membership lookup", { err: errMessage(e) });
    }
    if (!mem) {
      let msg = "Only linked members of this factory's team can answer. Link your Slack account and try again.";
      const u = await privateUrl(this.deps.linker, teamId, PROVIDER, userId, in_.user?.username ?? "");
      if (u !== "")
        msg = `Only linked members of this factory's team can answer. <${u}|Link your Slack account> and try again.`;
      await this.respond(url, { response_type: "ephemeral", replace_original: false, text: msg });
      return;
    }
    let inboxId = v.i ?? "";
    if (inboxId === "" && v.w) {
      try {
        inboxId = (await openInboxItem(this.deps.work, v.w)).id;
      } catch {
        await this.respond(url, {
          response_type: "ephemeral",
          replace_original: false,
          text: "This request has already been answered or is no longer waiting for input.",
        });
        return;
      }
    }
    const onAction = this.deps.work?.onAction;
    if (!onAction || inboxId === "") {
      this.log.warn("slack action received but no inbox resolver configured");
      return;
    }
    try {
      await onAction(inboxId, v.o, mem.id);
    } catch (e) {
      this.log.warn("resolve inbox from slack", { inbox: inboxId, err: errMessage(e) });
      await this.respond(url, {
        response_type: "ephemeral",
        replace_original: false,
        text: "Your answer could not be recorded: " + errMessage(e),
      });
      return;
    }
    // Replace the buttons with the decision.
    const blocks = (in_.message?.blocks ?? []).filter((b) => b["type"] !== "actions");
    blocks.push({
      type: "context",
      elements: [{ type: "mrkdwn", text: `*${escapeText(v.o)}* selected by <@${userId}>` }],
    });
    await this.respond(url, { replace_original: true, text: in_.message?.text ?? "", blocks });
  }
}

/** Whether the record links accounts by e-mail address. Off by default. */
export function linkByEmail(in_: Integration): boolean {
  return ["true", "1", "yes"].includes((in_.config?.[CONFIG_LINK_BY_EMAIL] ?? "").trim().toLowerCase());
}
