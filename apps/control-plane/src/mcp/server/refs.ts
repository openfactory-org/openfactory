// Pure helpers of the Factory MCP server (port of internal/mcpserver/resolve.go):
// reference parsing and the local setup guidance of start_working.
import {
  clipBytes,
  utf8,
  type PullRequestRef as CorePullRequestRef,
  type SourceRef,
  type WorkItem,
} from "@openfactory/core";

/** A pull request a reference names; url, branch and state may be unknown. */
export type PullRequestRef = Pick<CorePullRequestRef, "forge" | "repo" | "number"> &
  Partial<Pick<CorePullRequestRef, "url" | "branch" | "state">>;

export const ISSUE_KEY_RE = /^[A-Z][A-Z0-9]+-\d+$/;
export const SHORT_REF_RE = /^([\w.-]+\/[\w.-]+)#(\d+)$/;
export const SLACK_PATH_RE = /^\/archives\/([A-Z0-9]+)\/p(\d{10})(\d{6})$/;

const isInt = (s: string | undefined) => s !== undefined && /^[+-]?\d+$/.test(s);

function splitPath(path: string): string[] {
  return path.replace(/^\/+|\/+$/g, "").split("/");
}

/** Recognizes GitHub pull requests, GitLab merge requests and Azure DevOps pull requests. */
export function parsePullRequestURL(raw: string): PullRequestRef | null {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return null;
  }
  if (u.host === "") return null;
  const path = decodeURIComponent(u.pathname);
  const segs = splitPath(path);
  const host = u.host.toLowerCase();
  const url = `${u.protocol}//${u.host}${path}`.replace(/\/+$/, "");
  const base = { url, state: "open" as const };
  if (host.includes("dev.azure.com") || host.includes("visualstudio.com")) {
    // /{org}/{project}/_git/{repo}/pullrequest/{n}
    for (let i = 0; i + 1 < segs.length; i++) {
      if (segs[i] === "pullrequest" && i >= 2 && segs[i - 2] === "_git") {
        if (!isInt(segs[i + 1])) return null;
        return {
          ...base,
          forge: "AZURE_DEVOPS",
          repo: segs.slice(0, i - 2).join("/") + "/" + segs[i - 1],
          number: Number(segs[i + 1]),
        };
      }
    }
    return null;
  }
  const n = segs.length;
  if (n >= 4 && segs[n - 2] === "pull") {
    if (!isInt(segs[n - 1])) return null;
    return { ...base, forge: "GITHUB", repo: segs.slice(0, n - 2).join("/"), number: Number(segs[n - 1]) };
  }
  if (n >= 5 && segs[n - 3] === "-" && segs[n - 2] === "merge_requests") {
    if (!isInt(segs[n - 1])) return null;
    return { ...base, forge: "GITLAB", repo: segs.slice(0, n - 3).join("/"), number: Number(segs[n - 1]) };
  }
  return null;
}

export function parseIssueURL(u: URL): { forge: string; repo: string; number: number } | null {
  const segs = splitPath(decodeURIComponent(u.pathname));
  const n = segs.length;
  if (n >= 4 && segs[n - 2] === "issues") {
    if (!isInt(segs[n - 1])) return null;
    const num = Number(segs[n - 1]);
    if (n >= 5 && segs[n - 3] === "-") return { forge: "gitlab", repo: segs.slice(0, n - 3).join("/"), number: num };
    return { forge: "github", repo: segs.slice(0, n - 2).join("/"), number: num };
  }
  return null;
}

export type RefPlan =
  | { kind: "notFound" }
  | { kind: "dashboard"; workItems: string[]; runs: string[] }
  | { kind: "search"; keys: string[]; prURL: string; branch: string };

/**
 * What a reference points at (the part of resolveWorkItem after the work
 * item ID and run ID checks): dashboard URL segments, source keys, a pull
 * request URL, or a branch name.
 */
export function planReference(ref: string, publicURL: string): RefPlan {
  const keys: string[] = [];
  let prURL = "";
  let u: URL | null;
  try {
    u = new URL(ref);
  } catch {
    u = null;
  }
  if (u && u.protocol !== "" && u.host !== "" && /^[a-z][a-z0-9+.-]*:\/\//i.test(ref)) {
    const path = decodeURIComponent(u.pathname).replace(/\/+$/, "");
    const host = u.host.toLowerCase();
    const segs = splitPath(path);
    if (publicURL !== "" && ref.startsWith(publicURL)) {
      const workItems: string[] = [];
      const runs: string[] = [];
      for (let i = 0; i + 1 < segs.length; i++) {
        if (segs[i] === "work" || segs[i] === "work-items") workItems.push(segs[i + 1]!);
        if (segs[i] === "runs") runs.push(segs[i + 1]!);
      }
      return { kind: "dashboard", workItems, runs };
    }
    if (host.endsWith("slack.com")) {
      const m = SLACK_PATH_RE.exec(path);
      if (m) {
        let ts = `${m[2]}.${m[3]}`;
        const tts = u.searchParams.get("thread_ts");
        if (tts) ts = tts;
        keys.push(`slack:${m[1]}:${ts}`);
      }
    } else if (host === "linear.app") {
      for (let i = 0; i + 1 < segs.length; i++) if (segs[i] === "issue") keys.push("linear:" + segs[i + 1]);
    } else if (path.includes("/browse/")) {
      keys.push("jira:" + segs[segs.length - 1]);
    } else {
      const pr = parsePullRequestURL(ref);
      if (pr) {
        prURL = pr.url ?? "";
        keys.push(`${pr.forge.toLowerCase()}:${pr.repo}#${pr.number}`);
      } else {
        const is = parseIssueURL(u);
        if (is) keys.push(`${is.forge}:${is.repo}#${is.number}`);
      }
    }
    if (keys.length === 0 && prURL === "") return { kind: "notFound" };
    return { kind: "search", keys, prURL, branch: "" };
  }
  const m = SHORT_REF_RE.exec(ref);
  if (m) keys.push(`github:${m[1]}#${m[2]}`, `gitlab:${m[1]}#${m[2]}`);
  else if (ISSUE_KEY_RE.test(ref)) keys.push("linear:" + ref, "jira:" + ref);
  return { kind: "search", keys, prURL, branch: keys.length === 0 ? ref : "" };
}

/** The fields of a work item that matchesWorkItem reads. */
export type WorkItemLike = Pick<WorkItem, "id"> &
  Partial<Pick<WorkItem, "source_key" | "updated_at">> & {
    source?: Partial<SourceRef>;
    pull_requests?: PullRequestRef[];
  };

/** SourceRef.Key of the Go core (fallback when source_key is empty). */
function sourceKey(wi: WorkItemLike): string {
  return wi.source_key ?? "";
}

export function matchesWorkItem(wi: WorkItemLike, keys: string[], prURL: string, branch: string): boolean {
  const sk = sourceKey(wi);
  for (const k of keys) if (sk.toLowerCase() === k.toLowerCase()) return true;
  for (const pr of wi.pull_requests ?? []) {
    if (prURL !== "" && (pr.url ?? "").replace(/\/+$/, "").toLowerCase() === prURL.replace(/\/+$/, "").toLowerCase())
      return true;
    for (const k of keys) {
      if (`${pr.forge.toLowerCase()}:${pr.repo}#${pr.number}`.toLowerCase() === k.toLowerCase()) return true;
    }
    if (branch !== "" && pr.branch === branch) return true;
  }
  return false;
}

// ---------- start working ----------

export interface RepoSetup {
  repository: string;
  clone_url: string;
  branch: string;
  base_branch?: string;
  worktree: string;
  commands: string[];
  existing_branch: boolean;
}

export function cloneURL(forge: string, owner: string, name: string): string {
  switch (forge.toUpperCase()) {
    case "GITLAB":
      return `https://gitlab.com/${owner}/${name}.git`;
    case "AZURE_DEVOPS": {
      const i = owner.indexOf("/");
      const org = i < 0 ? owner : owner.slice(0, i);
      const project = i < 0 || owner.slice(i + 1) === "" ? org : owner.slice(i + 1);
      return `https://dev.azure.com/${org}/${project}/_git/${name}`;
    }
    default:
      return `https://github.com/${owner}/${name}.git`;
  }
}

export function slug(s: string): string {
  let out = s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (out.length > 40) out = out.slice(0, 40).replace(/^-+|-+$/g, "");
  return out;
}

export function shortId(id: string): string {
  const i = id.lastIndexOf("_");
  let s = i >= 0 ? id.slice(i + 1) : id;
  if (s.length > 8) s = s.slice(0, 8);
  return s;
}

export function repoSetupFor(
  forge: string,
  owner: string,
  name: string,
  branch: string,
  base: string,
  sid: string,
  existing: boolean,
): RepoSetup {
  const u = cloneURL(forge, owner, name);
  const wt = `../${name}-${sid}`;
  const commands = [`git clone ${u} ${name}  # skip if you already have a clone`, `cd ${name}`, "git fetch origin"];
  if (existing) commands.push(`git worktree add -B ${branch} ${wt} origin/${branch}`);
  else commands.push(`git worktree add -b ${branch} ${wt} origin/HEAD  # or origin/${base}`);
  commands.push(`cd ${wt}`);
  return {
    repository: `${owner}/${name}`,
    clone_url: u,
    branch,
    ...(existing ? {} : base ? { base_branch: base } : {}),
    worktree: wt,
    commands,
    existing_branch: existing,
  };
}

/**
 * Go truncate: at most n bytes plus a marker. Go cuts at byte n, so a
 * character split in half becomes U+FFFD in the JSON answer; this cut drops the
 * partial character instead.
 */
export function truncate(s: string, n: number): string {
  if (utf8(s).length <= n) return s;
  return clipBytes(s, n) + "… (truncated)";
}

export function firstLine(s: string): string {
  for (const raw of s.split("\n")) {
    let l = raw.replace(/^[# ]+/, "").trim();
    if (l !== "") {
      if (l.length > 80) l = l.slice(0, 79) + "…";
      return l;
    }
  }
  return "Work item from a coding agent";
}

export function workRefs(branch: string, prURL: string): string {
  const parts: string[] = [];
  if (branch) parts.push("Branch: `" + branch + "`");
  if (prURL) parts.push("Pull request: " + prURL);
  return parts.join("\n");
}
