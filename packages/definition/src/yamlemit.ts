// A small port of the gopkg.in/yaml.v3 encoder for string scalars, so that
// generated files (the default factory.yaml) are byte-equal to the Go output:
// the plain/single/double/literal style choice of the libyaml emitter, block
// indentation of 4, and no line wrapping.

import { resolvePlain } from "./yaml-resolve.ts";

const base60float = /^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$/;
const oldBools = ["y", "Y", "yes", "Yes", "YES", "on", "On", "ON", "n", "N", "no", "No", "NO", "off", "Off", "OFF"];

function isBase60Float(s: string): boolean {
  if (s === "") return false;
  const c = s[0]!;
  if (!(c === "+" || c === "-" || (c >= "0" && c <= "9")) || !s.includes(":")) return false;
  return base60float.test(s);
}

function isPrintable(cp: number): boolean {
  return (
    cp === 0x0a ||
    (cp >= 0x20 && cp <= 0x7e) ||
    cp === 0x85 ||
    (cp >= 0xa0 && cp <= 0xd7ff) ||
    (cp >= 0xe000 && cp <= 0xfffd && cp !== 0xfeff) ||
    (cp >= 0x10000 && cp <= 0x10ffff)
  );
}

function isBreak(cp: number): boolean {
  return cp === 0x0a || cp === 0x0d || cp === 0x85 || cp === 0x2028 || cp === 0x2029;
}

function isSpace(cp: number): boolean {
  return cp === 0x20;
}

function isBlankz(cp: number | undefined): boolean {
  return cp === undefined || cp === 0 || cp === 0x20 || cp === 0x09 || isBreak(cp);
}

interface Analysis {
  blockPlain: boolean;
  singleQuoted: boolean;
  block: boolean;
}

function analyze(s: string): Analysis {
  if (s === "") return { blockPlain: true, singleQuoted: true, block: false };
  const cps = [...s].map((ch) => ch.codePointAt(0)!);
  let blockIndicators = false;
  let specialCharacters = false;
  let lineBreaks = false;
  let leadingSpace = false;
  let leadingBreak = false;
  let trailingSpace = false;
  let trailingBreak = false;
  let breakSpace = false;
  let spaceBreak = false;
  let previousSpace = false;
  let previousBreak = false;
  if (s.startsWith("---") || s.startsWith("...")) blockIndicators = true;
  let precededByWhitespace = true;
  let followedByWhitespace = isBlankz(cps[1]);
  for (let i = 0; i < cps.length; i++) {
    const cp = cps[i]!;
    const ch = String.fromCodePoint(cp);
    if (i === 0) {
      if ("#,[]{}&*!|>'\"%@`".includes(ch)) blockIndicators = true;
      if ((ch === "?" || ch === ":") && followedByWhitespace) blockIndicators = true;
      if (ch === "-" && followedByWhitespace) blockIndicators = true;
    } else {
      if (ch === ":" && followedByWhitespace) blockIndicators = true;
      if (ch === "#" && precededByWhitespace) blockIndicators = true;
    }
    if (!isPrintable(cp)) specialCharacters = true;
    if (isBreak(cp)) lineBreaks = true;
    if (isSpace(cp)) {
      if (i === 0) leadingSpace = true;
      if (i === cps.length - 1) trailingSpace = true;
      if (previousBreak) breakSpace = true;
      previousSpace = true;
      previousBreak = false;
    } else if (isBreak(cp)) {
      if (i === 0) leadingBreak = true;
      if (i === cps.length - 1) trailingBreak = true;
      if (previousSpace) spaceBreak = true;
      previousBreak = true;
      previousSpace = false;
    } else {
      previousSpace = false;
      previousBreak = false;
    }
    precededByWhitespace = isBlankz(cp);
    followedByWhitespace = isBlankz(cps[i + 2]);
  }
  const a: Analysis = { blockPlain: true, singleQuoted: true, block: true };
  if (leadingSpace || leadingBreak || trailingSpace || trailingBreak) a.blockPlain = false;
  if (trailingSpace) a.block = false;
  if (breakSpace) {
    a.blockPlain = false;
    a.singleQuoted = false;
  }
  if (spaceBreak || specialCharacters) {
    a.blockPlain = false;
    a.singleQuoted = false;
    a.block = false;
  }
  if (lineBreaks) a.blockPlain = false;
  if (blockIndicators) a.blockPlain = false;
  return a;
}

function hex(cp: number, width: number): string {
  return cp.toString(16).toUpperCase().padStart(width, "0");
}

function doubleQuoted(s: string): string {
  let out = '"';
  for (const ch of s) {
    const cp = ch.codePointAt(0)!;
    if (!isPrintable(cp) || cp === 0xfeff || isBreak(cp) || ch === '"' || ch === "\\") {
      const esc: Record<number, string> = {
        0x00: "0",
        0x07: "a",
        0x08: "b",
        0x09: "t",
        0x0a: "n",
        0x0b: "v",
        0x0c: "f",
        0x0d: "r",
        0x1b: "e",
        0x22: '"',
        0x5c: "\\",
        0x85: "N",
        0xa0: "_",
        0x2028: "L",
        0x2029: "P",
      };
      const e = esc[cp];
      if (e !== undefined) out += "\\" + e;
      else if (cp <= 0xff) out += "\\x" + hex(cp, 2);
      else if (cp <= 0xffff) out += "\\u" + hex(cp, 4);
      else out += "\\U" + hex(cp, 8);
    } else {
      out += ch;
    }
  }
  return out + '"';
}

function literal(s: string, indent: number): string {
  const cps = [...s].map((ch) => ch.codePointAt(0)!);
  let out = "|";
  if (cps.length > 0 && (isSpace(cps[0]!) || isBreak(cps[0]!))) out += "4";
  if (cps.length === 0 || !isBreak(cps[cps.length - 1]!)) out += "-";
  else if (cps.length === 1 || isBreak(cps[cps.length - 2]!)) out += "+";
  let col = 1;
  let breaks = true;
  for (const cp of cps) {
    if (isBreak(cp)) {
      out += "\n";
      col = 0;
      breaks = true;
    } else {
      if (breaks) {
        if (col !== 0) out += "\n";
        out += " ".repeat(indent);
        col = indent;
      }
      out += String.fromCodePoint(cp);
      col++;
      breaks = false;
    }
  }
  return col !== 0 ? out + "\n" : out;
}

/**
 * yamlString renders a string value like yaml.v3 Marshal. The result ends
 * with a newline. indent is the indentation of block scalar lines.
 */
export function yamlString(s: string, indent: number): string {
  const canUsePlain = resolvePlain(s).tag === "!!str" && !(isBase60Float(s) || oldBools.includes(s));
  const a = analyze(s);
  let style: "plain" | "single" | "double" | "literal";
  if (s.includes("\n")) style = "literal";
  else if (canUsePlain) style = "plain";
  else style = "double";
  if (style === "plain" && (!a.blockPlain || s.length === 0)) style = "single";
  if (style === "single" && !a.singleQuoted) style = "double";
  if (style === "literal" && !a.block) style = "double";
  switch (style) {
    case "plain":
      return s + "\n";
    case "single":
      return "'" + s.replaceAll("'", "''") + "'\n";
    case "double":
      return doubleQuoted(s) + "\n";
    case "literal":
      return literal(s, indent);
  }
}
