// Path globs for the `paths` filter. Port of internal/triggers/glob.go with
// a port of Go path.Match ("*", "?", "[...]", "\\" escapes; "*" never
// crosses "/").

export class BadPatternError extends Error {
  constructor() {
    super("syntax error in pattern");
  }
}

/** Parses a [...] class at p[i] (after "["); returns the index after "]" and whether c matches. */
function matchClass(p: string, i: number, c: string): { next: number; ok: boolean } {
  let negated = false;
  if (p[i] === "^") {
    negated = true;
    i++;
  }
  let matched = false;
  let first = true;
  for (;;) {
    if (i >= p.length) throw new BadPatternError();
    if (p[i] === "]" && !first) {
      i++;
      break;
    }
    first = false;
    let lo: string;
    [lo, i] = classChar(p, i);
    let hi = lo;
    if (p[i] === "-") {
      [hi, i] = classChar(p, i + 1);
    }
    if (lo <= c && c <= hi) matched = true;
  }
  return { next: i, ok: matched !== negated };
}

function classChar(p: string, i: number): [string, number] {
  if (i >= p.length || p[i] === "-" || p[i] === "]") throw new BadPatternError();
  if (p[i] === "\\") {
    i++;
    if (i >= p.length) throw new BadPatternError();
  }
  const cp = p.codePointAt(i)!;
  const ch = String.fromCodePoint(cp);
  return [ch, i + ch.length];
}

/** Checks the whole pattern syntax (Go reports errors even after a mismatch). */
function validate(p: string): void {
  for (let i = 0; i < p.length;) {
    const c = p[i]!;
    if (c === "\\") {
      if (i + 1 >= p.length) throw new BadPatternError();
      i += 2;
    } else if (c === "[") {
      matchClass(p, i + 1, "\0");
      // advance past the class
      let j = i + 1;
      if (p[j] === "^") j++;
      let first = true;
      for (;;) {
        if (p[j] === "]" && !first) break;
        first = false;
        if (p[j] === "\\") j++;
        j++;
      }
      i = j + 1;
    } else {
      i++;
    }
  }
}

/** Go path.Match. Throws BadPatternError for a malformed pattern. */
export function pathMatch(pattern: string, name: string): boolean {
  validate(pattern);
  return matchHere(pattern, 0, Array.from(name), 0);
}

function matchHere(p: string, pi: number, s: string[], si: number): boolean {
  while (pi < p.length) {
    const c = p[pi]!;
    if (c === "*") {
      while (p[pi] === "*") pi++;
      if (pi === p.length) return !s.slice(si).includes("/");
      for (let k = si; k <= s.length; k++) {
        if (k > si && s[k - 1] === "/") break;
        if (matchHere(p, pi, s, k)) return true;
      }
      return false;
    }
    if (si >= s.length) return false;
    const ch = s[si]!;
    if (c === "?") {
      if (ch === "/") return false;
      pi++;
      si++;
      continue;
    }
    if (c === "[") {
      if (ch === "/") return false;
      const r = matchClass(p, pi + 1, ch);
      if (!r.ok) return false;
      pi = r.next;
      si++;
      continue;
    }
    if (c === "\\") pi++;
    const lit = String.fromCodePoint(p.codePointAt(pi)!);
    if (ch !== lit) return false;
    pi += lit.length;
    si++;
  }
  return si === s.length;
}

/** Matches a repository-relative path against a glob; "**" segments match zero or more segments. */
export function matchGlob(pattern: string, name: string): boolean {
  return matchSegments(pattern.replace(/^\//, "").split("/"), name.replace(/^\//, "").split("/"));
}

function matchSegments(pat: string[], segs: string[]): boolean {
  while (pat.length > 0) {
    const p = pat[0]!;
    if (p === "**") {
      while (pat.length > 1 && pat[1] === "**") pat = pat.slice(1);
      if (pat.length === 1) return true;
      for (let i = 0; i <= segs.length; i++) if (matchSegments(pat.slice(1), segs.slice(i))) return true;
      return false;
    }
    if (segs.length === 0) return false;
    let ok: boolean;
    try {
      ok = pathMatch(p, segs[0]!);
    } catch {
      return false;
    }
    if (!ok) return false;
    pat = pat.slice(1);
    segs = segs.slice(1);
  }
  return segs.length === 0;
}

/** Returns an error message for a malformed glob, or null. */
export function validateGlob(pattern: string): string | null {
  if (pattern === "") return "empty glob";
  for (const seg of pattern.replace(/^\//, "").split("/")) {
    if (seg === "**") continue;
    if (seg.includes("**")) return `"**" must be a whole path segment in ${pattern}`;
    try {
      pathMatch(seg, "");
    } catch {
      return "malformed glob " + pattern;
    }
  }
  return null;
}
