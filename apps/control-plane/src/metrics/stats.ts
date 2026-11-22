// Port of the number helpers of internal/metrics/metrics.go.

export function ratio(n: number, d: number): number {
  return d === 0 ? 0 : n / d;
}

function sortedNums(xs: readonly number[]): number[] {
  return [...xs].sort((a, b) => a - b);
}

/** Median of integers. An even count averages the two middle values with integer division, as Go does. */
export function median(xs: readonly number[]): number {
  if (xs.length === 0) return 0;
  const s = sortedNums(xs);
  const n = s.length;
  if (n % 2 === 1) return s[(n - 1) / 2]!;
  return Math.trunc((s[n / 2 - 1]! + s[n / 2]!) / 2);
}

export function medianFloat(xs: readonly number[]): number {
  if (xs.length === 0) return 0;
  const s = sortedNums(xs);
  const n = s.length;
  if (n % 2 === 1) return s[(n - 1) / 2]!;
  return (s[n / 2 - 1]! + s[n / 2]!) / 2;
}

/** Nearest-rank percentile. */
export function percentile(xs: readonly number[], p: number): number {
  if (xs.length === 0) return 0;
  const s = sortedNums(xs);
  let rank = Math.ceil((p / 100) * s.length);
  if (rank < 1) rank = 1;
  if (rank > s.length) rank = s.length;
  return s[rank - 1]!;
}

/** Mean of integers, truncated. */
export function mean(xs: readonly number[]): number {
  if (xs.length === 0) return 0;
  let sum = 0;
  for (const x of xs) sum += x;
  return Math.trunc(sum / xs.length);
}
