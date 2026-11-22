// The pull request of a link_pull_request call (linkPR and ParsePullRequestURL
// in internal/orchestration/server.go): a URL, or a repo and a number.
import {
  isOneOf,
  OfError,
  PULL_REQUEST_STATES,
  pullRequestSource,
  sourceKey,
  type PullRequestRef,
  type SourceRef,
} from "@openfactory/core";

export interface LinkPullRequestInput {
  url?: string;
  repo?: string;
  number?: number;
  branch?: string;
  forge?: string;
  state?: string;
}

/** Reads forge, repo and number from a GitHub, GitLab or Azure DevOps pull request URL. */
export function parsePullRequestUrl(raw: string): { forge: string; repo: string; number: number } | undefined {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return undefined;
  }
  if (!u.host) return undefined;
  const parts = u.pathname.replace(/^\/+|\/+$/g, "").split("/");
  for (let i = parts.length - 2; i >= 1; i--) {
    const s = parts[i + 1]!;
    if (!/^[+-]?[0-9]+$/.test(s)) continue;
    const n = Number(s);
    switch (parts[i]) {
      case "pull":
      case "pulls":
        return i >= 2 ? { forge: "GITHUB", repo: parts.slice(0, i).join("/"), number: n } : undefined;
      case "merge_requests": {
        let end = i;
        if (end > 0 && parts[end - 1] === "-") end--;
        return end >= 2 ? { forge: "GITLAB", repo: parts.slice(0, end).join("/"), number: n } : undefined;
      }
      case "pullrequest":
        // {org}/{project}/_git/{repo}/pullrequest/{n}
        if (i >= 2 && parts[i - 2] === "_git")
          return { forge: "AZURE_DEVOPS", repo: [...parts.slice(0, i - 2), parts[i - 1]!].join("/"), number: n };
        return { forge: "AZURE_DEVOPS", repo: parts.slice(0, i).join("/"), number: n };
    }
  }
  return undefined;
}

/** prRef: the pull request of a link_pull_request call. Throws invalid_request. */
export function prRef(input: LinkPullRequestInput): PullRequestRef {
  const str = (v: unknown): string => (typeof v === "string" ? v : "");
  const state = str(input.state).toLowerCase();
  if (!isOneOf(PULL_REQUEST_STATES, state))
    throw new OfError("invalid_request", `unknown state ${JSON.stringify(input.state)}`);
  const pr: PullRequestRef = {
    forge: str(input.forge).toUpperCase(),
    repo: str(input.repo),
    number: typeof input.number === "number" && Number.isInteger(input.number) ? input.number : 0,
    url: str(input.url),
    branch: str(input.branch),
    state,
  };
  const parsed = parsePullRequestUrl(pr.url);
  if (parsed) {
    if (!pr.forge) pr.forge = parsed.forge;
    if (!pr.repo) pr.repo = parsed.repo;
    if (!pr.number) pr.number = parsed.number;
  }
  if (!pr.url && (!pr.repo || !pr.number)) throw new OfError("invalid_request", "url (or repo and number) is required");
  return pr;
}

/**
 * The normalized reference of a linked pull request (core.PullRequestSource),
 * or undefined when the pull request has no repo and number (Go adds no
 * reference and no alias then).
 */
export function prReference(pr: PullRequestRef): { key: string; source: SourceRef } | undefined {
  if (!pr.repo || !pr.number) return undefined;
  const source = pullRequestSource(pr);
  return { key: sourceKey(source), source };
}
