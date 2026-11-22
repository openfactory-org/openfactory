// KeyLine. Port of internal/definition/locate.go.

import { parseYAML, resolve, splitFrontmatter } from "./yamlnode.ts";

interface PathSeg {
  key: string;
  /** -1 for a mapping key. */
  index: number;
}

/** splitKeyPath splits "a.b[0][1].c" into a, b, [0], [1], c; null for a bad index. */
function splitKeyPath(path: string): PathSeg[] | null {
  const out: PathSeg[] = [];
  for (const part of path.split(".")) {
    const b = part.indexOf("[");
    const key = b >= 0 ? part.slice(0, b) : part;
    let rest = b >= 0 ? part.slice(b + 1) : "";
    if (key !== "") out.push({ key, index: -1 });
    while (rest !== "") {
      const e = rest.indexOf("]");
      const num = e >= 0 ? rest.slice(0, e) : rest;
      if (e < 0 || !/^[+-]?[0-9]+$/.test(num)) return null;
      const i = Number(num);
      if (i < 0) return null;
      out.push({ key: "", index: i });
      rest = rest.slice(e + 1);
      if (rest.startsWith("[")) rest = rest.slice(1);
    }
  }
  return out;
}

/**
 * keyLine returns the 1-based line of a key path in a definition file, or 0
 * when the file does not parse or the path is not in it. A ".md" file is read
 * as Markdown with YAML frontmatter. The path uses the form of
 * ValidationError.field, for example "mcpServers.docs.managedId" or
 * "triggers[0].filter.webhook_ids[0]".
 */
export function keyLine(file: string, content: string, path: string): number {
  let src = content;
  let offset = 0;
  if (file.endsWith(".md")) {
    const fm = splitFrontmatter(content);
    if (fm.error !== null || !fm.hasFM) return 0;
    src = fm.fm;
    offset = fm.offset;
  }
  const r = parseYAML(src);
  if (r.error !== null || r.root === null) return 0;
  let n = r.root;
  let line = 0;
  const segs = splitKeyPath(path) ?? [];
  for (const seg of segs) {
    const cur = resolve(n);
    if (cur === null) return 0;
    if (seg.index >= 0) {
      if (cur.kind !== "seq" || seg.index >= cur.content.length) return 0;
      n = cur.content[seg.index]!;
      line = n.line;
      continue;
    }
    if (cur.kind !== "map") return 0;
    let found = false;
    for (let i = 0; i + 1 < cur.content.length; i += 2) {
      if (cur.content[i]!.value === seg.key) {
        line = cur.content[i]!.line;
        n = cur.content[i + 1]!;
        found = true;
        break;
      }
    }
    if (!found) return 0;
  }
  if (line === 0) return 0;
  return line + offset;
}
