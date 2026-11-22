// Slack responder: outbound messages as Block Kit in the originating thread,
// and the thread event context (port of slack/responder.go and context.go).
import type { Event, Integration, OutboundMessage, SourceRef } from "@openfactory/core";
import { selectTeam } from "../select";
import type {
  EventContextBuilder,
  EventContextFile,
  EventContextOmitted,
  EventContextResult,
  IntegrationDeps,
} from "../types";
import { truncate, type Responder } from "@openfactory/core";
import { errMessage, integrationSecret } from "../util";
import { SlackClient, type Block, type SlackMessage } from "./client";

export const PROVIDER = "slack";
/** Prefixes the action_id of option buttons. */
export const ACTION_PREFIX = "of_option_";

const MAX_SECTION_TEXT = 3000;
const MAX_BUTTON_TEXT = 75;
const MAX_BUTTONS = 25;

const MAX_THREAD_MESSAGES = 200;
const MAX_THREAD_FILES = 10;
const MAX_THREAD_FILE_SIZE = 1 << 20;
/** The only host that gets the bot token for a file download. */
const FILE_HOST = "files.slack.com";

export interface ActionValue {
  w?: string;
  i?: string;
  o: string;
}

const MD_LINK = /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g;
const MD_BOLD = /\*\*([^*\n]+)\*\*/g;
const MD_HEADING = /^#{1,6}\s+(.+)$/gm;
const MD_STRIKE = /~~([^~\n]+)~~/g;

const escapeText = (s: string): string => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

/** Converts common Markdown to Slack mrkdwn (links, bold, headings, strikethrough). */
export function mrkdwn(md: string): string {
  let s = escapeText(md);
  s = s.replace(MD_LINK, "<$2|$1>");
  s = s.replace(MD_HEADING, "*$1*");
  s = s.replace(MD_BOLD, "*$1*");
  s = s.replace(MD_STRIKE, "~$1~");
  return s;
}

function plain(md: string): string {
  let s = md.replace(MD_LINK, "$1 ($2)");
  s = s.replace(MD_BOLD, "$1");
  return s.replace(MD_HEADING, "$1");
}

function chunks(s: string, n: number): string[] {
  let r = Array.from(s);
  if (r.length === 0) return [" "];
  const out: string[] = [];
  while (r.length > n) {
    let cut = n;
    for (let i = n; i > n / 2; i--) {
      if (r[i] === "\n") {
        cut = i;
        break;
      }
    }
    out.push(r.slice(0, cut).join(""));
    r = r.slice(cut);
  }
  out.push(r.join(""));
  return out;
}

/** Renders an outbound message as a threaded Block Kit message with option buttons. */
export function buildMessage(msg: OutboundMessage): SlackMessage {
  let text = mrkdwn(msg.text);
  switch (msg.kind) {
    case "approval_request":
      text = ":raised_hand: *Approval needed*\n" + text;
      break;
    case "question":
      text = ":question: *Question*\n" + text;
      break;
    case "error":
      text = ":warning: " + text;
  }
  const blocks: Block[] = chunks(text, MAX_SECTION_TEXT).map((c) => ({
    type: "section",
    text: { type: "mrkdwn", text: c },
  }));
  const options = msg.options ?? [];
  if (options.length > 0) {
    const elems = options.slice(0, MAX_BUTTONS).map((opt, i) => {
      const val: ActionValue = { o: opt };
      if (msg.work_item_id) val.w = msg.work_item_id;
      if (msg.inbox_id) val.i = msg.inbox_id;
      const btn: Record<string, unknown> = {
        type: "button",
        action_id: `${ACTION_PREFIX}${i}`,
        text: { type: "plain_text", text: truncate(opt, MAX_BUTTON_TEXT), emoji: true },
        value: JSON.stringify(val),
      };
      if (i === 0 && msg.kind === "approval_request") btn["style"] = "primary";
      return btn;
    });
    blocks.push({ type: "actions", block_id: "of_options", elements: elems });
  }
  const m: SlackMessage = {
    channel: msg.source.channel ?? "",
    text: truncate(plain(msg.text), 3000),
    blocks,
  };
  if (msg.source.thread_id) m.thread_ts = msg.source.thread_id;
  return m;
}

interface ThreadFile {
  id: string;
  name: string;
  mimetype?: string;
  size?: number;
  url_private_download?: string;
}

interface ThreadMessage {
  ts: string;
  thread_ts?: string;
  user?: string;
  bot_id?: string;
  text?: string;
  files?: ThreadFile[];
}

export interface ThreadEntry {
  ts: string;
  user?: string;
  bot?: boolean;
  text: string;
  files?: string[];
}

function fileName(f: ThreadFile): string {
  const parts = (f.name ?? "").replaceAll("\\", "/").split("/");
  let n = parts[parts.length - 1] ?? "";
  if (n === "." || n === ".." || n === "") n = "file";
  return f.id + "-" + n;
}

export class SlackResponder implements Responder, EventContextBuilder {
  constructor(
    readonly deps: IntegrationDeps,
    private readonly apiBaseUrl = "",
  ) {}

  provider(): string {
    return PROVIDER;
  }

  /** The record for a reply: the workspace of the source, the record of the factory first. factoryId "" falls back to factoryOf of the work item. */
  private async integration(
    teamId: string,
    factoryId: string,
    workItemId: string,
    src: SourceRef,
  ): Promise<Integration> {
    let fid = factoryId;
    if (fid === "" && workItemId !== "" && this.deps.work?.factoryOf) {
      try {
        fid = await this.deps.work.factoryOf(workItemId);
      } catch {
        /* the team record serves */
      }
    }
    return selectTeam(this.deps.store, teamId, PROVIDER, fid, src.workspace ?? "");
  }

  async reply(teamId: string, msg: OutboundMessage): Promise<void> {
    if (!msg.source.channel) throw new Error("slack: outbound message has no channel");
    const in_ = await this.integration(teamId, msg.factory_id ?? "", msg.work_item_id, msg.source);
    const tok = await integrationSecret(this.deps.secrets, in_);
    await new SlackClient(this.apiBaseUrl, tok, this.deps.fetch).postMessage(buildMessage(msg));
  }

  /**
   * The earlier messages of the thread (thread.json) and the files of the
   * thread that the app can read (files/<id>-<name>). The app needs the scopes
   * channels:history, groups:history, im:history and files:read.
   */
  async eventContext(ev: Event): Promise<EventContextResult> {
    const channel = ev.source.channel ?? "";
    const thread = ev.source.thread_id ?? "";
    if (channel === "" || thread === "") return { files: [], omitted: [] };
    const in_ = await this.integration(ev.team_id, ev.factory_id ?? "", "", ev.source);
    let tok: string;
    try {
      tok = await integrationSecret(this.deps.secrets, in_);
    } catch (e) {
      throw new Error(`slack: integration credential: ${errMessage(e)}`, { cause: e });
    }
    const c = new SlackClient(this.apiBaseUrl, tok, this.deps.fetch);
    let out: { messages?: ThreadMessage[] };
    try {
      out = await c.call(
        "conversations.replies",
        new URLSearchParams({ channel, ts: thread, limit: String(MAX_THREAD_MESSAGES) }),
      );
    } catch (e) {
      return { files: [], omitted: [{ name: "context/thread.json", reason: errMessage(e) }] };
    }
    const files: EventContextFile[] = [];
    const omitted: EventContextOmitted[] = [];
    const entries: ThreadEntry[] = [];
    let nFiles = 0;
    for (const m of out.messages ?? []) {
      const e: ThreadEntry = { ts: m.ts, text: m.text ?? "" };
      if (m.user) e.user = m.user;
      if (m.bot_id) e.bot = true;
      for (const f of m.files ?? []) {
        const name = fileName(f);
        (e.files ??= []).push("context/files/" + name);
        if (nFiles >= MAX_THREAD_FILES) {
          omitted.push({ name: f.name, reason: "too many files" });
          continue;
        }
        if ((f.size ?? 0) > MAX_THREAD_FILE_SIZE) {
          omitted.push({ name: f.name, reason: `larger than ${MAX_THREAD_FILE_SIZE >> 10} KiB` });
          continue;
        }
        try {
          const data = await this.download(tok, f.url_private_download ?? "");
          nFiles++;
          files.push({ path: "files/" + name, data });
        } catch (err) {
          omitted.push({ name: f.name, reason: errMessage(err) });
        }
      }
      entries.push(e);
    }
    const data = new TextEncoder().encode(JSON.stringify(entries, null, 2));
    return { files: [{ path: "thread.json", data }, ...files], omitted };
  }

  /** Reads a private file. The token goes only to the file host. */
  private async download(token: string, raw: string): Promise<Uint8Array> {
    let ok: boolean;
    try {
      const u = new URL(raw);
      ok = u.protocol === "https:" && u.host === FILE_HOST;
    } catch {
      ok = false;
    }
    if (!ok && !(this.apiBaseUrl !== "" && raw.startsWith(this.apiBaseUrl.replace(/\/+$/, "") + "/"))) {
      throw new Error(`the file URL is not on ${FILE_HOST}`);
    }
    const f = this.deps.fetch ?? fetch;
    const resp = await f(raw, { headers: { Authorization: "Bearer " + token } });
    if (resp.status !== 200) throw new Error(`download returned ${resp.status}`);
    const data = new Uint8Array(await resp.arrayBuffer());
    if (data.length > MAX_THREAD_FILE_SIZE) throw new Error(`larger than ${MAX_THREAD_FILE_SIZE >> 10} KiB`);
    return data;
  }
}
