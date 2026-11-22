// Repository content for definitions: the git trees and blobs APIs.
import { pathEscape } from "@openfactory/core";
import { fromB64 } from "../../lib/bytes";
import { repoPath, wrap, type GitHubTransport } from "./rest";

export const MAX_TREE_FILES = 2000;
export const MAX_FILE_BYTES = 1 << 20;

export async function defaultBranch(t: GitHubTransport, owner: string, repo: string): Promise<string> {
  const r = await t.call<{ default_branch?: string }>(owner, repo, "GET", repoPath(owner, repo));
  return r?.default_branch ?? "";
}

/** Reads every blob under path at ref ("" = the default branch). */
export async function getFiles(
  t: GitHubTransport,
  owner: string,
  repo: string,
  ref: string,
  path: string,
): Promise<{ files: Record<string, string>; sha: string }> {
  const r = ref === "" ? await defaultBranch(t, owner, repo) : ref;
  let commit: { sha: string; commit: { tree: { sha: string } } };
  try {
    commit = await t.call(owner, repo, "GET", repoPath(owner, repo) + "/commits/" + pathEscape(r));
  } catch (e) {
    throw wrap(`github: resolve ${r}`, e);
  }
  let tree: { tree: { path: string; type: string; sha: string; size?: number }[]; truncated?: boolean };
  try {
    tree = await t.call(owner, repo, "GET", `${repoPath(owner, repo)}/git/trees/${commit.commit.tree.sha}?recursive=1`);
  } catch (e) {
    throw wrap("github: read tree", e);
  }
  if (tree.truncated) throw new Error("github: repository tree too large (truncated by GitHub)");
  let prefix = path.replace(/^\/+|\/+$/g, "");
  if (prefix !== "") prefix += "/";
  const files: Record<string, string> = {};
  let n = 0;
  for (const e of tree.tree) {
    if (e.type !== "blob" || !e.path.startsWith(prefix)) continue;
    if (n >= MAX_TREE_FILES) throw new Error(`github: more than ${MAX_TREE_FILES} files under "${path}"`);
    if ((e.size ?? 0) > MAX_FILE_BYTES) throw new Error(`github: ${e.path} exceeds ${MAX_FILE_BYTES} bytes`);
    let blob: { content: string; encoding: string };
    try {
      blob = await t.call(owner, repo, "GET", `${repoPath(owner, repo)}/git/blobs/${e.sha}`);
    } catch (err) {
      throw wrap(`github: read ${e.path}`, err);
    }
    let content = blob.content;
    if (blob.encoding === "base64") {
      try {
        content = new TextDecoder().decode(fromB64(blob.content.replace(/\n/g, "")));
      } catch (err) {
        throw new Error(`github: decode ${e.path}: ${(err as Error).message}`, { cause: err });
      }
    }
    files[e.path.slice(prefix.length)] = content;
    n++;
  }
  return { files, sha: commit.sha };
}
