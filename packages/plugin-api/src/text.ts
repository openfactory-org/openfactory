// Text helpers shared by forge plugins. Port of internal/forge/forgeutil/text.go.

import type { OutboundMessage } from "./types";

const FENCED_CODE = /(```|~~~)[\s\S]*?(```|~~~|$)/g;
const INLINE_CODE = /`[^`\n]*`/g;
const QUOTE_LINE = /^\s*>.*$/gm;
const MENTION = /(^|[^A-Za-z0-9_.\-/@`])@([A-Za-z0-9](?:[A-Za-z0-9_.\-]*[A-Za-z0-9_])?(?:\/[A-Za-z0-9_.\-]+)?(?:\[bot\])?)/g;

/** Replaces fenced code, inline code and quoted lines with a space. */
export function stripCode(text: string): string {
  return text.replace(FENCED_CODE, " ").replace(INLINE_CODE, " ").replace(QUOTE_LINE, " ");
}

/** The handles mentioned in text, once each (case-insensitive), without code and quotes. */
export function extractMentions(text: string): string[] {
  const stripped = stripCode(text);
  const out: string[] = [];
  const seen = new Set<string>();
  for (const m of stripped.matchAll(MENTION)) {
    const h = (m[2] ?? "").replace(/\.+$/, "");
    const k = h.toLowerCase();
    if (h === "" || seen.has(k)) continue;
    seen.add(k);
    out.push(h);
  }
  return out;
}

export function hasMention(text: string, handle: string): boolean {
  const h = handle.replace(/^@/, "");
  if (h === "") return false;
  return extractMentions(text).some(
    (m) => m.toLowerCase() === h.toLowerCase() || m.replace(/\[bot\]$/, "").toLowerCase() === h.toLowerCase(),
  );
}

/** "acme/platform/api" gives owner "acme/platform" and name "api". */
export function splitRepo(full: string): { owner: string; name: string } {
  const f = full.replace(/^\/+|\/+$/g, "");
  const i = f.lastIndexOf("/");
  if (i < 0) return { owner: "", name: f };
  return { owner: f.slice(0, i), name: f.slice(i + 1) };
}

/** Cuts s to n characters (code points), ending with an ellipsis. */
export function truncate(s: string, n: number): string {
  const r = Array.from(s);
  if (r.length <= n) return s;
  if (n <= 1) return r.slice(0, n).join("");
  return r.slice(0, n - 1).join("") + "…";
}

/** The non-empty strings of the input, once each, in order. */
export function stringSet(...items: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const s of items) {
    if (s === "" || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
  }
  return out;
}

/** The items of `after` that are not in `before`. */
export function difference(after: string[], before: string[]): string[] {
  const b = new Set(before);
  return after.filter((s) => !b.has(s));
}

/** Markdown for an outbound message in a forge comment. */
export function formatOutbound(msg: OutboundMessage): string {
  let out = "";
  switch (msg.kind) {
    case "question":
      out += "**Question from the factory**\n\n";
      break;
    case "approval_request":
      out += "**Approval needed**\n\n";
      break;
    case "error":
      out += "**The factory hit an error**\n\n";
      break;
    default:
  }
  out += msg.text.trim();
  if (msg.options && msg.options.length > 0) {
    out += "\n\nReply with one of:\n";
    for (const o of msg.options) out += `- \`${o}\`\n`;
  }
  return out.replace(/\n+$/, "");
}
