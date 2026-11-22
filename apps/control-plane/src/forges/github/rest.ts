// What the GitHub domain modules (checks, content, reviews) need of the
// client, and the small helpers they share.
import { OfError, pathEscape } from "@openfactory/core";

/** Authenticated calls for one repository, as GitHubClient makes them. */
export interface GitHubTransport {
  call<T = unknown>(owner: string, repo: string, method: string, path: string, body?: unknown): Promise<T>;
  graphql<T>(owner: string, repo: string, query: string, variables: Record<string, unknown>): Promise<T>;
  /** True for GitHub App authentication, false for a personal token. */
  isApp(): boolean;
  /** The numeric App ID; 0 for a personal token. */
  appId(): number;
  now(): number;
}

export function repoPath(owner: string, repo: string): string {
  return "/repos/" + pathEscape(owner) + "/" + pathEscape(repo);
}

/** Prefixes the message of e and keeps an OfError code and reason. */
export function wrap(msg: string, e: unknown): Error {
  if (e instanceof OfError)
    return new OfError(e.code, `${msg}: ${e.detail}`, { cause: e, ...(e.reason ? { reason: e.reason } : {}) });
  return new Error(`${msg}: ${(e as Error).message}`, { cause: e });
}

/** The numeric review comment ID of a thread reference. */
export function commentId(threadRef: string): number {
  const t = threadRef.trim();
  const id = /^\d+$/.test(t) ? Number(t) : NaN;
  if (!(id > 0))
    throw new OfError("invalid_request", `github: thread reference "${threadRef}" is not a review comment ID`);
  return id;
}
