// Pure parts of internal/dispatch/deliver.go and send.go.
import type { InputItem } from "@openfactory/runner-proto";
import type { RunInput, RunSession, RunState, SendOutcome } from "./types";

export type Capability = "resume" | "mid_turn_input" | "interrupt" | "needs_input_signal";

/** messageStartsTurn: system messages never start a turn; lifecycle "started" does not either. */
export function messageStartsTurn(m: Pick<RunInput, "kind" | "data">): boolean {
  if (m.kind === "system") return false;
  if (m.kind === "lifecycle") {
    const d = m.data as { event?: unknown } | undefined;
    if (d && typeof d === "object" && d.event === "started") return false;
  }
  return true;
}

export function startsTurn(msgs: readonly Pick<RunInput, "kind" | "data">[]): boolean {
  return msgs.some(messageStartsTurn);
}

export function hasCapability(s: Pick<RunSession, "capabilities">, c: Capability): boolean {
  return (s.capabilities ?? []).includes(c);
}

/** outcomeFor (send.go). */
export function outcomeFor(state: RunState, latest: Pick<RunSession, "state"> | undefined): SendOutcome {
  if (latest && latest.state === "active") return "live_session";
  switch (state) {
    case "queued":
    case "pending":
    case "claimed":
    case "running":
      return "queued";
  }
  return "new_session";
}

export interface AttachmentMeta {
  name: string;
  content_type: string;
  bytes: number;
}

/** toItem: a queued message as an input item for the runner. */
export function toItem(m: RunInput, lookup?: (id: string) => AttachmentMeta | undefined): InputItem {
  const item: InputItem = {
    message_id: m.message_id,
    kind: m.kind as InputItem["kind"],
    sender: {
      type: m.sender.type,
      ...(m.sender.id ? { id: m.sender.id } : {}),
      ...(m.sender.name ? { name: m.sender.name } : {}),
    },
    content: m.content,
    attachments: [],
  };
  if (m.data !== undefined && m.data !== null) item.data = m.data;
  if (m.untrusted) item.untrusted = true;
  for (const id of m.attachments ?? []) {
    const found = lookup?.(id);
    item.attachments!.push(
      found
        ? { id, name: found.name, content_type: found.content_type, bytes: found.bytes }
        : { id, name: "", content_type: "", bytes: 0 },
    );
  }
  return item;
}

/** What deliver decides for a session with pending messages. */
export type DeliverDecision = { kind: "none" } | { kind: "start_turn" } | { kind: "mid_turn" };

export function decideDelivery(
  s: Pick<RunSession, "state" | "ready_at" | "active_turn_id" | "capabilities"> | undefined,
  pending: readonly Pick<RunInput, "kind" | "data">[],
): DeliverDecision {
  if (!s || s.state !== "active" || !s.ready_at || pending.length === 0) return { kind: "none" };
  if (!s.active_turn_id && startsTurn(pending)) return { kind: "start_turn" };
  if (s.active_turn_id && hasCapability(s, "mid_turn_input")) return { kind: "mid_turn" };
  return { kind: "none" };
}
