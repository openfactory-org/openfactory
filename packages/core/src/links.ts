// Dashboard URLs. Port of internal/links: the single source of truth for the
// paths that the dashboard routes, so links in API answers, MCP tool results,
// scoring and chat or forge messages resolve to a dashboard page.
//
// Every function takes the public base URL (OF_PUBLIC_URL). A trailing slash
// is ignored, and an empty base gives a root-relative path.

import { encodeQuery, pathEscape } from "./gostd";

function join(base: string, ...parts: string[]): string {
  let out = base.replace(/\/+$/, "");
  for (const p of parts) out += "/" + p;
  return out;
}

const esc = pathEscape;

/** A factory's dashboard: /f/{fid}. */
export function factoryLink(base: string, factoryId: string): string {
  return join(base, "f", esc(factoryId));
}

/** A factory's settings page: /f/{fid}/settings. */
export function factorySettingsLink(base: string, factoryId: string): string {
  return join(base, "f", esc(factoryId), "settings");
}

/** A factory's definition page: /f/{fid}/definition. */
export function factoryDefinitionLink(base: string, factoryId: string): string {
  return join(base, "f", esc(factoryId), "definition");
}

/** A custom webhook's page: /f/{fid}/webhooks/{id}. */
export function factoryWebhookLink(base: string, factoryId: string, webhookId: string): string {
  return join(base, "f", esc(factoryId), "webhooks", esc(webhookId));
}

/** A work item: /f/{fid}/work/{wid}. */
export function workItemLink(base: string, factoryId: string, workItemId: string): string {
  return join(base, "f", esc(factoryId), "work", esc(workItemId));
}

/** A run transcript: /runs/{id}. */
export function runLink(base: string, runId: string): string {
  return join(base, "runs", esc(runId));
}

/** The team inbox: /inbox. */
export function inboxLink(base: string): string {
  return join(base, "inbox");
}

/** The integrations page: /integrations, with ?connection= and ?provider= when set. */
export function integrationsLink(base: string, provider = "", connection = ""): string {
  const u = join(base, "integrations");
  const q: Record<string, string> = {};
  if (provider !== "") q["provider"] = provider;
  if (connection !== "") q["connection"] = connection;
  return Object.keys(q).length > 0 ? u + "?" + encodeQuery(q) : u;
}

/** The link functions under their Go names (links.Factory, links.Run, ...). */
export const links = {
  factory: factoryLink,
  factorySettings: factorySettingsLink,
  factoryDefinition: factoryDefinitionLink,
  factoryWebhook: factoryWebhookLink,
  workItem: workItemLink,
  run: runLink,
  inbox: inboxLink,
  integrations: integrationsLink,
} as const;
