// Deterministic sampling and label resolution: port of scorer.go and the
// answer parsing of judge.go.
import type { ScorerLabel } from "@openfactory/core";

/**
 * Reports whether a run is in a scorer's sample. ratePercent is the sampling
 * rate from 0 through 100 (rounded to two decimals). The decision is a function
 * of (scorerId, runId): SHA-256 of both mapped uniformly onto [0, 100), so a
 * repeated delivery of the same run update always agrees and scorers sample
 * independently.
 */
export async function shouldSample(scorerId: string, runId: string, ratePercent: number): Promise<boolean> {
  const rate = Math.round(ratePercent * 100) / 100;
  if (!(rate > 0) || Number.isNaN(rate)) return false;
  if (rate >= 100) return true;
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(`${scorerId}\x00${runId}`)));
  // The first 8 bytes as a big-endian uint64; 53 bits give an exact float in [0, 1).
  let v = 0n;
  for (let i = 0; i < 8; i++) v = (v << 8n) | BigInt(digest[i]!);
  const u = Number(v >> 11n) / 2 ** 53;
  return u * 100 < rate;
}

/** Matches the judge's answer to a declared label: exactly first, then ignoring case, white space, quotes and dots. */
export function resolveLabel(labels: readonly ScorerLabel[] | null | undefined, got: string): ScorerLabel | undefined {
  const list = labels ?? [];
  for (const l of list) if (l.value === got) return l;
  const norm = got.trim().replace(/^["'`.]+|["'`.]+$/g, "");
  const lower = norm.toLowerCase();
  for (const l of list) if (l.value.toLowerCase() === lower) return l;
  return undefined;
}

/** Finds the last JSON object with a "label" in text. */
export function parseJudgeAnswer(text: string): { label: string; rationale: string } | undefined {
  for (let i = text.lastIndexOf("{"); i >= 0; i = i === 0 ? -1 : text.lastIndexOf("{", i - 1)) {
    const obj = firstJsonValue(text.slice(i));
    if (obj && typeof obj === "object" && !Array.isArray(obj)) {
      const o = obj as Record<string, unknown>;
      const label = typeof o["label"] === "string" ? o["label"].trim() : "";
      if (label !== "") {
        return { label, rationale: typeof o["rationale"] === "string" ? o["rationale"].trim() : "" };
      }
    }
  }
  return undefined;
}

/** Decodes the first JSON value of s and ignores what follows (Go json.Decoder.Decode). */
function firstJsonValue(s: string): unknown {
  // Find the end of the first balanced object, then parse that part.
  let depth = 0;
  let inStr = false;
  for (let i = 0; i < s.length; i++) {
    const c = s[i]!;
    if (inStr) {
      if (c === "\\") i++;
      else if (c === '"') inStr = false;
      continue;
    }
    if (c === '"') inStr = true;
    else if (c === "{") depth++;
    else if (c === "}") {
      depth--;
      if (depth === 0) {
        try {
          return JSON.parse(s.slice(0, i + 1)) as unknown;
        } catch {
          return undefined;
        }
      }
    }
  }
  return undefined;
}
