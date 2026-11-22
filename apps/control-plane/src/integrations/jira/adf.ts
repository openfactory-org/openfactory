// Atlassian Document Format (port of jira/adf.go).

export interface AdfMark {
  type: string;
  attrs?: Record<string, unknown>;
}

export interface AdfNode {
  type: string;
  version?: number;
  text?: string;
  attrs?: Record<string, unknown>;
  marks?: AdfMark[];
  content?: AdfNode[];
}

const INLINE_RE = /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)|\*\*([^*]+)\*\*|`([^`]+)`/g;
const HEADING_RE = /^(#{1,6})\s+(.*)$/;
const BULLET_RE = /^\s*[-*]\s+(.*)$/;
const ORDERED_RE = /^\s*\d+[.)]\s+(.*)$/;

function inline(s: string): AdfNode[] {
  const out: AdfNode[] = [];
  let last = 0;
  for (const m of s.matchAll(INLINE_RE)) {
    const start = m.index ?? 0;
    if (start > last) out.push({ type: "text", text: s.slice(last, start) });
    if (m[1] !== undefined) {
      out.push({ type: "text", text: m[1], marks: [{ type: "link", attrs: { href: m[2] } }] });
    } else if (m[3] !== undefined) {
      out.push({ type: "text", text: m[3], marks: [{ type: "strong" }] });
    } else if (m[4] !== undefined) {
      out.push({ type: "text", text: m[4], marks: [{ type: "code" }] });
    }
    last = start + m[0].length;
  }
  if (last < s.length) out.push({ type: "text", text: s.slice(last) });
  return out;
}

/** Converts common Markdown (paragraphs, headings, lists, fenced code, links, bold, inline code) into an ADF document. */
export function markdownToAdf(md: string): AdfNode {
  const doc: AdfNode = { type: "doc", version: 1, content: [] };
  const content = doc.content!;
  const lines = md.replaceAll("\r\n", "\n").split("\n");
  let para: string[] = [];
  let list: AdfNode | null = null;
  const flushPara = (): void => {
    if (para.length === 0) return;
    const p: AdfNode = { type: "paragraph", content: [] };
    para.forEach((l, i) => {
      if (i > 0) p.content!.push({ type: "hardBreak" });
      p.content!.push(...inline(l));
    });
    content.push(p);
    para = [];
  };
  const flushList = (): void => {
    if (list) {
      content.push(list);
      list = null;
    }
  };
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i]!;
    const trimmed = l.trim();
    if (trimmed.startsWith("```")) {
      flushPara();
      flushList();
      const lang = trimmed.slice(3).trim();
      const code: string[] = [];
      for (i++; i < lines.length && !lines[i]!.trim().startsWith("```"); i++) code.push(lines[i]!);
      const cb: AdfNode = { type: "codeBlock", content: [{ type: "text", text: code.join("\n") }] };
      if (lang !== "") cb.attrs = { language: lang };
      if (code.length === 0) delete cb.content;
      content.push(cb);
      continue;
    }
    if (trimmed === "") {
      flushPara();
      flushList();
      continue;
    }
    const h = HEADING_RE.exec(trimmed);
    if (h) {
      flushPara();
      flushList();
      content.push({ type: "heading", attrs: { level: h[1]!.length }, content: inline(h[2]!) });
      continue;
    }
    const isBullet = BULLET_RE.test(l);
    if (isBullet || ORDERED_RE.test(l)) {
      flushPara();
      const typ = isBullet ? "bulletList" : "orderedList";
      const re = isBullet ? BULLET_RE : ORDERED_RE;
      if (list && (list as AdfNode).type !== typ) flushList();
      list ??= { type: typ, content: [] };
      const item = re.exec(l)![1]!;
      (list as AdfNode).content!.push({ type: "listItem", content: [{ type: "paragraph", content: inline(item) }] });
      continue;
    }
    flushList();
    para.push(l);
  }
  flushPara();
  flushList();
  if (content.length === 0) doc.content = [{ type: "paragraph" }];
  return doc;
}

function asNode(raw: unknown): AdfNode | null {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return null;
  return raw as AdfNode;
}

/** Plain text of an ADF document or a plain string body (REST v2 / wiki markup). Mentions are "@Name". */
export function adfText(raw: unknown): string {
  if (raw === null || raw === undefined) return "";
  if (typeof raw === "string") return raw;
  const root = asNode(raw);
  if (!root) return "";
  let b = "";
  const walk = (n: AdfNode): void => {
    switch (n.type) {
      case "text":
        b += n.text ?? "";
        break;
      case "mention": {
        const t = typeof n.attrs?.["text"] === "string" ? (n.attrs["text"] as string) : "";
        if (t !== "") b += (t.startsWith("@") ? "" : "@") + t;
        break;
      }
      case "hardBreak":
        b += "\n";
        break;
    }
    for (const c of n.content ?? []) walk(c);
    switch (n.type) {
      case "paragraph":
      case "heading":
      case "codeBlock":
      case "listItem":
      case "blockquote":
        b += "\n";
    }
  };
  walk(root);
  return b.trim();
}

const WIKI_MENTION = /\[~accountid:([A-Za-z0-9:_-]+)\]/g;

/** Account IDs mentioned in an ADF document or wiki-markup body. */
export function adfMentions(raw: unknown): string[] {
  if (raw === null || raw === undefined) return [];
  if (typeof raw === "string") return [...raw.matchAll(WIKI_MENTION)].map((m) => m[1]!);
  const root = asNode(raw);
  if (!root) return [];
  const out: string[] = [];
  const walk = (n: AdfNode): void => {
    if (n.type === "mention") {
      const id = n.attrs?.["id"];
      if (typeof id === "string" && id !== "") out.push(id);
    }
    for (const c of n.content ?? []) walk(c);
  };
  walk(root);
  return out;
}
