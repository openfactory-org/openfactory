// Shared helpers of the chat and tracker integrations (port of integrations.go).
import type { Integration, User } from "@openfactory/core";
import type { IntegrationStore, InboxItemRef, LinkerLike, SecretReader, WorkSide } from "./types";
import { integrationSecret } from "./util";
import { OfError } from "@openfactory/core";

export function providerLabel(p: string): string {
  switch (p) {
    case "slack":
      return "Slack";
    case "teams":
    case "microsoft_teams":
      return "Microsoft Teams";
    case "linear":
      return "Linear";
    case "jira":
      return "Jira";
    case "github":
      return "GitHub";
  }
  return p;
}

/** The user linked to an external account, when the user belongs to teamId. Null otherwise. */
export async function member(
  store: IntegrationStore | undefined,
  teamId: string,
  provider: string,
  externalId: string,
): Promise<User | null> {
  if (!store || externalId === "") return null;
  const x = await store.findExternalIdentity(provider, externalId);
  if (!x || x.user_id === "") return null;
  const u = await store.getUser(x.user_id);
  if (!u || u.team_id !== teamId) return null;
  return u;
}

export const SETTING_AUTO_RESPOND_TO_THREAD_REPLIES = "autoRespondToThreadReplies";

/** A factory integration setting for provider (definition `type`), or "". */
export async function factorySetting(
  store: IntegrationStore | undefined,
  factoryId: string,
  provider: string,
  key: string,
): Promise<string> {
  if (!store || factoryId === "") return "";
  let m: Record<string, Record<string, string>>;
  try {
    m = await store.getFactoryIntegrations(factoryId);
  } catch {
    return "";
  }
  for (const p of [provider, provider.replaceAll("-", "_"), provider.replaceAll("_", "-")]) {
    const s = m[p];
    if (s) return s[key] ?? "";
  }
  return "";
}

/** Reports whether plain thread replies continue work (default true). */
export async function autoRespondToThreadReplies(
  store: IntegrationStore | undefined,
  factoryId: string,
  provider: string,
): Promise<boolean> {
  const v = (await factorySetting(store, factoryId, provider, SETTING_AUTO_RESPOND_TO_THREAD_REPLIES)).trim().toLowerCase();
  return v !== "false" && v !== "0" && v !== "no";
}

/** The newest unresolved inbox item of a work item. Throws resource_not_found when there is none. */
export async function openInboxItem(work: WorkSide | undefined, workItemId: string): Promise<InboxItemRef> {
  const it = await work?.openInboxItem?.(workItemId);
  if (!it) throw new OfError("resource_not_found", "inbox item not found");
  return it;
}

/** The text of a link prompt in a public place. Works without a linker. */
export function dashboardHint(linker: LinkerLike | undefined, provider: string): string {
  if (linker) return linker.dashboardHint(provider);
  return `Sign in to the Open Factory dashboard and link your ${providerLabel(provider)} account under "Link your account".`;
}

export async function privateUrl(
  linker: LinkerLike | undefined,
  teamId: string,
  provider: string,
  externalId: string,
  handle: string,
): Promise<string> {
  return linker ? linker.privateUrl(teamId, provider, externalId, handle) : "";
}

/** Reads a secret for a record (the token, or the named config key). */
export function secretOf(secrets: SecretReader | undefined, i: Integration, configKey = ""): Promise<string> {
  return integrationSecret(secrets, i, configKey);
}
