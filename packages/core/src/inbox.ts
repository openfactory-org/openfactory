// Inbox items: approvals, questions, handoffs and failures that wait for a
// person. Port of internal/core (types.go). Field names are the Go JSON names.

import type { Timestamp } from "./common";

export const INBOX_KINDS = [
  "approval",
  "question",
  "handoff",
  "failure",
  "review",
  "spend_limit",
  "blocked",
  "answered",
] as const;
export type InboxKind = (typeof INBOX_KINDS)[number];

/** Question types of an inbox question. */
export const QUESTION_SINGLE_SELECT = "single_select";
export const QUESTION_MULTI_SELECT = "multi_select";
export const QUESTION_TEXT = "text";
export const QUESTION_TYPES = [QUESTION_SINGLE_SELECT, QUESTION_MULTI_SELECT, QUESTION_TEXT] as const;
export type QuestionType = (typeof QUESTION_TYPES)[number];

export interface Question {
  question_id: string;
  text: string;
  type: QuestionType;
  options?: string[];
  recommended_option?: string;
}

/** Tools an inbox item can be resolved from (InboxItem.resolved_via). */
export const INBOX_VIA_DASHBOARD = "dashboard";
export const INBOX_VIA_API = "api";
export const INBOX_VIA_MCP = "mcp";
export const INBOX_VIA_CHAT = "chat";
export const INBOX_VIA_FORGE = "forge";
export const INBOX_VIAS = [INBOX_VIA_DASHBOARD, INBOX_VIA_API, INBOX_VIA_MCP, INBOX_VIA_CHAT, INBOX_VIA_FORGE] as const;
export type InboxVia = (typeof INBOX_VIAS)[number];

export interface InboxItem {
  id: string;
  factory_id: string;
  work_item_id?: string;
  run_id?: string;
  kind: InboxKind;
  title: string;
  body: string;
  options?: string[];
  questions?: Question[];
  assignee_id?: string;
  recipients?: string[];
  read_by?: string[];
  answer_message_id?: string;
  resolved: boolean;
  resolution?: string;
  resolved_by?: string;
  resolved_via?: InboxVia;
  created_at: Timestamp;
  resolved_at?: Timestamp;
  version: number;
}

/** Who resolves an inbox item and from which tool. */
export interface Resolver {
  userId: string;
  via: InboxVia;
}

/** The recipients of an inbox item: recipients, or the old assignee. */
export function inboxRecipientIds(i: Pick<InboxItem, "recipients" | "assignee_id">): string[] {
  if (i.recipients && i.recipients.length > 0) return i.recipients;
  if (i.assignee_id) return [i.assignee_id];
  return [];
}

/** Reports whether the user marked the item as read. */
export function inboxIsReadBy(i: Pick<InboxItem, "read_by">, userId: string): boolean {
  return (i.read_by ?? []).includes(userId);
}
