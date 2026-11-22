// The normalized conversation of a run, built from the stored run messages.
// Port of internal/conversation. The types have the shape of
// schemas/runner/v1/conversation.json (packages/runner-proto).

import type { Run, RunMessage } from "./run";

export const BLOCK_TEXT = "text";
export const BLOCK_ACTION = "action";
export const BLOCK_ACTION_RESULT = "action_result";

export interface ConversationBlock {
  type: string;
  call_id?: string;
  category?: string;
  input?: unknown;
  is_error?: boolean;
  name?: string;
  state?: string;
  text?: string;
}

export interface ConversationMessage {
  blocks: ConversationBlock[] | null;
  kind?: string;
  message_id?: string;
  role: string;
  sender?: { type: string; id?: string; name?: string };
}

export interface ConversationStep {
  messages: ConversationMessage[] | null;
  session: number;
  step_id: string;
  turn_id?: string;
}

export interface Conversation {
  conversation_id: string;
  steps: ConversationStep[] | null;
}

interface MessageData {
  call_id?: unknown;
  category?: unknown;
  arguments?: unknown;
  is_error?: unknown;
}

function readData(raw: unknown): MessageData {
  let v = raw;
  if (typeof v === "string") {
    try {
      v = JSON.parse(v);
    } catch {
      return {}; // a bad data field only loses the extras
    }
  }
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as MessageData) : {};
}

const str = (v: unknown): string => (typeof v === "string" ? v : "");

/** Turns the messages of a run into the normalized conversation. A user message starts a new step. */
export function buildConversation(
  run: Pick<Run, "id">,
  msgs: readonly Pick<RunMessage, "id" | "role" | "content" | "data">[],
): Conversation {
  const steps: ConversationStep[] = [];
  let step: (ConversationStep & { messages: ConversationMessage[] }) | null = null;
  const newStep = (): ConversationStep & { messages: ConversationMessage[] } => {
    const s = { step_id: "step_" + (steps.length + 1), session: 0, messages: [] as ConversationMessage[] };
    steps.push(s);
    step = s;
    return s;
  };
  const assistant = (id: string): ConversationMessage & { blocks: ConversationBlock[] } => {
    const s = step ?? newStep();
    const last = s.messages[s.messages.length - 1];
    if (last && last.role === "assistant") {
      last.blocks ??= [];
      return last as ConversationMessage & { blocks: ConversationBlock[] };
    }
    const m = { message_id: id, role: "assistant", blocks: [] as ConversationBlock[] };
    s.messages.push(m);
    return m;
  };
  for (const m of msgs) {
    const data = readData(m.data);
    const callId = str(data.call_id);
    const text: ConversationBlock = m.content === "" ? { type: BLOCK_TEXT } : { type: BLOCK_TEXT, text: m.content };
    switch (m.role) {
      case "user":
        newStep().messages.push({ message_id: m.id, role: "user", kind: "user", blocks: [text] });
        break;
      case "assistant":
        assistant(m.id).blocks.push(text);
        break;
      case "tool_call": {
        const b: ConversationBlock = { type: BLOCK_ACTION, state: "completed" };
        if (m.content !== "") b.name = m.content;
        if (callId) b.call_id = callId;
        if (str(data.category)) b.category = str(data.category);
        if (data.arguments !== undefined && data.arguments !== null) b.input = data.arguments;
        assistant(m.id).blocks.push(b);
        break;
      }
      case "tool_result": {
        const b: ConversationBlock = { type: BLOCK_ACTION_RESULT };
        if (m.content !== "") b.text = m.content;
        if (callId) b.call_id = callId;
        if (data.is_error === true) b.is_error = true;
        assistant(m.id).blocks.push(b);
        break;
      }
      case "system":
        (step ?? newStep()).messages.push({ message_id: m.id, role: "system", kind: "system", blocks: [text] });
        break;
    }
  }
  return { conversation_id: run.id, steps };
}
