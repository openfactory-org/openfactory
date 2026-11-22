// Port of the pure parts of internal/spend/spend.go. Periods are UTC calendar
// days and months. The spend of a period is the cost of the runs created in
// that period (C5: the whole cost of a run stays in the day and month of its
// created time).

export const WARN_FRACTION = 0.8;

export type Scope = "factory" | "team";
export type Period = "daily" | "monthly";

export interface Limit {
  scope: Scope;
  period: Period;
  spent_usd: number;
  limit_usd: number;
  period_start: string;
  resets_at: string;
}

/** factory.yaml spendLimit (daily, monthly). 0 or missing: no limit. */
export interface SpendLimit {
  daily?: number;
  monthly?: number;
}

export function parseSpendLimit(v: unknown): SpendLimit {
  if (!v || typeof v !== "object") return {};
  const o = v as Record<string, unknown>;
  const num = (x: unknown): number | undefined => (typeof x === "number" && x > 0 ? x : undefined);
  const out: SpendLimit = {};
  const d = num(o.daily) ?? num(o.daily_usd) ?? num(o.dailyUsd);
  const m = num(o.monthly) ?? num(o.monthly_usd) ?? num(o.monthlyUsd);
  if (d !== undefined) out.daily = d;
  if (m !== undefined) out.monthly = m;
  return out;
}

export const over = (l: Pick<Limit, "limit_usd" | "spent_usd">): boolean =>
  l.limit_usd > 0 && l.spent_usd >= l.limit_usd;
export const fraction = (l: Pick<Limit, "limit_usd" | "spent_usd">): number =>
  l.limit_usd <= 0 ? 0 : l.spent_usd / l.limit_usd;

/** dayStart: "YYYY-MM-DD" (UTC) of t. */
export function dayKey(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/** monthStart: "YYYY-MM" (UTC) of t. */
export function monthKey(ms: number): string {
  return new Date(ms).toISOString().slice(0, 7);
}

export function dayStartMs(ms: number): number {
  const d = new Date(ms);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
}

export function monthStartMs(ms: number): number {
  const d = new Date(ms);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
}

export function nextMonthStartMs(ms: number): number {
  const d = new Date(ms);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
}

/** The limits of a factory at `now`, given the spent totals of the current day and month. */
export function buildLimits(
  scope: Scope,
  limit: SpendLimit,
  spentDay: number,
  spentMonth: number,
  nowMs: number,
): Limit[] {
  const out: Limit[] = [];
  if (limit.daily && limit.daily > 0) {
    const s = dayStartMs(nowMs);
    out.push({
      scope,
      period: "daily",
      spent_usd: spentDay,
      limit_usd: limit.daily,
      period_start: new Date(s).toISOString(),
      resets_at: new Date(s + 86_400_000).toISOString(),
    });
  }
  if (limit.monthly && limit.monthly > 0) {
    const s = monthStartMs(nowMs);
    out.push({
      scope,
      period: "monthly",
      spent_usd: spentMonth,
      limit_usd: limit.monthly,
      period_start: new Date(s).toISOString(),
      resets_at: new Date(nextMonthStartMs(nowMs)).toISOString(),
    });
  }
  return out;
}

export function exceeded(limits: readonly Limit[]): Limit | undefined {
  return limits.find(over);
}

export function warnings(limits: readonly Limit[]): Limit[] {
  return limits.filter((l) => l.limit_usd > 0 && fraction(l) >= WARN_FRACTION);
}

/** warnKey: one inbox notice per (scope, id, period, start). */
export function warnKey(l: Pick<Limit, "scope" | "period" | "period_start">, id: string): string {
  return `spend:warned:${l.scope}:${id}:${l.period}:${l.period_start.slice(0, 10)}`;
}

export function noticeText(l: Limit): string {
  const pct = Math.round(fraction(l) * 100);
  const period = l.period === "daily" ? "daily" : "monthly";
  return `The ${l.scope} has used ${pct}% of its ${period} spend limit ($${l.spent_usd.toFixed(2)} of $${l.limit_usd.toFixed(2)}).`;
}
