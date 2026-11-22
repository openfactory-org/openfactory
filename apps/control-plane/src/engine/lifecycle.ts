// Port of internal/engine/lifecycle.go and the turn-end decision of
// internal/engine/runs.go (turnEndMessage, TurnMarker). Pure.
import type { Run, RunState } from "./types";

/** The most bytes of a child result that a lifecycle message carries. */
export const LIFECYCLE_RESULT_MAX = 6144;

/** lifecycleEvent: the event name that the parent gets for a run state. */
export function lifecycleEvent(st: RunState | string): string {
  switch (st) {
    case "succeeded":
      return "succeeded";
    case "blocked":
      return "blocked";
    case "failed":
      return "failed";
    case "error":
      return "error";
    case "cancelled":
      return "cancelled";
  }
  return "";
}

const enc = new TextEncoder();
const dec = new TextDecoder();

/** truncateUTF8: cut s to at most max bytes on a UTF-8 boundary. */
export function truncateUTF8(s: string, max: number): [string, boolean] {
  const b = enc.encode(s);
  if (b.length <= max) return [s, false];
  let cut = max;
  while (cut > 0 && (b[cut]! & 0xc0) === 0x80) cut--;
  return [dec.decode(b.subarray(0, cut)), true];
}

/** Go strconv.Quote for the label of a lifecycle message (ASCII-safe subset). */
export function goQuote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    if (ch === '"') out += '\\"';
    else if (ch === "\\") out += "\\\\";
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (c < 0x20 || c === 0x7f) out += "\\x" + c.toString(16).padStart(2, "0");
    else out += ch;
  }
  return out + '"';
}

export interface LifecycleParts {
  /** JSON object; the field order is part of the message format. */
  data: {
    event: string;
    run_id: string;
    name: string;
    agent: string;
    failure_code: string;
    result: string;
    result_truncated: boolean;
    url: string;
  };
  content: string;
}

/** lifecycleParts: the data and the text of a lifecycle message about child. */
export function lifecycleParts(child: Run, side: boolean, event: string, runUrl: string): LifecycleParts {
  const [result, truncated] = truncateUTF8(child.result ?? "", LIFECYCLE_RESULT_MAX);
  const data = {
    event,
    run_id: child.id,
    name: child.name ?? "",
    agent: child.agent_name ?? "",
    failure_code: child.failure_code ?? "",
    result,
    result_truncated: truncated,
    url: runUrl,
  };
  const label = child.name ? child.name : child.id;
  let content = (side ? "Side run " : "Child run ") + goQuote(label) + " " + event + ".";
  if ((event === "failed" || event === "error") && child.failure_code) {
    content += " Failure code: " + child.failure_code + ".";
  }
  return { data, content };
}

/** The dedupe key of a lifecycle message (sendLifecycle). */
export function lifecycleDedupeKey(child: Run, event: string): string {
  if (event === "started") return `lifecycle:${child.id}:started`;
  return `lifecycle:${child.id}:${event}:${child.turns ?? 0}`;
}

/**
 * core.TurnMarker: identifies one turn end of r. An idle run that later
 * expires keeps the same marker; a cancel of a waiting run is reported once more.
 */
export function turnMarker(r: Pick<Run, "consumed_seq" | "turns" | "state">): string {
  let m = `${r.consumed_seq ?? 0}:${r.turns ?? 0}`;
  if (r.state === "cancelled") m += ":cancelled";
  return m;
}

/** Dashboard URL of a run (internal/links.Run). Empty without a public URL. */
export function runUrl(publicUrl: string, runId: string): string {
  if (!publicUrl) return "";
  return publicUrl.replace(/\/+$/, "") + "/runs/" + encodeURIComponent(runId);
}
