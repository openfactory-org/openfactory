// Dispatch records, event topics, outbox partition keys, and the workers and
// environments that runs execute on. Port of internal/core (dispatch.go,
// coordination.go, types.go). Field names are the Go JSON names.

import type { Timestamp } from "./common";
import { type Event, type OutboundMessage, sourceKey } from "./workitem";

/** One dispatch decision: one action given to one target ("auto:<id>", "wi:<id>", "mention:<factory>"). */
export interface DispatchRecord {
  action_key: string;
  target: string;
  ref?: string;
  created_at: Timestamp;
}

/** How long dispatch records are kept, in milliseconds. */
export const DISPATCH_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

/** Topics. The durable ones go through an outbox. */
export const TOPIC_INBOUND_EVENT = "event.inbound";
export const TOPIC_RUN_UPDATED = "run.updated";
export const TOPIC_RUN_MESSAGE = "run.message";
export const TOPIC_WORK_ITEM = "workitem.updated";
export const TOPIC_INBOX = "inbox.updated";
export const TOPIC_DEFINITION = "definition.applied";
export const TOPIC_OUTBOUND_NOTICE = "notice.outbound";
export const DURABLE_TOPICS: readonly string[] = [TOPIC_INBOUND_EVENT, TOPIC_OUTBOUND_NOTICE];

export const OUTBOX_STATES = ["pending", "delivered", "dead"] as const;
export type OutboxState = (typeof OUTBOX_STATES)[number];

// ---------- Workers and environments ----------

export interface Worker {
  id: string;
  team_id: string;
  name: string;
  backend: string;
  labels?: Record<string, string>;
  capacity: number;
  active: number;
  last_heartbeat: Timestamp;
  version?: string;
}

/** A reusable workspace (repositories and setup) that agents run in. */
export interface Environment {
  id: string;
  team_id: string;
  name: string;
  description?: string;
  /** forge:owner/name */
  repos: string[] | null;
  image?: string;
  setup_commands?: string[];
  env_vars?: Record<string, string>;
}

// ---------- Outbox partition keys ----------

/** Reports whether a topic is delivered through the outbox. */
export function isDurableTopic(topic: string): boolean {
  return DURABLE_TOPICS.includes(topic);
}

/** The partition key of an inbound event: team and source key. */
export function eventPartitionKey(e: Pick<Event, "team_id" | "source"> | null | undefined): string {
  if (!e) return "";
  const k = sourceKey(e.source);
  return k === "" ? "" : `team:${e.team_id}|${k}`;
}

/** The partition key of an outbound notice: its work item, else its source. */
export function noticePartitionKey(m: Pick<OutboundMessage, "work_item_id" | "source"> | null | undefined): string {
  if (!m) return "";
  if (m.work_item_id) return "wi:" + m.work_item_id;
  const k = sourceKey(m.source);
  return k === "" ? "" : "source:" + k;
}

/**
 * The outbox partition key of a payload, prefixed with the topic. kind names
 * the payload: an inbound event, an outbound notice, or an explicit key.
 */
export function outboxPartitionKey(
  topic: string,
  payload:
    | { kind: "event"; event: Pick<Event, "team_id" | "source"> }
    | { kind: "notice"; notice: Pick<OutboundMessage, "work_item_id" | "source"> }
    | { kind: "key"; key: string }
    | { kind: "none" },
): string {
  let key = "";
  switch (payload.kind) {
    case "event":
      key = eventPartitionKey(payload.event);
      break;
    case "notice":
      key = noticePartitionKey(payload.notice);
      break;
    case "key":
      key = payload.key;
      break;
  }
  return key === "" ? "" : topic + "|" + key;
}
