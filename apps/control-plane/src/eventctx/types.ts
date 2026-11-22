// Types and limits shared by the bundle, the storage and the builders.
import type { Event, SourceRef } from "@openfactory/core";
import { utf8 } from "../lib/bytes";

export const DIR = ".openfactory/events";
export const MAX_PAYLOAD_BYTES = 256 << 10;
export const MAX_CONTEXT_BYTES = 2 << 20;
export const MAX_CONTEXT_FILES = 40;
export const MAX_REVIEW_FILE_BYTES = MAX_PAYLOAD_BYTES;

export interface File {
  path: string;
  data: Uint8Array;
}

export interface Omitted {
  name: string;
  reason: string;
}

export interface Envelope {
  message_id: string;
  provider: string;
  event_types: string[];
  action_key?: string;
  delivery_id?: string;
  actor: { handle?: string; external_id?: string; is_bot?: boolean };
  trust?: string;
  source: SourceRef;
  received_at: string;
  webhook?: { id?: string; name?: string; delivery?: string };
  payload_bytes: number;
  payload_truncated: boolean;
  context_files?: string[];
  omitted?: Omitted[];
}

/** Adds the provider context of an event. Files are relative to context/. */
export interface Builder {
  provider(): string;
  eventContext(ev: Event): Promise<{ files: File[]; omitted?: Omitted[] }>;
}

export interface Bundle {
  message_id: string;
  envelope: Envelope;
  files: File[];
}

/** Go json.MarshalIndent with two spaces (no HTML escaping differences matter here). */
export const indentJSON = (v: unknown): Uint8Array => utf8(JSON.stringify(v, null, 2));
