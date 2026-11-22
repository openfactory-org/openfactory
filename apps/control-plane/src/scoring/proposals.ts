// Proposed definition changes: the pure parts of proposals.go (limits, path
// cleaning, merge onto the active tree, diff, review item body).
import { OfError, type ValidationError } from "@openfactory/core";
import { definitionRoot } from "./improve";
import { oneLine } from "@openfactory/core";
import { truncate } from "./text";

export const MAX_PROPOSAL_FILES = 200;
export const MAX_PROPOSAL_BYTES = 2 << 20;
export const MAX_DIFF_BODY_BYTES = 60_000;
export const MAX_DIFF_LINES_PER_HUNK = 4000;
export const DIFF_CONTEXT = 3;

export interface ProposalRequest {
  runId: string;
  /** Path (relative to the definition root) to full new content. */
  files: Record<string, string>;
  /** Paths to remove. */
  delete?: string[];
  message: string;
}

/** A proposal that the compiler or the preflight refused. Carries the validation errors. */
export function proposalError(errors: readonly ValidationError[]): OfError {
  let b = `the proposed definition has ${errors.length} error(s):`;
  for (let i = 0; i < errors.length; i++) {
    if (i === 20) {
      b += `\n- (${errors.length - i} more)`;
      break;
    }
    const ve = errors[i]!;
    b += `\n- ${validationErrorText(ve)}`;
    if (ve.line && ve.line > 0) b += ` (line ${ve.line})`;
  }
  return new OfError("invalid_request", b, { extra: { errors: [...errors] } });
}

/** ValidationError.Error() of the Go code: "file: field: message". */
export function validationErrorText(ve: ValidationError): string {
  let s = ve.file;
  if (ve.field) s += (s ? ": " : "") + ve.field;
  s += (s ? ": " : "") + ve.message;
  return s;
}

/** A definition path: slash separated, no "..", no leading "./" or "/". */
export function cleanDefinitionPath(p: string): string {
  const raw = p.replaceAll("\\", "/").trim();
  const stack: string[] = [];
  for (const part of ("/" + raw).split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") stack.pop();
    else stack.push(part);
  }
  const clean = stack.join("/");
  const trimmed = raw.startsWith("./") ? raw.slice(2) : raw;
  if (clean === "" || clean !== trimmed || clean.startsWith("../")) {
    throw new OfError("invalid_request", `${JSON.stringify(p)} is not a valid path in the definition`);
  }
  return clean;
}

export interface ProposalTree {
  files: Record<string, string>;
  diff: string;
}

/**
 * Merges the requested changes onto the base tree (the active version) and
 * returns the new tree with its diff. Throws invalid_request for a bad request.
 */
export function mergeProposal(
  base: Record<string, string> | null | undefined,
  req: Pick<ProposalRequest, "files" | "delete">,
): ProposalTree {
  const reqFiles = req.files ?? {};
  const del = req.delete ?? [];
  if (Object.keys(reqFiles).length === 0 && del.length === 0) {
    throw new OfError("invalid_request", "files or delete must name at least one file");
  }
  if (Object.keys(reqFiles).length + del.length > MAX_PROPOSAL_FILES) {
    throw new OfError("invalid_request", `a proposal can change at most ${MAX_PROPOSAL_FILES} files`);
  }
  const baseFiles = base ?? {};
  const root = definitionRoot(baseFiles);
  const files: Record<string, string> = { ...baseFiles };
  let size = 0;
  const enc = new TextEncoder();
  for (const [p, c] of Object.entries(reqFiles)) {
    const clean = cleanDefinitionPath(p);
    size += enc.encode(c).length;
    files[root + clean] = c;
  }
  if (size > MAX_PROPOSAL_BYTES) {
    throw new OfError("invalid_request", `the changed files are larger than ${MAX_PROPOSAL_BYTES} bytes`);
  }
  for (const p of del) {
    const clean = cleanDefinitionPath(p);
    if (!(root + clean in files))
      throw new OfError("invalid_request", `cannot delete ${clean}: the file does not exist`);
    delete files[root + clean];
  }
  const diff = diffFiles(baseFiles, files);
  if (diff === "") throw new OfError("invalid_request", "the proposal does not change the definition");
  return { files, diff };
}

/** The body of the review item of a proposal. */
export function proposalBody(
  v: { id: string; author_run_id: string; message: string; base_version_id: string },
  diff: string,
): string {
  let b = `Run ${v.author_run_id} proposes a change to the factory definition.\n\n`;
  b += `${v.message}\n\n`;
  b += `Proposed version: \`${v.id}\`. Base version: \`${v.base_version_id}\`. Approve applies the version. Reject sends your comment to the run.\n\n`;
  b += "```diff\n";
  if (new TextEncoder().encode(diff).length > MAX_DIFF_BODY_BYTES) {
    b += truncate(diff, MAX_DIFF_BODY_BYTES) + "\n";
  } else {
    b += diff;
  }
  b += "```\n";
  return b;
}

export const proposalTitle = (message: string): string =>
  "Review definition change: " + truncate(oneLine(message), 120);

// ---------- Diff ----------

const sortStr = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

/** A unified diff of two definition trees, file by file. */
export function diffFiles(
  oldTree: Record<string, string> | null | undefined,
  newTree: Record<string, string> | null | undefined,
): string {
  const o = oldTree ?? {};
  const n = newTree ?? {};
  const paths = [...new Set([...Object.keys(o), ...Object.keys(n)])].sort(sortStr);
  let b = "";
  for (const p of paths) {
    const inOld = p in o;
    const inNew = p in n;
    if (inOld && inNew && o[p] === n[p]) continue;
    b += `--- ${inOld ? "a/" + p : "/dev/null"}\n+++ ${inNew ? "b/" + p : "/dev/null"}\n`;
    b += lineDiff(splitLines(o[p] ?? ""), splitLines(n[p] ?? ""));
  }
  return b;
}

function splitLines(s: string): string[] {
  if (s === "") return [];
  return (s.endsWith("\n") ? s.slice(0, -1) : s).split("\n");
}

/** The hunks of a diff of two line lists, with DIFF_CONTEXT unchanged lines around each change. */
export function lineDiff(a: string[], b: string[]): string {
  const ops = lineOps(a, b);
  const show = new Array<boolean>(ops.length).fill(false);
  ops.forEach((op, i) => {
    if (op[0] === " ") return;
    for (let j = Math.max(0, i - DIFF_CONTEXT); j <= Math.min(ops.length - 1, i + DIFF_CONTEXT); j++) show[j] = true;
  });
  let out = "";
  let oldLine = 1;
  let newLine = 1;
  for (let i = 0; i < ops.length;) {
    if (!show[i]) {
      if (ops[i]![0] !== "+") oldLine++;
      if (ops[i]![0] !== "-") newLine++;
      i++;
      continue;
    }
    let j = i;
    let oldN = 0;
    let newN = 0;
    while (j < ops.length && show[j]) {
      if (ops[j]![0] !== "+") oldN++;
      if (ops[j]![0] !== "-") newN++;
      j++;
    }
    let oldStart = oldLine;
    let newStart = newLine;
    if (oldN === 0) oldStart--; // an empty range starts at the line before
    if (newN === 0) newStart--;
    out += `@@ -${oldStart},${oldN} +${newStart},${newN} @@\n`;
    for (const op of ops.slice(i, j)) out += op + "\n";
    oldLine += oldN;
    newLine += newN;
    i = j;
  }
  return out;
}

/** The edit script of two line lists: lines with the prefix " " (kept), "-" (removed) or "+" (added). */
export function lineOps(a: string[], b: string[]): string[] {
  if (a.length * b.length > (MAX_DIFF_LINES_PER_HUNK * MAX_DIFF_LINES_PER_HUNK) / 4) {
    return [...a.map((l) => "-" + l), ...b.map((l) => "+" + l)];
  }
  const n = a.length;
  const m = b.length;
  const lcs: Int32Array[] = Array.from({ length: n + 1 }, () => new Int32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      if (a[i] === b[j]) lcs[i]![j] = lcs[i + 1]![j + 1]! + 1;
      else lcs[i]![j] = lcs[i + 1]![j]! >= lcs[i]![j + 1]! ? lcs[i + 1]![j]! : lcs[i]![j + 1]!;
    }
  }
  const ops: string[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      ops.push(" " + a[i]!);
      i++;
      j++;
    } else if (lcs[i + 1]![j]! >= lcs[i]![j + 1]!) {
      ops.push("-" + a[i]!);
      i++;
    } else {
      ops.push("+" + b[j]!);
      j++;
    }
  }
  for (; i < n; i++) ops.push("-" + a[i]!);
  for (; j < m; j++) ops.push("+" + b[j]!);
  return ops;
}

/** States of a version that make it a proposal. */
export const isProposalState = (st: string | undefined): boolean =>
  st === "proposed" || st === "approved" || st === "rejected";
