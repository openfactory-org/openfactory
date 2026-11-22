import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

/** The repository root. */
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");

export function repoPath(rel: string): string {
  return resolve(REPO_ROOT, rel);
}
