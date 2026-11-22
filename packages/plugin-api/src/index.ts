// @openfactory/plugin-api: plugin Worker RPC interfaces (types.ts)
// and helpers for plugin authors and for the host.
//
// Worker-only code is in "@openfactory/plugin-api/worker" (needs the Workers runtime).
// Test helpers are in "@openfactory/plugin-api/testing".
import { EVENT_PROVIDER as PROVIDERS } from "@openfactory/core";
import { splitRepo as coreSplitRepo } from "@openfactory/core";

export * from "./types";
export * from "./errors";
export * from "./http";
// Event attribute names and text helpers live in @openfactory/core; plugins get them here.
export {
  EVENT_ATTR,
  EVENT_PROVIDER,
  difference,
  extractMentions,
  formatOutbound,
  hasMention,
  stringSet,
  stripCode,
  truncate,
} from "@openfactory/core";

/**
 * Splits "group/sub/repo" at the last slash into its owner and name. The
 * object shape is part of the plugin API; core's splitRepo returns a tuple.
 */
export function splitRepo(full: string): { owner: string; name: string } {
  const [owner, name] = coreSplitRepo(full);
  return { owner, name };
}

/** @deprecated Use EVENT_PROVIDER.GitLab. */
export const PROVIDER_GITLAB = PROVIDERS.GitLab;
/** @deprecated Use EVENT_PROVIDER.AzureDevOps. */
export const PROVIDER_AZURE_DEVOPS = PROVIDERS.AzureDevOps;
export * from "./version";
export * from "./webhook";
