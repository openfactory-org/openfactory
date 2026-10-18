/** Line diff (LCS) used by the definition version page. */

export type DiffOp = { kind: "same" | "add" | "del"; text: string; oldNo?: number; newNo?: number };

export function diffLines(a: string, b: string): DiffOp[] {
  const x = a === "" ? [] : a.replace(/\n$/, "").split("\n");
  const y = b === "" ? [] : b.replace(/\n$/, "").split("\n");
  const n = x.length;
  const m = y.length;
  // LCS table; definition files are small (capped at 512 KB).
  const dp: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out: DiffOp[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ kind: "same", text: x[i], oldNo: i + 1, newNo: j + 1 });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ kind: "del", text: x[i], oldNo: i + 1 });
      i++;
    } else {
      out.push({ kind: "add", text: y[j], newNo: j + 1 });
      j++;
    }
  }
  for (; i < n; i++) out.push({ kind: "del", text: x[i], oldNo: i + 1 });
  for (; j < m; j++) out.push({ kind: "add", text: y[j], newNo: j + 1 });
  return out;
}

export type FileDiff = {
  path: string;
  status: "added" | "removed" | "modified";
  ops: DiffOp[];
  added: number;
  removed: number;
};

export function diffFiles(oldFiles: Record<string, string> | undefined, newFiles: Record<string, string>): FileDiff[] {
  const before = oldFiles ?? {};
  const paths = Array.from(new Set([...Object.keys(before), ...Object.keys(newFiles)])).sort();
  const out: FileDiff[] = [];
  for (const path of paths) {
    const a = before[path];
    const b = newFiles[path];
    if (a === b) continue;
    const ops = diffLines(a ?? "", b ?? "");
    out.push({
      path,
      status: a === undefined ? "added" : b === undefined ? "removed" : "modified",
      ops,
      added: ops.filter((o) => o.kind === "add").length,
      removed: ops.filter((o) => o.kind === "del").length,
    });
  }
  return out;
}

export type Gap = { kind: "gap"; count: number };

/** Collapses long unchanged runs, keeping `context` lines around changes. */
export function hunks(ops: DiffOp[], context = 3): (DiffOp | Gap)[] {
  const keep = new Array<boolean>(ops.length).fill(false);
  ops.forEach((o, i) => {
    if (o.kind === "same") return;
    for (let k = Math.max(0, i - context); k <= Math.min(ops.length - 1, i + context); k++) keep[k] = true;
  });
  const out: (DiffOp | Gap)[] = [];
  let gap = 0;
  ops.forEach((o, i) => {
    if (keep[i]) {
      if (gap) out.push({ kind: "gap", count: gap });
      gap = 0;
      out.push(o);
    } else gap++;
  });
  if (gap) out.push({ kind: "gap", count: gap });
  return out;
}
