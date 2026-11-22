// YAML node tree and decoders. Port of internal/definition/yamlnode.go.
//
// The Go compiler walks gopkg.in/yaml.v3 nodes. parseYAML builds the same
// tree (kinds, tags, values, 1-based lines) from the `yaml` package, parsed
// with the failsafe schema so that every scalar keeps its source text; tags
// are then resolved with the yaml.v3 rules (yaml-resolve.ts).

import { isAlias, isMap, isPair, isScalar, isSeq, LineCounter, parseAllDocuments, type Document } from "yaml";
import { goFloat, goQuote, goTrimSpace } from "./gofmt.ts";
import { decodeBool, decodeFloat, decodeInt, resolvePlain, shortTag } from "./yaml-resolve.ts";
import { isOneOf } from "@openfactory/core";
import type { ValidationError } from "./types.ts";

export type NodeKind = "map" | "seq" | "scalar" | "alias";

export interface YNode {
  kind: NodeKind;
  tag: string;
  value: string;
  line: number;
  content: YNode[];
  /** Target of an alias node. */
  alias: YNode | null;
  /** True for quoted and block scalars. */
  styled: boolean;
  /** True when the tag was written in the document. */
  explicitTag: boolean;
}

function scalarNode(value: string, tag: string, line: number, styled = false, explicitTag = false): YNode {
  return { kind: "scalar", tag, value, line, content: [], alias: null, styled, explicitTag };
}

export function emptyMapping(line: number): YNode {
  return { kind: "map", tag: "!!map", value: "", line, content: [], alias: null, styled: false, explicitTag: false };
}

export interface ParseResult {
  /** Root node; null when the source holds no document. */
  root: YNode | null;
  error: { line: number; message: string } | null;
}

type AnyNode = unknown;

/** parseYAML parses the first document of src like yaml.Unmarshal into a yaml.Node. */
export function parseYAML(src: string): ParseResult {
  const lc = new LineCounter();
  let docs: Document.Parsed[] | unknown;
  try {
    docs = parseAllDocuments(src, { lineCounter: lc, schema: "failsafe", uniqueKeys: false, prettyErrors: false });
  } catch (err) {
    return { root: null, error: { line: 0, message: "yaml: " + String(err) } };
  }
  if (!Array.isArray(docs) || docs.length === 0) return { root: null, error: null };
  const doc = docs[0] as Document.Parsed;
  if (doc.errors.length > 0) {
    const e = doc.errors[0]!;
    const line = lc.linePos(e.pos[0]).line;
    return { root: null, error: { line, message: firstLine(e.message) } };
  }
  const lineOf = (n: { range?: [number, number, number] | null } | null | undefined, fallback: number): number =>
    n?.range ? lc.linePos(n.range[0]).line : fallback;
  const cache = new Map<AnyNode, YNode>();
  let aliasError: string | null = null;

  const convert = (n: AnyNode, fallbackLine: number): YNode => {
    if (n === null || n === undefined) return scalarNode("", "!!null", fallbackLine);
    const cached = cache.get(n);
    if (cached) return cached;
    if (isScalar(n)) {
      const line = lineOf(n, fallbackLine);
      const value = n.value === null || n.value === undefined ? "" : String(n.value);
      const styled = n.type !== undefined && n.type !== "PLAIN";
      const explicit = typeof n.tag === "string" && n.tag !== "" && n.tag !== "!";
      let tag: string;
      if (explicit) tag = shortTag(n.tag as string);
      else if (styled) tag = "!!str";
      else if (value === "<<") tag = "!!merge";
      else tag = resolvePlain(value).tag;
      const y = scalarNode(value, tag, line, styled, explicit);
      cache.set(n, y);
      return y;
    }
    if (isMap(n)) {
      const explicit = typeof n.tag === "string" && n.tag !== "" && n.tag !== "!";
      const y: YNode = {
        kind: "map",
        tag: explicit ? shortTag(n.tag as string) : "!!map",
        value: "",
        line: lineOf(n, fallbackLine),
        content: [],
        alias: null,
        styled: false,
        explicitTag: explicit,
      };
      cache.set(n, y);
      for (const p of n.items) {
        const k = convert(p.key, y.line);
        y.content.push(k);
        y.content.push(convert(p.value, k.line));
      }
      return y;
    }
    if (isSeq(n)) {
      const explicit = typeof n.tag === "string" && n.tag !== "" && n.tag !== "!";
      const y: YNode = {
        kind: "seq",
        tag: explicit ? shortTag(n.tag as string) : "!!seq",
        value: "",
        line: lineOf(n, fallbackLine),
        content: [],
        alias: null,
        styled: false,
        explicitTag: explicit,
      };
      cache.set(n, y);
      for (const it of n.items) {
        if (isPair(it)) {
          const k = convert(it.key, y.line);
          const m = emptyMapping(k.line);
          m.content.push(k, convert(it.value, k.line));
          y.content.push(m);
        } else {
          y.content.push(convert(it, y.line));
        }
      }
      return y;
    }
    if (isAlias(n)) {
      const target = n.resolve(doc);
      const y: YNode = {
        kind: "alias",
        tag: "",
        value: n.source,
        line: lineOf(n, fallbackLine),
        content: [],
        alias: null,
        styled: false,
        explicitTag: false,
      };
      cache.set(n, y);
      if (target === undefined) {
        aliasError ??= `yaml: unknown anchor '${n.source}' referenced`;
        y.alias = scalarNode("", "!!null", y.line);
      } else {
        y.alias = convert(target, y.line);
      }
      return y;
    }
    if (isPair(n)) {
      const k = convert(n.key, fallbackLine);
      const m = emptyMapping(k.line);
      m.content.push(k, convert(n.value, k.line));
      return m;
    }
    return scalarNode("", "!!null", fallbackLine);
  };

  const root = convert(doc.contents, 1);
  if (aliasError !== null) return { root: null, error: { line: 0, message: aliasError } };
  return { root, error: null };
}

function firstLine(s: string): string {
  const i = s.indexOf("\n");
  const line = i >= 0 ? s.slice(0, i) : s;
  return line.replace(/ at line \d+, column \d+:?$/, "");
}

// ---- diagnostics ----

export interface RawError {
  file: string;
  line: number;
  field: string;
  message: string;
}

/** Diag collects validation errors for a compile. */
export class Diag {
  errs: RawError[] = [];

  add(file: string, line: number, field: string, message: string): void {
    this.errs.push({ file, line, field, message });
  }

  /** sorted returns the errors sorted by file, then line (stable). */
  sorted(): ValidationError[] {
    const out = [...this.errs];
    out.sort((a, b) => {
      if (a.file !== b.file) return a.file < b.file ? -1 : 1;
      return a.line - b.line;
    });
    return out.map(encodeError);
  }
}

export function encodeError(e: RawError): ValidationError {
  const out: ValidationError = { file: e.file, message: e.message };
  if (e.line !== 0) out.line = e.line;
  if (e.field !== "") out.field = e.field;
  return out;
}

// ---- node helpers ----

/** put sets o[k] as an own property, also for keys such as "__proto__". */
export function put<V>(o: Record<string, V>, k: string, v: V): void {
  if (k === "__proto__")
    Object.defineProperty(o, k, { value: v, enumerable: true, writable: true, configurable: true });
  else o[k] = v;
}

export function resolve(n: YNode | null | undefined): YNode | null {
  let cur = n ?? null;
  while (cur !== null && cur.kind === "alias") cur = cur.alias;
  return cur;
}

export function isSeqNode(n: YNode | null | undefined): boolean {
  const r = resolve(n);
  return r !== null && r.kind === "seq";
}

export function isNull(n: YNode | null | undefined): boolean {
  const r = resolve(n);
  return r === null || (r.kind === "scalar" && r.tag === "!!null");
}

export function joinPath(base: string, key: string): string {
  return base === "" ? key : base + "." + key;
}

export function indexPath(base: string, i: number): string {
  return `${base}[${i}]`;
}

function indicatedString(n: YNode): boolean {
  return n.kind === "scalar" && (n.tag === "!!str" || (!n.explicitTag && n.styled));
}

export function nodeBool(n: YNode): boolean | null {
  return decodeBool(n.tag, n.value, indicatedString(n));
}

export function nodeInt(n: YNode): number | null {
  return decodeInt(n.tag, n.value, indicatedString(n));
}

export function nodeFloat(n: YNode): number | null {
  return decodeFloat(n.tag, n.value, indicatedString(n));
}

/** Doc is one YAML document being decoded: its file and line offset. */
export class Doc {
  readonly d: Diag;
  readonly file: string;
  readonly offset: number;

  constructor(d: Diag, file: string, offset = 0) {
    this.d = d;
    this.file = file;
    this.offset = offset;
  }

  line(n: YNode | null | undefined): number {
    if (!n) return 0;
    return n.line + this.offset;
  }

  errf(n: YNode | null | undefined, field: string, message: string): void {
    this.d.add(this.file, this.line(n), field, message);
  }

  /** parse parses content into its root node; an empty document is an empty mapping. */
  parse(content: string): YNode | null {
    const r = parseYAML(content);
    if (r.error) {
      let line = 0;
      let msg = r.error.message;
      if (r.error.line > 0) {
        line = r.error.line + this.offset;
        msg = "invalid YAML: " + msg;
      }
      this.d.add(this.file, line, "", msg);
      return null;
    }
    if (r.root === null) return emptyMapping(1);
    return r.root;
  }

  mapping(n: YNode | null | undefined, path: string): Mapping | null {
    const r = resolve(n);
    if (r === null || r.kind !== "map") {
      this.errf(r, path, `${describe(path)} must be an object`);
      return null;
    }
    const m = new Mapping(this, r, path);
    for (let i = 0; i + 1 < r.content.length; i += 2) {
      const k = resolve(r.content[i])!;
      const v = r.content[i + 1]!;
      if (k.kind === "scalar" && k.tag === "!!merge") {
        this.errf(k, path, "YAML merge keys (<<) are not supported");
        continue;
      }
      if (k.kind !== "scalar") {
        this.errf(k, path, `${describe(path)} keys must be strings`);
        continue;
      }
      if (m.vals.has(k.value)) {
        this.errf(k, joinPath(path, k.value), `duplicate key ${goQuote(k.value)}`);
        continue;
      }
      m.order.push(k.value);
      m.vals.set(k.value, v);
      m.keys.set(k.value, k);
    }
    return m;
  }

  str(n: YNode | null | undefined, path: string): [string, boolean] {
    const r = resolve(n);
    if (r === null || r.kind !== "scalar" || r.tag !== "!!str") {
      this.errf(r, path, `${path} must be a string${quoteHint(r)}`);
      return ["", false];
    }
    return [r.value, true];
  }

  nonEmptyStr(n: YNode | null | undefined, path: string): [string, boolean] {
    const [s, ok] = this.str(n, path);
    if (!ok) return ["", false];
    if (goTrimSpace(s) === "") {
      this.errf(n, path, `${path} must not be empty`);
      return ["", false];
    }
    return [s, true];
  }

  boolean(n: YNode | null | undefined, path: string): [boolean, boolean] {
    const r = resolve(n);
    if (r === null || r.kind !== "scalar" || r.tag !== "!!bool") {
      this.errf(r, path, `${path} must be true or false`);
      return [false, false];
    }
    const b = nodeBool(r);
    if (b === null) {
      this.errf(r, path, `${path} must be true or false`);
      return [false, false];
    }
    return [b, true];
  }

  integer(n: YNode | null | undefined, path: string): [number, boolean] {
    const r = resolve(n);
    if (r === null || r.kind !== "scalar" || r.tag !== "!!int") {
      this.errf(r, path, `${path} must be an integer`);
      return [0, false];
    }
    const i = nodeInt(r);
    if (i === null) {
      this.errf(r, path, `${path} must be an integer`);
      return [0, false];
    }
    return [i, true];
  }

  number(n: YNode | null | undefined, path: string): [number, boolean] {
    const r = resolve(n);
    if (r === null || r.kind !== "scalar" || (r.tag !== "!!int" && r.tag !== "!!float")) {
      this.errf(r, path, `${path} must be a number`);
      return [0, false];
    }
    const f = nodeFloat(r);
    if (f === null || !Number.isFinite(f)) {
      this.errf(r, path, `${path} must be a finite number`);
      return [0, false];
    }
    return [f, true];
  }

  intRange(n: YNode | null | undefined, path: string, lo: number, hi: number): [number, boolean] {
    const [i, ok] = this.integer(n, path);
    if (!ok) return [0, false];
    if (i < lo || i > hi) {
      this.errf(n, path, `${path} must be between ${lo} and ${hi}`);
      return [0, false];
    }
    return [i, true];
  }

  /** enum returns the value when it is one of allowed, else "" (with an error). */
  enum<T extends string>(n: YNode | null | undefined, path: string, ...allowed: readonly T[]): [T, true] | ["", false] {
    const [s, ok] = this.str(n, path);
    if (!ok) return ["", false];
    if (isOneOf(allowed, s)) return [s, true];
    this.errf(n, path, `${path} must be one of ${allowed.join(", ")} (got ${goQuote(s)})`);
    return ["", false];
  }

  /** seq returns the items of a list, or null (with an error) when n is not a list. */
  seq(n: YNode | null | undefined, path: string): YNode[] | null {
    const r = resolve(n);
    if (r === null || r.kind !== "seq") {
      this.errf(r, path, `${path} must be a list`);
      return null;
    }
    return r.content;
  }

  strList(n: YNode | null | undefined, path: string): [string[] | null, boolean] {
    const items = this.seq(n, path);
    if (items === null) return [null, false];
    const out: string[] = [];
    let ok = true;
    items.forEach((it, i) => {
      const [s, good] = this.nonEmptyStr(it, indexPath(path, i));
      if (!good) {
        ok = false;
        return;
      }
      out.push(s);
    });
    return [out, ok];
  }

  uniqueStrList(n: YNode | null | undefined, path: string): [string[] | null, boolean] {
    const [list, ok0] = this.strList(n, path);
    if (!ok0 || list === null) return [list, false];
    let ok = true;
    const seen = new Set<string>();
    const items = resolve(n)!.content;
    list.forEach((s, i) => {
      if (seen.has(s)) {
        this.errf(items[i], indexPath(path, i), `${path} lists ${goQuote(s)} more than once`);
        ok = false;
      }
      seen.add(s);
    });
    return [list, ok];
  }

  strMap(n: YNode | null | undefined, path: string): [Record<string, string> | null, boolean] {
    const m = this.mapping(n, path);
    if (m === null) return [null, false];
    const out: Record<string, string> = {};
    let ok = true;
    for (const k of m.order) {
      const v = m.get(k);
      const [s, good] = this.str(v, m.field(k));
      if (!good) {
        ok = false;
        continue;
      }
      put(out, k, s);
    }
    return [out, ok];
  }
}

export function describe(path: string): string {
  return path === "" ? "document" : path;
}

export function quoteHint(n: YNode | null): string {
  if (n !== null && n.kind === "scalar" && (n.tag === "!!int" || n.tag === "!!float" || n.tag === "!!bool")) {
    return ` (quote ${n.value} to keep it a string)`;
  }
  return "";
}

/** Mapping is a YAML mapping being decoded; finish reports unconsumed keys. */
export class Mapping {
  order: string[] = [];
  vals = new Map<string, YNode>();
  keys = new Map<string, YNode>();
  used = new Set<string>();
  readonly c: Doc;
  readonly node: YNode;
  readonly path: string;

  constructor(c: Doc, node: YNode, path: string) {
    this.c = c;
    this.node = node;
    this.path = path;
  }

  field(k: string): string {
    return joinPath(this.path, k);
  }

  has(k: string): boolean {
    return this.vals.has(k);
  }

  /** get returns the value of k (null when absent) and marks it consumed. */
  get(k: string): YNode | null {
    const v = this.vals.get(k);
    if (v === undefined) return null;
    this.used.add(k);
    return v;
  }

  /** present reports whether k is present with a non-null value. */
  present(k: string): boolean {
    const v = this.vals.get(k);
    return v !== undefined && !isNull(v);
  }

  keyNode(k: string): YNode {
    return this.keys.get(k) ?? this.node;
  }

  require(k: string): YNode | null {
    const v = this.get(k);
    if (v === null || isNull(v)) {
      this.c.errf(this.node, this.field(k), `${this.field(k)} is required`);
      return null;
    }
    return v;
  }

  finish(): void {
    for (const k of this.order) {
      if (!this.used.has(k)) this.c.errf(this.keys.get(k), this.field(k), `unknown key ${goQuote(this.field(k))}`);
    }
  }
}

/** toValue converts a node into a JSON-compatible value. */
export function toValue(n: YNode | null | undefined): unknown {
  const r = resolve(n);
  if (r === null) return null;
  switch (r.kind) {
    case "map": {
      const m: Record<string, unknown> = {};
      for (let i = 0; i + 1 < r.content.length; i += 2) {
        put(m, resolve(r.content[i])?.value ?? "", toValue(r.content[i + 1]));
      }
      return m;
    }
    case "seq":
      return r.content.map((c) => toValue(c));
    case "scalar":
      switch (r.tag) {
        case "!!null":
          return null;
        case "!!bool":
          return nodeBool(r) ?? false;
        case "!!int": {
          const i = nodeInt(r);
          if (i !== null) return i;
          return nodeFloat(r) ?? 0;
        }
        case "!!float":
          return nodeFloat(r) ?? 0;
        default:
          return r.value;
      }
  }
  return null;
}

/** sprint formats a scalar node's decoded value like Go's fmt.Sprint of toValue. */
export function sprintNode(n: YNode | null | undefined): string {
  const r = resolve(n);
  if (r === null) return "<nil>";
  const v = toValue(r);
  if (v === null) return "<nil>";
  if (r.kind === "scalar" && r.tag === "!!float" && typeof v === "number") return goFloat(v);
  return String(v);
}

export interface Frontmatter {
  fm: string;
  body: string;
  hasFM: boolean;
  offset: number;
  error: string | null;
}

/** splitFrontmatter splits a Markdown document into YAML frontmatter and body. */
export function splitFrontmatter(content: string): Frontmatter {
  let s = content.startsWith("﻿") ? content.slice(1) : content;
  s = s.replaceAll("\r\n", "\n");
  const nl = s.indexOf("\n");
  const first = nl >= 0 ? s.slice(0, nl) : s;
  if (first.replace(/[ \t]+$/, "") !== "---") return { fm: "", body: s, hasFM: false, offset: 0, error: null };
  const lines = s.split("\n");
  for (let i = 1; i < lines.length; i++) {
    if (lines[i]!.replace(/[ \t]+$/, "") === "---") {
      return {
        fm: lines.slice(1, i).join("\n"),
        body: lines.slice(i + 1).join("\n"),
        hasFM: true,
        offset: 1,
        error: null,
      };
    }
  }
  return {
    fm: "",
    body: "",
    hasFM: true,
    offset: 1,
    error: 'frontmatter starting on line 1 is not closed by a "---" line',
  };
}
