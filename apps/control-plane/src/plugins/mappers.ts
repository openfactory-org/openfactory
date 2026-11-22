// Maps between the camelCase plugin API (packages/plugin-api) and the snake_case
// core wire shapes of the control plane (packages/core, contracts/do-messages).

import type { Integration, OutboundMessage, PullRequestRef, SourceRef } from "@openfactory/core";
import type {
  IntegrationRecord,
  OutboundMessage as PluginOutbound,
  PluginEvent,
  PullRequestRef as PluginPullRequest,
  SourceRef as PluginSource,
} from "@openfactory/plugin-api";
import type { CoreEvent } from "../contracts/do-messages";

export function sourceToPlugin(s: SourceRef): PluginSource {
  const out: PluginSource = { provider: s.provider, kind: s.kind };
  if (s.url) out.url = s.url;
  if (s.repo) out.repo = s.repo;
  if (s.number) out.number = s.number;
  if (s.channel) out.channel = s.channel;
  if (s.thread_id) out.threadId = s.thread_id;
  if (s.external_id) out.externalId = s.external_id;
  if (s.title) out.title = s.title;
  if (s.thread_ref) out.threadRef = s.thread_ref;
  return out;
}

export function sourceFromPlugin(s: PluginSource): SourceRef {
  const out: SourceRef = { provider: s.provider, kind: s.kind };
  if (s.url) out.url = s.url;
  if (s.repo) out.repo = s.repo;
  if (s.number) out.number = s.number;
  if (s.channel) out.channel = s.channel;
  if (s.threadId) out.thread_id = s.threadId;
  if (s.externalId) out.external_id = s.externalId;
  if (s.title) out.title = s.title;
  if (s.threadRef) out.thread_ref = s.threadRef;
  return out;
}

export function pullRequestToPlugin(p: PullRequestRef): PluginPullRequest {
  const out: PluginPullRequest = { forge: p.forge, repo: p.repo, number: p.number };
  if (p.url) out.url = p.url;
  if (p.branch) out.branch = p.branch;
  if (p.state) out.state = p.state as PluginPullRequest["state"];
  return out;
}

export function pullRequestFromPlugin(p: PluginPullRequest): PullRequestRef {
  return { forge: p.forge, repo: p.repo, number: p.number, url: p.url ?? "", branch: p.branch ?? "", state: p.state ?? "" };
}

export function outboundToPlugin(m: OutboundMessage): PluginOutbound {
  const out: PluginOutbound = {
    workItemId: m.work_item_id,
    source: sourceToPlugin(m.source),
    text: m.text,
    kind: m.kind as PluginOutbound["kind"],
  };
  if (m.options) out.options = m.options;
  if (m.inbox_id) out.inboxId = m.inbox_id;
  if (m.factory_id) out.factoryId = m.factory_id;
  if (m.thread_ref) out.threadRef = m.thread_ref;
  if (m.issue_state) out.issueState = m.issue_state as PluginOutbound["issueState"];
  if (m.pull_request) out.pullRequest = pullRequestToPlugin(m.pull_request);
  return out;
}

export function integrationToRecord(i: Integration): IntegrationRecord {
  const out: IntegrationRecord = {
    id: i.id,
    teamId: i.team_id,
    provider: i.provider,
    name: i.name,
    config: { ...(i.config ?? {}) },
  };
  if (i.secret_ref) out.secretRef = i.secret_ref;
  if (i.factory_id) out.factoryId = i.factory_id;
  return out;
}

/** The dedupe key of an inbound plugin event: the delivery when there is one, else the event. */
export function eventDedupeKey(ev: PluginEvent): string {
  return ev.deliveryId ? `${ev.provider}:${ev.deliveryId}` : (ev.id ?? "");
}

/** Converts a plugin event to the core event that IngressDO stores. */
export function eventToCore(ev: PluginEvent, id: string, now: () => Date = () => new Date()): CoreEvent {
  const out: CoreEvent = {
    id,
    team_id: ev.teamId ?? "",
    provider: ev.provider,
    type: ev.type,
    actor: {
      provider: ev.actor?.provider ?? ev.provider,
      external_id: ev.actor?.externalId ?? "",
      handle: ev.actor?.handle ?? "",
      is_bot: ev.actor?.isBot ?? false,
      ...(ev.actor?.userId ? { user_id: ev.actor.userId } : {}),
    },
    source: ev.source ? { ...sourceFromPlugin(ev.source) } : { provider: ev.provider, kind: "" },
    attributes: { ...(ev.attributes ?? {}) },
    received_at: ev.receivedAt ?? now().toISOString(),
  };
  if (ev.factoryId) out.factory_id = ev.factoryId;
  if (ev.deliveryId) out.delivery_id = ev.deliveryId;
  if (ev.actionKey) out.action_key = ev.actionKey;
  if (ev.payload !== undefined) out.payload = ev.payload;
  if (ev.text) out.text = ev.text;
  return out;
}
