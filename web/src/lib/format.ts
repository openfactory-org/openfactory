/** Formatting helpers shared by pages. */

export function shortID(id?: string | null): string {
  if (!id) return "";
  const i = id.indexOf("_");
  if (i >= 0 && id.length > i + 9) return id.slice(i + 1, i + 9);
  return id.length > 8 ? id.slice(0, 8) : id;
}

export function money(usd?: number | null): string {
  const v = usd ?? 0;
  return v < 0.01 && v > 0 ? "<$0.01" : `$${v.toFixed(2)}`;
}

export function count(n?: number | null): string {
  const v = n ?? 0;
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
  if (v >= 10_000) return `${(v / 1000).toFixed(1)}k`;
  return v.toLocaleString("en-US");
}

export function duration(ms?: number | null): string {
  if (!ms || ms <= 0) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** A scorer sampling rate. The definition stores it as a percentage (0 to 100, default 10), not a ratio. */
export function samplingLabel(rate?: number | null): string {
  const v = rate ?? 10;
  return `${Number(v.toFixed(2))}%`;
}

export function percent(r?: number | null): string {
  if (r === undefined || r === null || Number.isNaN(r)) return "—";
  return `${Math.round(r * 100)}%`;
}

export function fmtTime(t?: string | null): string {
  if (!t) return "—";
  const d = new Date(t);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 1971) return "—";
  return d.toLocaleString();
}

/** The time of day, for example 14:05:09. */
export function fmtClock(t?: string | null): string {
  if (!t) return "";
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export function ago(t?: string | null, now: number = Date.now()): string {
  if (!t) return "—";
  const d = new Date(t).getTime();
  if (Number.isNaN(d)) return "—";
  const s = Math.round((now - d) / 1000);
  if (s < 45) return "just now";
  if (s < 90) return "1 minute ago";
  const m = Math.round(s / 60);
  if (m < 45) return `${m} minutes ago`;
  const h = Math.round(m / 60);
  if (h < 24) return h === 1 ? "1 hour ago" : `${h} hours ago`;
  const days = Math.round(h / 24);
  if (days < 30) return days === 1 ? "yesterday" : `${days} days ago`;
  return new Date(t).toLocaleDateString();
}

export function firstLine(s?: string | null, max = 80): string {
  const line =
    (s ?? "")
      .split("\n")
      .map((l) => l.trim())
      .find((l) => l !== "") ?? "";
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

/**
 * Splits a raw failure text into a short message for people and the raw text.
 * A provider error often wraps a JSON document: its "message" is the useful part.
 * `detail` is empty when the message already is the whole text.
 */
export function humanizeError(raw?: string | null): { message: string; detail: string } {
  const text = (raw ?? "").trim();
  if (!text) return { message: "", detail: "" };
  const code = FAILURE_TEXT[text];
  if (code) return { message: code, detail: text };
  const all = [...text.matchAll(/"message"\s*:\s*"((?:[^"\\]|\\.)*)"/g)];
  const last = all[all.length - 1]?.[1];
  if (!last) return { message: text, detail: "" };
  let message = last;
  try {
    message = JSON.parse(`"${last}"`) as string;
  } catch {
    /* keep the escaped text */
  }
  message = message.charAt(0).toUpperCase() + message.slice(1);
  return { message, detail: text };
}

/** Sentences for the run failure codes (internal/core/failure.go), for a run error that is only a code. */
export const FAILURE_TEXT: Record<string, string> = {
  invalid_configuration: "A secret, machine, MCP server or code-forge integration that the agent needs is missing or not valid.",
  environment_setup_failed: "The environment setup failed: a clone, a setup command or an MCP server did not start.",
  harness_auth_failed: "The harness credential is missing or was refused. Check the API key secret.",
  external_authentication_required: "The user that the run acts as has no linked account for this integration.",
  integration_disabled: "The integration that this run needs is not active.",
  agent_failed: "The agent ended its turn with an error.",
  out_of_memory: "The sandbox ran out of memory. Give the agent a larger machine.",
  resource_unavailable: "The sandbox backend had no capacity. The run can be tried again.",
  sandbox_create_failed: "The sandbox could not be created or did not start in time.",
  network_policy_timeout: "The network policy did not take effect in time, so the task did not start.",
  sandbox_lost: "The sandbox was lost: it was evicted or its node went away.",
  runner_lost: "The runner stopped responding.",
  agent_process_failed: "The agent process stopped before the session ended.",
  infrastructure_timeout: "The run waited too long in the queue or passed the maximum session time.",
  internal_error: "The control plane hit an internal error.",
  spend_limit_exceeded: "The spend limit was reached, so the run was stopped.",
};

/** Parses "KEY=value" lines into a map. Throws on malformed lines. */
export function parseKVLines(src: string): Record<string, string> {
  const out: Record<string, string> = {};
  src.split("\n").forEach((raw, i) => {
    const line = raw.trim();
    if (!line || line.startsWith("#")) return;
    const eq = line.indexOf("=");
    if (eq <= 0) throw new Error(`line ${i + 1}: expected KEY=value`);
    out[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  });
  return out;
}

export function kvLines(m?: Record<string, string> | null): string {
  return Object.entries(m ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

export function lines(src: string): string[] {
  return src
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
}
