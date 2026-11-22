// Microsoft Teams through the Bot Framework: the activities endpoint (Bot
// Connector JWT validation), mentions, sign in / sign out / help commands,
// replies in the originating thread, Microsoft Graph thread context.
// Port of teams/handler.go, graph.go and the Handler part of context.go.
//
// Integration records (provider "microsoft_teams"), one per connected team:
//   config team_id (Graph team UUID, necessary), tenant_id, app_type, app_id
//   (necessary), channel_ids; secret_ref: the bot app password.
import { OfError, integrationWorkspace, newId } from "@openfactory/core";
import type { Actor, Event, Integration, OutboundMessage, SourceRef } from "@openfactory/core";
import { A, INTEGRATION_TEAMS, PROVIDER_TEAMS } from "../eventattr";
import { ApiClient, ApiError } from "../http";
import { intake, selectTeam } from "../select";
import { autoRespondToThreadReplies, dashboardHint, member } from "../shared";
import type {
  EventContextBuilder,
  EventContextOmitted,
  EventContextResult,
  IntegrationDeps,
  Logger,
  Responder,
  WebhookOutcome,
} from "../types";
import { consoleLogger } from "../types";
import { Dedupe, cfg, equalFold, errMessage, errorResponse, formatOutbound, integrationSecret, readBody, stringSet } from "../util";
import { DEFAULT_TOKEN_TENANT, GRAPH_SCOPE, KeySet, TokenSource } from "./auth";
import { GRAPH_PAGE_SIZE, MAX_THREAD_MESSAGES, cleanText, fitThread, threadEntry, type ThreadEntry } from "./context";
import { EVENT_CONTEXT_MAX_PAYLOAD_BYTES } from "../types";

export const PROVIDER = PROVIDER_TEAMS;
export const INTEGRATION_PROVIDER = INTEGRATION_TEAMS;
/** The factory.yaml integrations[].type. */
export const DEFINITION_TYPE = "microsoft-teams";
export const MESSAGES_PATH = "/integrations/teams/messages";
/** Used when no serviceUrl was recorded for a conversation. */
export const DEFAULT_SERVICE_URL = "https://smba.trafficmanager.net/teams/";
export const NOT_CONNECTED_MESSAGE =
  "This Microsoft Teams channel isn’t connected to an Open Factory factory. Connect this team to a factory, then mention this bot again.";
export const DEFAULT_GRAPH_URL = "https://graph.microsoft.com/v1.0";
const MAX_GRAPH_PAGES = 50;

export interface Account {
  id: string;
  name?: string;
  aadObjectId?: string;
}

/** The subset of a Bot Framework activity used here. */
export interface Activity {
  type?: string;
  id: string;
  serviceUrl?: string;
  channelId?: string;
  from?: Account | null;
  recipient?: Account | null;
  conversation?: { id: string; conversationType?: string; tenantId?: string } | null;
  text?: string;
  entities?: { type: string; mentioned?: Account | null; text?: string }[];
  channelData?: {
    team?: { id?: string; aadGroupId?: string; name?: string } | null;
    channel?: { id?: string; name?: string } | null;
    tenant?: { id?: string } | null;
  } | null;
}

/** The channel ID and the root message ID of a thread from a conversation ID ("19:abc@thread.tacv2;messageid=1700000000000"). */
export function threadRoot(conversationId: string, activityId: string): { channel: string; root: string } {
  const i = conversationId.indexOf(";messageid=");
  if (i >= 0) {
    const rest = conversationId.slice(i + ";messageid=".length);
    if (rest !== "") return { channel: conversationId.slice(0, i), root: rest };
    return { channel: conversationId.slice(0, i), root: activityId };
  }
  return { channel: conversationId, root: activityId };
}

const serviceUrlKey = (channelId: string): string => "teams:service_url:" + channelId;

export class TeamsHandler implements Responder, EventContextBuilder {
  private readonly log: Logger;
  private readonly keys: KeySet;
  private readonly dedupe = new Dedupe(3600_000);
  private readonly tokens = new Map<string, TokenSource>();

  constructor(
    readonly deps: IntegrationDeps,
    private readonly opts: { openIdMetadataUrl?: string; loginUrl?: string; graphUrl?: string } = {},
  ) {
    this.log = deps.log ?? consoleLogger;
    this.keys = new KeySet({
      metadataUrl: opts.openIdMetadataUrl,
      fetch: deps.fetch,
      now: deps.now ? () => deps.now!().getTime() : undefined,
    });
  }

  private now(): Date {
    return this.deps.now ? this.deps.now() : new Date();
  }

  provider(): string {
    return PROVIDER;
  }

  private async allRecords(): Promise<Integration[]> {
    return (await this.deps.store.listIntegrations("")).filter((i) => equalFold(i.provider, INTEGRATION_PROVIDER));
  }

  private async appIds(): Promise<string[]> {
    try {
      return stringSet(...(await this.allRecords()).map((i) => cfg(i, "app_id")));
    } catch {
      return [];
    }
  }

  /** Whether a Bot Framework account ID ("28:<app id>") is one of our bots. */
  private async isOwnBot(fromId: string): Promise<boolean> {
    if (!fromId.startsWith("28:") || fromId.length === 3) return false;
    const id = fromId.slice(3);
    return (await this.appIds()).some((a) => a !== "" && equalFold(a, id));
  }

  async handleMessages(req: Request): Promise<WebhookOutcome> {
    let body: string;
    try {
      body = await readBody(req);
    } catch (e) {
      return { response: errorResponse(413, "payload_too_large", errMessage(e)) };
    }
    let act: Activity;
    try {
      act = JSON.parse(body) as Activity;
    } catch {
      return { response: errorResponse(400, "invalid_payload", "activity is not valid JSON") };
    }
    try {
      await this.keys.validate(req.headers.get("Authorization") ?? "", await this.appIds(), act.channelId ?? "", act.serviceUrl ?? "");
    } catch (e) {
      this.log.info("rejected bot framework request", { err: errMessage(e) });
      return { response: errorResponse(401, "unauthorized", "invalid Bot Connector token") };
    }
    const ok = new Response(null, { status: 200 });
    if (act.type !== "message" || this.dedupe.isSeen(act.id)) return { response: ok };
    return {
      response: ok,
      background: async () => {
        try {
          await this.handleMessage(act, body);
        } catch (e) {
          this.log.warn("handle teams activity", { id: act.id, err: errMessage(e) });
        }
      },
    };
  }

  /** The record of an activity: records whose team_id is the Graph team and whose app_id is the bot that received it. */
  private findIntegration(graphTeamId: string, recipientId: string) {
    const botId = recipientId.startsWith("28:") ? recipientId.slice(3) : recipientId;
    return intake(this.deps.store, INTEGRATION_PROVIDER, graphTeamId, (i) => botId === "" || equalFold(cfg(i, "app_id"), botId));
  }

  private async handleMessage(act: Activity, raw: string): Promise<void> {
    if (!act.from || !act.recipient || !act.conversation) return;
    if (act.from.id === act.recipient.id || (await this.isOwnBot(act.from.id))) return; // our own bots never start or continue work
    if (act.conversation.conversationType !== "channel") return; // private and group chats are not supported
    const recipient = act.recipient;
    const mentioned = (act.entities ?? []).some((e) => e.type === "mention" && e.mentioned?.id === recipient.id);
    let graphTeam = "";
    let tenant = act.conversation.tenantId ?? "";
    let channelId = "";
    let channelName = "";
    const cd = act.channelData;
    if (cd) {
      if (cd.team) graphTeam = cd.team.aadGroupId ?? "";
      if (cd.tenant?.id) tenant = cd.tenant.id;
      if (cd.channel) {
        channelId = cd.channel.id ?? "";
        channelName = cd.channel.name ?? "";
      }
    }
    const tr = threadRoot(act.conversation.id, act.id);
    if (channelId === "") channelId = tr.channel;
    const root = tr.root;
    if (act.serviceUrl) {
      try {
        await this.deps.store.kvSet(serviceUrlKey(channelId), act.serviceUrl);
      } catch (e) {
        this.log.debug("record service url", { err: errMessage(e) });
      }
    }
    const text = cleanText(act.text ?? "");
    const found = await this.findIntegration(graphTeam, recipient.id);
    if (found.error instanceof OfError && found.error.code === "integration_disabled") {
      this.log.info("activity for a disabled integration ignored", { team: graphTeam, err: found.error.message });
      return;
    }
    if (found.error || !found.integration) {
      if (mentioned) await this.replyActivity(await this.botRecord(recipient.id), act, NOT_CONNECTED_MESSAGE);
      return;
    }
    const in_ = found.integration;
    const actorId = act.from.aadObjectId || act.from.id;
    const isBot = act.from.id.startsWith("28:");
    const actor: Actor = { provider: PROVIDER, external_id: actorId, handle: act.from.name ?? "", is_bot: isBot };

    if (mentioned) {
      switch (text.replace(/^[ .!]+|[ .!]+$/g, "").toLowerCase()) {
        case "sign in":
        case "signin":
        case "login":
        case "link":
          await this.replyActivity(in_, act, this.signInText());
          return;
        case "sign out":
        case "signout":
        case "logout":
        case "unlink":
          if (this.deps.linker) await this.deps.linker.unlink(PROVIDER, actorId, "");
          await this.replyActivity(in_, act, "Your Microsoft account is no longer linked to Open Factory in this organization.");
          return;
        case "help":
        case "":
          await this.replyActivity(
            in_,
            act,
            "Mention me with a request to start factory work in this thread. Reply in the thread to continue it. Use `sign in` to link your Microsoft account, or `sign out` to unlink it.",
          );
          return;
      }
    }

    // Eligible users: the installation's organization and a linked active team member.
    let mem = null;
    try {
      mem = await member(this.deps.store, in_.team_id, PROVIDER, actorId);
    } catch (e) {
      this.log.warn("membership lookup", { err: errMessage(e) });
    }
    const want = cfg(in_, "tenant_id");
    if (want !== "" && !equalFold(want, tenant)) {
      if (mentioned) {
        await this.replyActivity(
          in_,
          act,
          "Only people in this team's Microsoft 365 organization with a linked Open Factory account can start or continue factory work.",
        );
      }
      return;
    }
    const isReply = root !== act.id;
    if (!mem) {
      if (mentioned) await this.replyActivity(in_, act, this.signInText());
      return;
    }
    actor.user_id = mem.id;
    const factoryId = in_.factory_id ?? "";
    const attrs: Record<string, unknown> = {
      [A.TeamID]: graphTeam,
      [A.ChannelID]: channelId,
      [A.UserID]: actorId,
      [A.TenantID]: tenant,
      [A.ActorIsBot]: isBot,
      [A.ActorIsSelf]: false, // own bots are dropped above
      [A.Linked]: true,
    };
    if (text !== "") attrs[A.Text] = text;
    // Teams gives the name of a channel only when it is not the General channel, and no e-mail address of the sender.
    if (channelName !== "") attrs[A.ChannelName] = channelName;
    if (act.from.name) attrs[A.UserName] = act.from.name;
    if (isReply) {
      attrs[A.ThreadReply] = true;
      attrs[A.AutoFollowUp] = await autoRespondToThreadReplies(this.deps.store, factoryId, DEFINITION_TYPE);
    }
    const src: SourceRef = { provider: PROVIDER, kind: "thread", channel: channelId, thread_id: root, workspace: integrationWorkspace(in_) };
    const payload = JSON.parse(raw) as unknown;
    const events: Event[] = [];
    const emit = (typ: string): void => {
      events.push({
        id: newId("evt"),
        team_id: in_.team_id,
        factory_id: factoryId,
        provider: PROVIDER,
        type: typ,
        delivery_id: act.id + ":" + typ,
        actor: { ...actor },
        source: src,
        attributes: { ...attrs },
        payload,
        text,
        received_at: this.now().toISOString(),
      });
    };
    if (mentioned) emit("app_mention");
    emit("message_posted");
    await this.deps.publish(events);
  }

  /** The link prompt. A thread reply is public, so it has no link URL. */
  private signInText(): string {
    return (
      "To use this factory, link your Microsoft account to an active member of the factory's Open Factory workspace, then send your request again. " +
      dashboardHint(this.deps.linker, PROVIDER)
    );
  }

  /** A record of the bot that received an activity, for its credentials only. */
  private async botRecord(recipientId: string): Promise<Integration | null> {
    if (!recipientId.startsWith("28:") || recipientId.length === 3) return null;
    const id = recipientId.slice(3);
    try {
      return (
        (await this.allRecords()).find((i) => equalFold(cfg(i, "app_id"), id) && (i.secret_ref ?? "") !== "" && (i.status || "active") === "active") ??
        null
      );
    } catch {
      return null;
    }
  }

  /** The app ID and the password of the bot of a record. */
  private async botCredentials(in_: Integration | null): Promise<{ appId: string; password: string }> {
    if (!in_ || cfg(in_, "app_id") === "" || !in_.secret_ref) {
      throw new Error("teams: bot credentials are not configured: set config app_id and secret_ref on the record");
    }
    return { appId: cfg(in_, "app_id"), password: await integrationSecret(this.deps.secrets, in_) };
  }

  private async tokenSource(in_: Integration | null): Promise<TokenSource> {
    const { appId, password } = await this.botCredentials(in_);
    const { tenant, fallback } = tokenTenants(in_);
    const key = `${appId}|${tenant}|${fallback}`;
    let ts = this.tokens.get(key);
    if (!ts || ts.appPassword !== password) {
      ts = new TokenSource({
        appId,
        appPassword: password,
        tenant,
        fallbackTenant: fallback,
        loginUrl: this.opts.loginUrl,
        fetch: this.deps.fetch,
        now: this.deps.now ? () => this.deps.now!().getTime() : undefined,
      });
      this.tokens.set(key, ts);
    }
    return ts;
  }

  /** Posts a message activity into a channel thread. */
  private async send(in_: Integration | null, serviceUrl: string, channelId: string, rootId: string, text: string): Promise<void> {
    const ts = await this.tokenSource(in_);
    const tok = await ts.getToken();
    const conv = rootId !== "" ? `${channelId};messageid=${rootId}` : channelId;
    const c = new ApiClient({
      baseUrl: serviceUrl.replace(/\/+$/, ""),
      fetch: this.deps.fetch,
      userAgent: "openfactory-teams",
      authorize: (h) => h.set("Authorization", "Bearer " + tok),
    });
    try {
      await c.send("POST", `/v3/conversations/${encodeURIComponent(conv)}/activities`, { type: "message", text, textFormat: "markdown" });
    } catch (e) {
      throw new Error(`teams: send activity: ${errMessage(e)}`, { cause: e });
    }
  }

  /**
   * Posts a message into a channel, or into a thread of it when rootId is
   * set, as the bot of the record. The built-in tool server uses it. When the
   * record selects channels (config channel_ids), only those are allowed.
   * Otherwise the bot may post only in a channel where it already got an
   * activity, because only then the service URL of the channel is known.
   */
  async postMessage(in_: Integration, channelIdIn: string, rootId: string, text: string): Promise<void> {
    const channelId = channelIdIn.trim();
    if (channelId === "") throw new OfError("invalid_request", "teams: a channel is required");
    let serviceUrl = (await this.deps.store.kvGet(serviceUrlKey(channelId))) ?? "";
    const allowed = cfg(in_, "channel_ids")
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s !== "");
    const selected = allowed.includes(channelId);
    if (allowed.length > 0 && !selected) {
      throw new OfError("not_authorized", `teams: channel ${channelId} is not one of the channels of this integration`);
    }
    if (serviceUrl === "" && !selected) {
      throw new OfError("not_authorized", `teams: the bot has not been used in channel ${channelId}`);
    }
    if (serviceUrl === "") serviceUrl = DEFAULT_SERVICE_URL;
    await this.send(in_, serviceUrl, channelId, rootId, text);
  }

  private async replyActivity(in_: Integration | null, act: Activity, text: string): Promise<void> {
    const { channel, root } = threadRoot(act.conversation?.id ?? "", act.id);
    try {
      await this.send(in_, act.serviceUrl ?? "", channel, root, text);
    } catch (e) {
      this.log.warn("teams reply", { err: errMessage(e) });
    }
  }

  /** Replies in the originating channel thread. */
  async reply(teamId: string, msg: OutboundMessage): Promise<void> {
    if (!msg.source.channel) throw new Error("teams: outbound message has no channel");
    const in_ = await selectTeam(this.deps.store, teamId, INTEGRATION_PROVIDER, msg.factory_id ?? "", msg.source.workspace ?? "");
    const v = await this.deps.store.kvGet(serviceUrlKey(msg.source.channel));
    await this.send(in_, v || DEFAULT_SERVICE_URL, msg.source.channel, msg.source.thread_id ?? "", formatOutbound(msg));
  }

  // ---------- Microsoft Graph ----------

  graphUrl(): string {
    return (this.opts.graphUrl || DEFAULT_GRAPH_URL).replace(/\/+$/, "");
  }

  /** A Graph client for a record: the bot with the client-credentials grant in the tenant of the installation. */
  async graphClient(in_: Integration | null, tenantIn: string): Promise<ApiClient> {
    const { appId, password } = await this.botCredentials(in_);
    let tenant = tenantIn;
    if (in_ && cfg(in_, "tenant_id").trim() !== "") tenant = cfg(in_, "tenant_id").trim();
    if (tenant === "") throw new Error("teams: Microsoft Graph needs the Microsoft 365 tenant: set config tenant_id on the integration record");
    const key = `${appId}|${tenant}|graph`;
    let ts = this.tokens.get(key);
    if (!ts || ts.appPassword !== password) {
      ts = new TokenSource({
        appId,
        appPassword: password,
        tenant,
        scope: GRAPH_SCOPE,
        loginUrl: this.opts.loginUrl,
        fetch: this.deps.fetch,
        now: this.deps.now ? () => this.deps.now!().getTime() : undefined,
      });
      this.tokens.set(key, ts);
    }
    const tok = await ts.getToken();
    return new ApiClient({
      baseUrl: this.graphUrl(),
      fetch: this.deps.fetch,
      userAgent: "openfactory-teams",
      authorize: (h) => h.set("Authorization", "Bearer " + tok),
    });
  }

  /** Reads a Graph list. Follows @odata.nextLink only on the Graph host. `each` returns false to stop. */
  async graphPages(c: ApiClient, path: string, each: (items: Record<string, unknown>[]) => boolean): Promise<void> {
    const base = new URL(this.graphUrl());
    let next = path;
    for (let page = 0; next !== "" && page < MAX_GRAPH_PAGES; page++) {
      const resp = await c.json<{ value?: Record<string, unknown>[]; "@odata.nextLink"?: string }>("GET", next);
      if (!each(resp?.value ?? [])) return;
      next = "";
      const link = resp?.["@odata.nextLink"];
      if (link) {
        try {
          const u = new URL(link);
          if (u.protocol === base.protocol && u.host === base.host) next = link;
        } catch {
          /* not followed */
        }
      }
    }
  }

  /**
   * The messages of the thread of the event, the first message and its
   * replies, in time order (thread.json), read with Graph as the bot of the
   * record. The bot needs ChannelMessage.Read.Group in the team and the record
   * needs tenant_id. A failed read is reported in the omitted list.
   */
  async eventContext(ev: Event): Promise<EventContextResult> {
    const channel = ev.source.channel ?? "";
    const root = ev.source.thread_id ?? "";
    if (channel === "" || root === "") return { files: [], omitted: [] };
    const graphTeam = typeof ev.attributes?.[A.TeamID] === "string" ? (ev.attributes[A.TeamID] as string) : "";
    const tenant = typeof ev.attributes?.[A.TenantID] === "string" ? (ev.attributes[A.TenantID] as string) : "";
    if (graphTeam === "") throw new Error("teams: the event has no team ID");
    const in_ = await selectTeam(this.deps.store, ev.team_id, INTEGRATION_PROVIDER, ev.factory_id ?? "", graphTeam);
    const omit = (reason: string): EventContextResult => ({
      files: [],
      omitted: [{ name: "context/thread.json", reason } satisfies EventContextOmitted],
    });
    let c: ApiClient;
    try {
      c = await this.graphClient(in_, tenant);
    } catch (e) {
      return omit(errMessage(e));
    }
    const perm = "ChannelMessage.Read.Group";
    const base = `/teams/${encodeURIComponent(graphTeam)}/channels/${encodeURIComponent(channel)}/messages/${encodeURIComponent(root)}`;
    let first: Record<string, unknown> | undefined;
    try {
      first = await c.json("GET", base);
    } catch (e) {
      return omit(describeGraphError("read the thread", perm, e).message);
    }
    const msgs: Record<string, unknown>[] = first ? [first] : [];
    try {
      await this.graphPages(c, `${base}/replies?$top=${GRAPH_PAGE_SIZE}`, (items) => {
        msgs.push(...items);
        return msgs.length < MAX_THREAD_MESSAGES * 2;
      });
    } catch (e) {
      return omit(describeGraphError("read the replies", perm, e).message);
    }
    const entries: ThreadEntry[] = [];
    for (const m of msgs) {
      const e = threadEntry(m);
      if (e) entries.push(e);
    }
    // Graph does not promise an order: the oldest message comes first.
    entries.sort((a, b) => ((a.time ?? "") < (b.time ?? "") ? -1 : (a.time ?? "") > (b.time ?? "") ? 1 : 0));
    const { data, left } = fitThread(entries);
    const omitted: EventContextOmitted[] = [];
    if (left > 0) {
      omitted.push({
        name: "context/thread.json",
        reason: `${left} of ${entries.length} messages were left out: the thread is longer than the limit of ${MAX_THREAD_MESSAGES} messages or ${EVENT_CONTEXT_MAX_PAYLOAD_BYTES >> 10} KiB`,
      });
    }
    return { files: [{ path: "thread.json", data }], omitted };
  }
}

/** A hint for the errors that a missing permission or a wrong tenant gives. */
export function describeGraphError(what: string, permission: string, err: unknown): Error {
  if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
    return new Error(`Microsoft Graph refused the request to ${what}: the bot needs the permission ${permission} in the team: ${err.message}`, { cause: err });
  }
  return err instanceof Error ? err : new Error(String(err));
}

/**
 * The tenant that Bot Connector tokens come from, and an optional fallback:
 * app_type SingleTenant uses tenant_id (required); MultiTenant uses
 * botframework.com; otherwise botframework.com with tenant_id as the fallback.
 */
export function tokenTenants(in_: Integration | null): { tenant: string; fallback: string } {
  const recTenant = in_ ? cfg(in_, "tenant_id").trim() : "";
  const appType = in_ ? cfg(in_, "app_type").trim().toLowerCase() : "";
  switch (appType) {
    case "singletenant":
    case "single_tenant":
    case "single-tenant":
      if (recTenant === "") throw new Error("teams: app_type SingleTenant requires config tenant_id");
      return { tenant: recTenant, fallback: "" };
    case "multitenant":
    case "multi_tenant":
    case "multi-tenant":
      return { tenant: DEFAULT_TOKEN_TENANT, fallback: "" };
    case "":
      break;
    default:
      throw new Error(`teams: unknown app_type "${in_ ? cfg(in_, "app_type") : ""}" (want SingleTenant or MultiTenant)`);
  }
  return { tenant: DEFAULT_TOKEN_TENANT, fallback: recTenant };
}
