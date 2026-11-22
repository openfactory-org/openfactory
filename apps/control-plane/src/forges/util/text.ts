// Text helpers shared by the forge and integration code. Port of
// internal/forge/forgeutil/text.go.
import type { OutboundMessage } from "@openfactory/core";

const fencedCode = /(```|~~~)[\s\S]*?(```|~~~|$)/g;
const inlineCode = /`[^`\n]*`/g;
const quoteLine = /^\s*>.*$/gm;
// GitHub and GitLab handles: @login, @org/team, @app-name, @bot[bot]
const mentionRe = /(^|[^A-Za-z0-9_.\-/@`])@([A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9_])?(?:\/[A-Za-z0-9_.-]+)?(?:\[bot\])?)/g;

/** Removes fenced code blocks, inline code spans and quoted lines. */
export function stripCode(text: string): string {
  return text.replace(fencedCode, " ").replace(inlineCode, " ").replace(quoteLine, " ");
}

/** The distinct @handles of text (outside code), in order, without "@". */
export function extractMentions(text: string): string[] {
  const t = stripCode(text);
  const out: string[] = [];
  const seen = new Set<string>();
  for (const m of t.matchAll(mentionRe)) {
    const h = m[2]!.replace(/\.+$/, "");
    const k = h.toLowerCase();
    if (h === "" || seen.has(k)) continue;
    seen.add(k);
    out.push(h);
  }
  return out;
}

/** Whether handle (case-insensitive, optional "@") is mentioned outside code. */
export function hasMention(text: string, handle: string): boolean {
  const h = handle.replace(/^@/, "").toLowerCase();
  if (h === "") return false;
  return extractMentions(text).some((m) => {
    const l = m.toLowerCase();
    return l === h || l.replace(/\[bot\]$/, "") === h;
  });
}

/** Splits "owner/name" at the last slash (owner may hold slashes). */
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

/** Renders an outbound message as markdown for comment-based destinations. */
export function formatOutbound(msg: Pick<OutboundMessage, "kind" | "text" | "options">): string {
  let b = "";
  switch (msg.kind) {
    case "question":
      b += "**Question from the factory**\n\n";
      break;
    case "approval_request":
      b += "**Approval needed**\n\n";
      break;
    case "error":
      b += "**The factory hit an error**\n\n";
      break;
  }
  b += msg.text.trim();
  if (msg.options && msg.options.length > 0) {
    b += "\n\nReply with one of:\n";
    for (const o of msg.options) b += "- `" + o + "`\n";
  }
  return b.replace(/\n+$/, "");
}

/** The distinct non-empty values, in order. */
export function stringSet(...vals: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const s of vals) {
    if (s === "" || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
  }
  return out;
}

/** The elements of after that are not in before. */
export function diff(after: string[], before: string[]): string[] {
  const b = new Set(before);
  return after.filter((s) => !b.has(s));
}
