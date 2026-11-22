// The options, clock, IDs and log that the scoring evaluator and the
// Self-improvement coordinator share.
import { base32 } from "@openfactory/core";
import { DEFAULT_MAX_FAILURE_AGE_MS, DEFAULT_MAX_TRANSCRIPT_BYTES, type ScoringOptions } from "./types";

export type ResolvedScoringOptions = Required<Pick<ScoringOptions, "maxTranscriptBytes" | "maxFailureAgeMs">> &
  ScoringOptions;

export class ScoringContext {
  readonly o: ResolvedScoringOptions;

  constructor(opts: ScoringOptions = {}) {
    this.o = {
      ...opts,
      maxTranscriptBytes:
        opts.maxTranscriptBytes && opts.maxTranscriptBytes > 0 ? opts.maxTranscriptBytes : DEFAULT_MAX_TRANSCRIPT_BYTES,
      maxFailureAgeMs:
        opts.maxFailureAgeMs === undefined || opts.maxFailureAgeMs === 0
          ? DEFAULT_MAX_FAILURE_AGE_MS
          : opts.maxFailureAgeMs,
    };
  }

  now(): number {
    return (this.o.now ?? Date.now)();
  }

  nowIso(): string {
    return new Date(this.now()).toISOString();
  }

  newId(prefix: string): string {
    if (this.o.newId) return this.o.newId(prefix);
    return `${prefix}_${base32(crypto.getRandomValues(new Uint8Array(12))).slice(0, 20)}`;
  }

  randomInt(n: number): number {
    if (this.o.randomInt) return this.o.randomInt(n);
    return crypto.getRandomValues(new Uint32Array(1))[0]! % n;
  }

  runUrl(runId: string): string {
    return this.o.publicUrl ? `${this.o.publicUrl.replace(/\/+$/, "")}/runs/${runId}` : "";
  }

  log(level: "info" | "warn" | "error", msg: string, f?: Record<string, unknown>): void {
    this.o.log?.[level](msg, f);
  }
}

export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
