// YAML editing with the yaml Document API: keeps comments and key order.
// Used by the dashboard edits and the webhook routes.
import { OfError } from "@openfactory/core";
import { Document, isMap, isScalar, parseDocument, YAMLMap } from "yaml";

export function yamlMapping(src: string): Document.Parsed {
  const doc = parseDocument(src.trim() === "" ? "{}" : src);
  if (doc.errors.length) throw new OfError("invalid_request", `cannot parse YAML: ${doc.errors[0]!.message}`);
  if (doc.contents === null) doc.contents = new YAMLMap() as never;
  if (!isMap(doc.contents)) throw new OfError("invalid_request", "YAML document root must be a mapping");
  return doc;
}

export const yamlText = (doc: Document) => doc.toString({ indent: 2, lineWidth: 0 });

/** editYAMLDocument: sets (or with null removes) top-level keys, sorted. */
export function editYamlDocument(src: string, set: Record<string, unknown>): string {
  const doc = yamlMapping(src);
  for (const k of Object.keys(set).sort()) {
    const v = set[k];
    if (v === null || v === undefined) doc.delete(k);
    else doc.set(k, v);
  }
  return yamlText(doc);
}

export interface MdDoc {
  front: Document;
  body: string;
}

/** parseMarkdownDoc: YAML frontmatter between "---" lines, then the body. */
export function parseMarkdownDoc(src: string): MdDoc {
  const s = src.replace(/\r\n/g, "\n");
  const empty = () => new Document(new YAMLMap());
  if (!s.startsWith("---\n") && s !== "---") return { front: empty(), body: s };
  let rest = s.slice(3);
  if (rest.startsWith("\n")) rest = rest.slice(1);
  let end = -1;
  if (rest.startsWith("---\n") || rest === "---") end = 0;
  else if (rest.includes("\n---\n")) end = rest.indexOf("\n---\n") + 1;
  else if (rest.endsWith("\n---")) end = rest.length - 3;
  if (end < 0) throw new Error("frontmatter is not terminated by a '---' line");
  const fm = rest.slice(0, end);
  let body = rest.slice(end).replace(/^---/, "");
  if (body.startsWith("\n")) body = body.slice(1);
  let front = empty();
  if (fm.trim() !== "") {
    const d = parseDocument(fm);
    if (d.errors.length) throw new Error(`frontmatter: ${d.errors[0]!.message}`);
    if (d.contents !== null) {
      if (!isMap(d.contents)) throw new Error("frontmatter must be a YAML mapping");
      front = d;
    }
  }
  return { front, body };
}

export function mdString(d: MdDoc): string {
  let out = "---\n";
  if (isMap(d.front.contents) && d.front.contents.items.length > 0) out += yamlText(d.front);
  out += "---\n";
  const body = d.body.replace(/^\n+/, "");
  if (body !== "") out += "\n" + body + (body.endsWith("\n") ? "" : "\n");
  return out;
}

export function nodeString(n: unknown): string {
  if (isScalar(n)) return n.value === null || n.value === undefined ? "" : String(n.value);
  return "";
}

export function frontGet(d: Document, path: string[]): unknown {
  return d.getIn(path, true);
}

export function setOrDelete(d: Document, path: string[], v: string): void {
  const t = v.trim();
  if (t === "") d.deleteIn(path);
  else d.setIn(path, t);
}
