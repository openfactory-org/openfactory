// Slack Web API client (port of slack/client.go).
import { ApiClient } from "../http";
import type { Fetch } from "../types";

export const DEFAULT_API_BASE_URL = "https://slack.com/api";

/** A Slack Web API error ({"ok":false,"error":"..."}). */
export class SlackApiError extends Error {
  override readonly name = "SlackApiError";
  constructor(
    readonly method: string,
    readonly code: string,
  ) {
    super(`slack ${method}: ${code}`);
  }
}

export type Block = Record<string, unknown>;

export interface SlackMessage {
  channel: string;
  text: string;
  thread_ts?: string;
  blocks?: Block[];
  /** chat.postEphemeral only. */
  user?: string;
  mrkdwn?: boolean;
}

export interface UserInfo {
  id: string;
  name: string;
  is_bot?: boolean;
  deleted?: boolean;
  team_id?: string;
  is_restricted?: boolean;
  is_ultra_restricted?: boolean;
  is_stranger?: boolean;
  profile?: { email?: string; display_name?: string };
}

export interface HistoryMessage {
  ts: string;
  thread_ts?: string;
  user?: string;
  bot_id?: string;
  text?: string;
  blocks?: unknown[];
  attachments?: unknown[];
}

export class SlackClient {
  private readonly api: ApiClient;
  constructor(baseUrl: string, token: string, fetchFn?: Fetch) {
    this.api = new ApiClient({
      baseUrl: baseUrl || DEFAULT_API_BASE_URL,
      fetch: fetchFn,
      userAgent: "openfactory-slack",
      authorize: (h) => h.set("Authorization", "Bearer " + token),
    });
  }

  /** Calls a Web API method: a JSON body, or query values (URLSearchParams) for read methods. */
  async call<T = Record<string, unknown>>(method: string, input: object | URLSearchParams): Promise<T> {
    const raw =
      input instanceof URLSearchParams
        ? await this.api.json<Record<string, unknown>>("GET", `${method}?${input.toString()}`)
        : await this.api.json<Record<string, unknown>>("POST", method, input);
    if (!raw || typeof raw !== "object") throw new Error(`slack ${method}: decode: empty answer`);
    if (raw["ok"] !== true) throw new SlackApiError(method, String(raw["error"] ?? ""));
    return raw as T;
  }

  /** Posts a message and returns its ts. */
  async postMessage(m: SlackMessage): Promise<string> {
    const { user: _user, ...rest } = m;
    const out = await this.call<{ ts?: string }>("chat.postMessage", rest);
    return out.ts ?? "";
  }

  /** Shows a message only to m.user. */
  async postEphemeral(m: SlackMessage): Promise<void> {
    await this.call("chat.postEphemeral", m);
  }

  /** Adds a reaction. already_reacted is not an error. */
  async addReaction(channel: string, ts: string, name: string): Promise<void> {
    try {
      await this.call("reactions.add", { channel, timestamp: ts, name });
    } catch (e) {
      if (e instanceof SlackApiError && e.code === "already_reacted") return;
      throw e;
    }
  }

  async userInfo(userId: string): Promise<UserInfo> {
    const out = await this.call<{ user: UserInfo }>("users.info", new URLSearchParams({ user: userId }));
    return out.user;
  }

  async channelName(channel: string): Promise<string> {
    const out = await this.call<{ channel?: { name?: string } }>(
      "conversations.info",
      new URLSearchParams({ channel }),
    );
    return out.channel?.name ?? "";
  }

  /** One message by channel and ts (root or thread reply). */
  async getMessage(channel: string, ts: string): Promise<HistoryMessage> {
    try {
      const out = await this.call<{ messages?: HistoryMessage[] }>(
        "conversations.history",
        new URLSearchParams({ channel, latest: ts, oldest: ts, inclusive: "true", limit: "1" }),
      );
      if (out.messages?.[0]?.ts === ts) return out.messages[0];
    } catch {
      /* thread replies are not in history: ask conversations.replies */
    }
    const out = await this.call<{ messages?: HistoryMessage[] }>(
      "conversations.replies",
      new URLSearchParams({ channel, ts, latest: ts, oldest: ts, inclusive: "true", limit: "1" }),
    );
    const m = (out.messages ?? []).find((x) => x.ts === ts);
    if (!m) throw new Error(`slack: message ${ts} not found in ${channel}`);
    return m;
  }
}

/** Posts to an interaction response_url. */
export async function respondUrl(fetchFn: Fetch | undefined, responseUrl: string, body: unknown): Promise<void> {
  await new ApiClient({ fetch: fetchFn, userAgent: "openfactory-slack" }).send("POST", responseUrl, body as object);
}

/** Visible text of Block Kit blocks and legacy attachments. */
export function textOfBlocks(blocks: unknown[] | undefined, attachments: unknown[] | undefined): string {
  const parts: string[] = [];
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) {
      for (const c of v) walk(c);
    } else if (typeof v === "object" && v !== null) {
      const t = v as Record<string, unknown>;
      if (typeof t["text"] === "string" && t["text"] !== "") parts.push(t["text"]);
      for (const [k, child] of Object.entries(t)) {
        if (k === "text" && typeof child === "string") continue;
        walk(child);
      }
    }
  };
  for (const v of [...(blocks ?? []), ...(attachments ?? [])]) {
    if (typeof v === "object" && v !== null && !Array.isArray(v)) {
      const m = v as Record<string, unknown>;
      for (const k of ["pretext", "title", "fallback"]) {
        if (typeof m[k] === "string" && m[k] !== "") parts.push(m[k]);
      }
    }
    walk(v);
  }
  return parts.join("\n");
}
