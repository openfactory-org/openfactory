// Teams thread context entries (port of the pure parts of teams/context.go).
import { EVENT_CONTEXT_MAX_PAYLOAD_BYTES } from "../types";

export const MAX_THREAD_MESSAGES = 200;
export const GRAPH_PAGE_SIZE = 50;

export interface ThreadAttachment {
  name?: string;
  content_type?: string;
}

export interface ThreadEntry {
  id: string;
  time?: string;
  user?: string;
  user_id?: string;
  bot?: boolean;
  text: string;
  attachments?: ThreadAttachment[];
}

export const str = (m: Record<string, unknown> | null | undefined, key: string): string => {
  const s = m?.[key];
  return typeof s === "string" ? s : "";
};

export const obj = (m: Record<string, unknown> | null | undefined, key: string): Record<string, unknown> | null => {
  const o = m?.[key];
  return typeof o === "object" && o !== null && !Array.isArray(o) ? (o as Record<string, unknown>) : null;
};

const AT_TAG = /<at[^>]*>[\s\S]*?<\/at>/gi;
const HTML_TAG = /<[^>]+>/g;

/** Removes <at> mention tags and HTML markup from a Teams message. */
export function cleanText(s: string): string {
  let t = s.replace(AT_TAG, "");
  t = t.replaceAll("<br>", "\n").replaceAll("<br/>", "\n").replaceAll("</p>", "\n");
  t = t.replace(HTML_TAG, "");
  t = t
    .replaceAll("&nbsp;", " ")
    .replaceAll("&amp;", "&")
    .replaceAll("&lt;", "<")
    .replaceAll("&gt;", ">")
    .replaceAll("&quot;", '"')
    .replaceAll("&#39;", "'");
  return t.trim();
}

/** A Graph chatMessage as an entry. System messages and deleted messages give null. */
export function threadEntry(m: Record<string, unknown>): ThreadEntry | null {
  const t = str(m, "messageType");
  if (t !== "" && t !== "message") return null;
  if (str(m, "deletedDateTime") !== "") return null;
  const e: ThreadEntry = { id: str(m, "id"), text: "" };
  const time = str(m, "createdDateTime");
  if (time !== "") e.time = time;
  const from = obj(m, "from");
  if (from) {
    const u = obj(from, "user");
    const a = obj(from, "application");
    if (u) {
      e.user = str(u, "displayName");
      e.user_id = str(u, "id");
    } else if (a) {
      e.user = str(a, "displayName");
      e.bot = true;
    }
  }
  const b = obj(m, "body");
  if (b) {
    e.text = str(b, "content");
    if (str(b, "contentType").toLowerCase() === "html") e.text = cleanText(e.text);
  }
  if (Array.isArray(m["attachments"])) {
    for (const a of m["attachments"]) {
      if (typeof a === "object" && a !== null && str(a as Record<string, unknown>, "name") !== "") {
        const am = a as Record<string, unknown>;
        const att: ThreadAttachment = { name: str(am, "name") };
        if (str(am, "contentType") !== "") att.content_type = str(am, "contentType");
        (e.attachments ??= []).push(att);
      }
    }
  }
  return e.id === "" ? null : e;
}

/**
 * Writes the entries as JSON within the limits. When the thread is too long,
 * the first message stays, the oldest replies go first, and `left` is the
 * number of messages dropped.
 */
export function fitThread(entries: ThreadEntry[]): { data: Uint8Array; left: number } {
  let keep = entries;
  if (keep.length > MAX_THREAD_MESSAGES) keep = [keep[0]!, ...keep.slice(keep.length - MAX_THREAD_MESSAGES + 1)];
  const enc = new TextEncoder();
  for (;;) {
    const b = enc.encode(JSON.stringify(keep, null, 2));
    if (b.length <= EVENT_CONTEXT_MAX_PAYLOAD_BYTES || keep.length <= 1)
      return { data: b, left: entries.length - keep.length };
    // Drop the oldest reply, in steps that grow with the size.
    let drop = 1 + Math.floor(keep.length / 20);
    if (drop > keep.length - 1) drop = keep.length - 1;
    keep = [keep[0]!, ...keep.slice(1 + drop)];
  }
}
