// Text and mention helpers shared by the forge, integration and plugin code.
// Port of internal/forge/forgeutil/text.go.
import type { OutboundMessage } from "./workitem";

const FENCED_CODE = /(```|~~~)[\s\S]*?(```|~~~|$)/g;
const INLINE_CODE = /`[^`\n]*`/g;
const QUOTE_LINE = /^\s*>.*$/gm;
// GitHub and GitLab handles: @login, @org/team, @app-name, @bot[bot]
const MENTION =
  /(^|[^A-Za-z0-9_./@`-])@([A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9_])?(?:\/[A-Za-z0-9_.-]+)?(?:\[bot\])?)/g;

/** Replaces fenced code blocks, inline code spans and quoted lines with a space. */
export function stripCode(text: string): string {
  return text.replace(FENCED_CODE, " ").replace(INLINE_CODE, " ").replace(QUOTE_LINE, " ");
}

/** The distinct @handles of text (outside code and quotes), in order, without "@". Case-insensitive. */
export function extractMentions(text: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const m of stripCode(text).matchAll(MENTION)) {
    const h = (m[2] ?? "").replace(/\.+$/, "");
    const k = h.toLowerCase();
    if (h === "" || seen.has(k)) continue;
    seen.add(k);
    out.push(h);
  }
  return out;
}

/** Whether handle (case-insensitive, optional "@") is mentioned outside code. "x[bot]" matches "x". */
export function hasMention(text: string, handle: string): boolean {
  const h = handle.replace(/^@/, "").toLowerCase();
  if (h === "") return false;
  return extractMentions(text).some((m) => {
    const l = m.toLowerCase();
    return l === h || l.replace(/\[bot\]$/, "") === h;
  });
}

/**
 * Splits "owner/name" at the last slash after it trims slashes at both ends.
 * The owner may hold slashes: "acme/platform/api" gives ["acme/platform", "api"].
 */
export function splitRepo(full: string): [owner: string, name: string] {
  const f = full.replace(/^\/+|\/+$/g, "");
  const i = f.lastIndexOf("/");
  if (i < 0) return ["", f];
  return [f.slice(0, i), f.slice(i + 1)];
}

/** Shortens s to at most n code points, with an ellipsis when cut. */
export function truncate(s: string, n: number): string {
  const r = Array.from(s);
  if (r.length <= n) return s;
  if (n <= 1) return r.slice(0, Math.max(n, 0)).join("");
  return r.slice(0, n - 1).join("") + "…";
}

/** Cuts b to at most max bytes at the start of a UTF-8 character. */
export function cutBytes(b: Uint8Array, max: number): Uint8Array {
  if (b.length <= max) return b;
  let n = Math.max(max, 0);
  while (n > 0 && (b[n]! & 0xc0) === 0x80) n--;
  return b.subarray(0, n);
}

/** The first n UTF-8 bytes of s, without cutting a character in half. */
export function clipBytes(s: string, n: number): string {
  const b = new TextEncoder().encode(s);
  if (b.length <= n) return s;
  return new TextDecoder().decode(cutBytes(b, n));
}

/** Renders an outbound message as markdown for comment-based destinations. */
export function formatOutbound(msg: Pick<OutboundMessage, "kind" | "text" | "options">): string {
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
    for (const o of msg.options) out += "- `" + o + "`\n";
  }
  return out.replace(/\n+$/, "");
}

/** The distinct non-empty values, in order. */
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

/** The items of after that are not in before. */
export function difference(after: string[], before: string[]): string[] {
  const b = new Set(before);
  return after.filter((s) => !b.has(s));
}
