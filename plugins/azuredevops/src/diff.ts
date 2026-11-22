// Azure DevOps has no call for the unified diff of a file. The plugin reads the
// file at the merge base and at the head of the pull request and makes the diff
// itself. Port of plugins/azuredevops/internal/azuredevops/diff.go.

/** Unchanged lines around a change. */
const DIFF_CONTEXT = 3;
/** Limit of the line comparison table. Two longer middles are shown as one replaced block. */
const MAX_DIFF_CELLS = 4_000_000;

type Kind = " " | "-" | "+";
interface DiffOp {
  kind: Kind;
  text: string;
}

export function splitLines(s: string): string[] {
  if (s === "") return [];
  return (s.endsWith("\n") ? s.slice(0, -1) : s).split("\n");
}

/** The edit script of two line lists. */
export function diffOps(a: string[], b: string[]): DiffOp[] {
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  const ops: DiffOp[] = [];
  for (const l of a.slice(0, pre)) ops.push({ kind: " ", text: l });
  const ma = a.slice(pre, a.length - suf);
  const mb = b.slice(pre, b.length - suf);
  if (ma.length === 0 || mb.length === 0 || ma.length * mb.length > MAX_DIFF_CELLS) {
    for (const l of ma) ops.push({ kind: "-", text: l });
    for (const l of mb) ops.push({ kind: "+", text: l });
  } else {
    // Longest common subsequence of the middles.
    const n = ma.length;
    const m = mb.length;
    const w = m + 1;
    const t = new Int32Array((n + 1) * w);
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        t[i * w + j] = ma[i] === mb[j] ? t[(i + 1) * w + j + 1]! + 1 : Math.max(t[(i + 1) * w + j]!, t[i * w + j + 1]!);
      }
    }
    let i = 0;
    let j = 0;
    while (i < n || j < m) {
      if (i < n && j < m && ma[i] === mb[j]) {
        ops.push({ kind: " ", text: ma[i]! });
        i++;
        j++;
      } else if (i < n && (j === m || t[(i + 1) * w + j]! >= t[i * w + j + 1]!)) {
        // A removed line comes before an added one.
        ops.push({ kind: "-", text: ma[i]! });
        i++;
      } else {
        ops.push({ kind: "+", text: mb[j]! });
        j++;
      }
    }
  }
  for (const l of a.slice(a.length - suf)) ops.push({ kind: " ", text: l });
  return ops;
}

function hunkRange(start: number, n: number): string {
  return n === 1 ? String(start) : `${start},${n}`;
}

/** The hunks of the diff of two texts, without the file header. Empty when the texts are the same. */
export function unifiedDiff(oldText: string, newText: string): string {
  const ops = diffOps(splitLines(oldText), splitLines(newText));
  // Mark the lines that a hunk shows: each change and its context.
  const show = new Array<boolean>(ops.length).fill(false);
  let changed = false;
  ops.forEach((op, i) => {
    if (op.kind === " ") return;
    changed = true;
    for (let k = Math.max(0, i - DIFF_CONTEXT); k <= Math.min(ops.length - 1, i + DIFF_CONTEXT); k++) show[k] = true;
  });
  if (!changed) return "";
  let out = "";
  let oldLine = 1;
  let newLine = 1;
  for (let i = 0; i < ops.length;) {
    if (!show[i]) {
      if (ops[i]!.kind !== "+") oldLine++;
      if (ops[i]!.kind !== "-") newLine++;
      i++;
      continue;
    }
    let j = i;
    let oldN = 0;
    let newN = 0;
    while (j < ops.length && show[j]) {
      if (ops[j]!.kind !== "+") oldN++;
      if (ops[j]!.kind !== "-") newN++;
      j++;
    }
    let oldStart = oldLine;
    let newStart = newLine;
    if (oldN === 0) oldStart--;
    if (newN === 0) newStart--;
    out += `@@ -${hunkRange(oldStart, oldN)} +${hunkRange(newStart, newN)} @@\n`;
    for (const op of ops.slice(i, j)) out += op.kind + op.text + "\n";
    oldLine += oldN;
    newLine += newN;
    i = j;
  }
  return out;
}
