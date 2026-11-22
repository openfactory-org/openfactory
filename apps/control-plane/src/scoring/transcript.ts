// Transcripts for judges: port of BuildTranscript in scorer.go.
import type { Run } from "@openfactory/core";
import { MAX_DESCENDANTS } from "./types";
import type { RunsPort } from "./types";
import { capMiddle, truncate } from "./text";

export const MAX_MESSAGE_CHARS = 4000;

type TranscriptRun = Pick<Run, "id" | "state" | "prompt" | "created_at"> &
  Partial<Pick<Run, "agent_name" | "name" | "parent_run_id" | "result" | "error">>;

function writeRun(
  out: string[],
  run: TranscriptRun,
  depth: number,
  perMessage: number,
  messages: readonly { role: string; content: string }[],
): void {
  const cut = (s: string): string => (perMessage <= 0 ? s : truncate(s, MAX_MESSAGE_CHARS));
  let head = `=== ${depth > 0 ? "Child run" : "Run"} ${run.id}`;
  if (run.agent_name) {
    head += ` (agent: ${run.agent_name}`;
    if (run.name && run.name !== run.agent_name) head += `, name: ${run.name}`;
    head += ")";
  }
  if (run.parent_run_id) head += ` parent: ${run.parent_run_id}`;
  head += ` state: ${run.state} ===\n`;
  out.push(head);
  if (messages.length === 0 && run.prompt) out.push(`[user]\n${cut(run.prompt)}\n\n`);
  for (const m of messages) out.push(`[${m.role}]\n${cut(m.content)}\n\n`);
  if (run.result) out.push(`[final result]\n${cut(run.result)}\n\n`);
  if (run.error) out.push(`[error]\n${cut(run.error)}\n\n`);
}

/**
 * Renders a run's conversation as plain text for a judge. With
 * includeDescendants every descendant run (breadth first) follows as extra
 * evidence. The result is capped at `total` bytes, keeping the start and the end.
 * `perMessage` > 0 caps each message at MAX_MESSAGE_CHARS bytes.
 */
export async function buildTranscript(
  runs: Pick<RunsPort, "listChildren" | "listMessages">,
  run: TranscriptRun,
  includeDescendants: boolean,
  perMessage: number,
  total: number,
): Promise<string> {
  const out: string[] = [];
  writeRun(out, run, 0, perMessage, await runs.listMessages(run.id));
  if (includeDescendants) {
    const queue: string[] = [run.id];
    const seen = new Set<string>([run.id]);
    let count = 0;
    while (queue.length > 0 && count < MAX_DESCENDANTS) {
      const parent = queue.shift()!;
      const children = [...(await runs.listChildren(parent))].sort((a, b) => {
        const ta = Date.parse(a.created_at);
        const tb = Date.parse(b.created_at);
        return ta !== tb ? ta - tb : a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
      });
      for (const c of children) {
        if (seen.has(c.id)) continue;
        seen.add(c.id);
        count++;
        if (count > MAX_DESCENDANTS) break;
        writeRun(out, c, c.parent_run_id === run.id ? 1 : 2, perMessage, await runs.listMessages(c.id));
        queue.push(c.id);
      }
    }
  }
  return capMiddle(out.join(""), total);
}
