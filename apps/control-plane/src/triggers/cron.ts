// Cron schedules in UTC. Port of internal/triggers/cron.go with a port of the
// github.com/robfig/cron/v3 parser (five fields plus descriptors, "@every"
// with Go duration syntax) and its Next() algorithm, so slots match the Go
// scheduler. Times are Unix milliseconds; 0 is "no next time".

const STAR_BIT = 1n << 63n;

interface Bounds {
  min: number;
  max: number;
  names?: Record<string, number>;
}
const MINUTES: Bounds = { min: 0, max: 59 };
const HOURS: Bounds = { min: 0, max: 23 };
const DOM: Bounds = { min: 1, max: 31 };
const MONTHS: Bounds = {
  min: 1,
  max: 12,
  names: { jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12 },
};
const DOW: Bounds = { min: 0, max: 6, names: { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 } };

/** The shortest accepted "@every" interval. */
export const MIN_EVERY_INTERVAL_MS = 60_000;

export interface CronSchedule {
  /** The first slot strictly after t (ms), or 0. */
  next(t: number): number;
}

class SpecSchedule implements CronSchedule {
  constructor(
    readonly minute: bigint,
    readonly hour: bigint,
    readonly dom: bigint,
    readonly month: bigint,
    readonly dow: bigint,
  ) {}

  private dayMatches(d: Date): boolean {
    const domMatch = ((1n << BigInt(d.getUTCDate())) & this.dom) > 0n;
    const dowMatch = ((1n << BigInt(d.getUTCDay())) & this.dow) > 0n;
    if ((this.dom & STAR_BIT) > 0n || (this.dow & STAR_BIT) > 0n) return domMatch && dowMatch;
    return domMatch || dowMatch;
  }

  next(t0: number): number {
    // Round up to the next whole second (seconds field is always 0).
    let t = new Date(t0 - (((t0 % 1000) + 1000) % 1000) + 1000);
    let added = false;
    const yearLimit = t.getUTCFullYear() + 5;
    const ymd = (y: number, m: number, d: number, h = 0, mi = 0) => new Date(Date.UTC(y, m, d, h, mi, 0, 0));
    wrap: for (;;) {
      if (t.getUTCFullYear() > yearLimit) return 0;
      while (((1n << BigInt(t.getUTCMonth() + 1)) & this.month) === 0n) {
        if (!added) {
          added = true;
          t = ymd(t.getUTCFullYear(), t.getUTCMonth(), 1);
        }
        t = ymd(t.getUTCFullYear(), t.getUTCMonth() + 1, 1, t.getUTCHours(), t.getUTCMinutes());
        if (t.getUTCMonth() === 0) continue wrap;
      }
      while (!this.dayMatches(t)) {
        if (!added) {
          added = true;
          t = ymd(t.getUTCFullYear(), t.getUTCMonth(), t.getUTCDate());
        }
        t = new Date(t.getTime() + 86_400_000);
        if (t.getUTCDate() === 1) continue wrap;
      }
      while (((1n << BigInt(t.getUTCHours())) & this.hour) === 0n) {
        if (!added) {
          added = true;
          t = ymd(t.getUTCFullYear(), t.getUTCMonth(), t.getUTCDate(), t.getUTCHours());
        }
        t = new Date(t.getTime() + 3_600_000);
        if (t.getUTCHours() === 0) continue wrap;
      }
      while (((1n << BigInt(t.getUTCMinutes())) & this.minute) === 0n) {
        if (!added) {
          added = true;
          t = new Date(t.getTime() - (t.getTime() % 60_000));
        }
        t = new Date(t.getTime() + 60_000);
        if (t.getUTCMinutes() === 0) continue wrap;
      }
      // Second field: only second 0.
      while (t.getUTCSeconds() !== 0) {
        if (!added) {
          added = true;
          t = new Date(t.getTime() - (t.getTime() % 1000));
        }
        t = new Date(t.getTime() + 1000);
        if (t.getUTCSeconds() === 0) continue wrap;
      }
      return t.getTime();
    }
  }
}

class ConstantDelaySchedule implements CronSchedule {
  constructor(readonly delayMs: number) {}
  next(t: number): number {
    return t - (((t % 1000) + 1000) % 1000) + this.delayMs;
  }
}

class CronError extends Error {}

function atoi(s: string): number {
  if (!/^[+-]?\d+$/.test(s)) throw new CronError(`strconv.Atoi: parsing "${s}": invalid syntax`);
  return Number(s);
}

function mustParseInt(expr: string): number {
  let n: number;
  try {
    n = atoi(expr);
  } catch (e) {
    throw new CronError(`failed to parse int from ${expr}: ${(e as Error).message}`);
  }
  if (n < 0) throw new CronError(`negative number (${n}) not allowed: ${expr}`);
  return n;
}

function parseIntOrName(expr: string, names?: Record<string, number>): number {
  const v = names?.[expr.toLowerCase()];
  return v !== undefined ? v : mustParseInt(expr);
}

function getBits(min: number, max: number, step: number): bigint {
  let b = 0n;
  if (step === 1) return ~(~0n << BigInt(max + 1)) & (~0n << BigInt(min));
  for (let i = min; i <= max; i += step) b |= 1n << BigInt(i);
  return b;
}

function getRange(expr: string, r: Bounds): bigint {
  const rangeAndStep = expr.split("/");
  const lowAndHigh = rangeAndStep[0]!.split("-");
  const single = lowAndHigh.length === 1;
  let start: number;
  let end: number;
  let extra = 0n;
  if (lowAndHigh[0] === "*" || lowAndHigh[0] === "?") {
    start = r.min;
    end = r.max;
    extra = STAR_BIT;
  } else {
    start = parseIntOrName(lowAndHigh[0]!, r.names);
    if (lowAndHigh.length === 1) end = start;
    else if (lowAndHigh.length === 2) end = parseIntOrName(lowAndHigh[1]!, r.names);
    else throw new CronError(`too many hyphens: ${expr}`);
  }
  let step: number;
  if (rangeAndStep.length === 1) step = 1;
  else if (rangeAndStep.length === 2) {
    step = mustParseInt(rangeAndStep[1]!);
    if (single) end = r.max;
    if (step > 1) extra = 0n;
  } else throw new CronError(`too many slashes: ${expr}`);
  if (start < r.min) throw new CronError(`beginning of range (${start}) below minimum (${r.min}): ${expr}`);
  if (end > r.max) throw new CronError(`end of range (${end}) above maximum (${r.max}): ${expr}`);
  if (start > end) throw new CronError(`beginning of range (${start}) beyond end of range (${end}): ${expr}`);
  if (step === 0) throw new CronError(`step of range should be a positive number: ${expr}`);
  return getBits(start, end, step) | extra;
}

function getField(field: string, r: Bounds): bigint {
  let bits = 0n;
  for (const expr of field.split(",")) bits |= getRange(expr, r);
  return bits;
}

const all = (r: Bounds): bigint => getBits(r.min, r.max, 1) | STAR_BIT;

const UNIT: Record<string, number> = { ns: 1e-6, us: 1e-3, "µs": 1e-3, "μs": 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

/** Go time.ParseDuration, in milliseconds (fractions of a millisecond kept). */
export function parseGoDuration(orig: string): number {
  let s = orig;
  let neg = false;
  if (s[0] === "-" || s[0] === "+") {
    neg = s[0] === "-";
    s = s.slice(1);
  }
  if (s === "0") return 0;
  if (s === "") throw new CronError(`time: invalid duration "${orig}"`);
  let total = 0;
  while (s !== "") {
    const m = /^(\d*)(?:\.(\d*))?/.exec(s)!;
    if (m[0] === "" || m[0] === "." || (m[1] === "" && (m[2] ?? "") === "")) throw new CronError(`time: invalid duration "${orig}"`);
    s = s.slice(m[0].length);
    const u = /^[^\d.]+/.exec(s);
    if (!u) throw new CronError(`time: missing unit in duration "${orig}"`);
    const unit = UNIT[u[0]];
    if (unit === undefined) throw new CronError(`time: unknown unit "${u[0]}" in duration "${orig}"`);
    s = s.slice(u[0].length);
    total += Number((m[1] || "0") + "." + (m[2] || "0")) * unit;
  }
  return neg ? -total : total;
}

function parseDescriptor(d: string): CronSchedule {
  switch (d) {
    case "@yearly":
    case "@annually":
      return new SpecSchedule(1n, 1n, 1n << 1n, 1n << 1n, all(DOW));
    case "@monthly":
      return new SpecSchedule(1n, 1n, 1n << 1n, all(MONTHS), all(DOW));
    case "@weekly":
      return new SpecSchedule(1n, 1n, all(DOM), all(MONTHS), 1n);
    case "@daily":
    case "@midnight":
      return new SpecSchedule(1n, 1n, all(DOM), all(MONTHS), all(DOW));
    case "@hourly":
      return new SpecSchedule(1n, all(HOURS), all(DOM), all(MONTHS), all(DOW));
  }
  if (d.startsWith("@every ")) {
    let ms: number;
    try {
      ms = parseGoDuration(d.slice("@every ".length));
    } catch (e) {
      throw new CronError(`failed to parse duration ${d}: ${(e as Error).message}`);
    }
    // robfig rounds down to whole seconds, at least one second.
    let delay = ms - (ms % 1000);
    if (delay < 1000) delay = 1000;
    return new ConstantDelaySchedule(delay);
  }
  throw new CronError(`unrecognized descriptor: ${d}`);
}

function parseSpec(spec: string): CronSchedule {
  if (spec.length === 0) throw new CronError("empty spec string");
  if (spec.startsWith("@")) return parseDescriptor(spec);
  const fields = spec.split(/\s+/).filter((x) => x !== "");
  if (fields.length !== 5) throw new CronError(`expected exactly 5 fields, found ${fields.length}: [${fields.join(" ")}]`);
  return new SpecSchedule(
    getField(fields[0]!, MINUTES),
    getField(fields[1]!, HOURS),
    getField(fields[2]!, DOM),
    getField(fields[3]!, MONTHS),
    getField(fields[4]!, DOW),
  );
}

/** Parses a schedule; throws Error with the Go message for an invalid one. */
export function parseCron(expr0: string): CronSchedule {
  const expr = expr0.trim();
  if (expr === "") throw new Error("cron expression is required");
  if (expr.startsWith("TZ=") || expr.startsWith("CRON_TZ=")) throw new Error("time zones are not supported; schedules run in UTC");
  if (expr.startsWith("@every")) {
    let s: CronSchedule;
    try {
      s = parseSpec(expr);
    } catch (e) {
      throw new Error(`invalid cron expression "${expr}": ${(e as Error).message}`);
    }
    if (s instanceof ConstantDelaySchedule && s.delayMs < MIN_EVERY_INTERVAL_MS) {
      throw new Error(`invalid cron expression "${expr}": interval must be at least 1m0s`);
    }
    return s;
  }
  if (!expr.startsWith("@") && expr.split(/\s+/).filter((x) => x !== "").length !== 5) {
    throw new Error(`invalid cron expression "${expr}": expected 5 fields (minute hour day-of-month month day-of-week)`);
  }
  try {
    return parseSpec(expr);
  } catch (e) {
    throw new Error(`invalid cron expression "${expr}": ${(e as Error).message}`);
  }
}

/** Returns an error message for an invalid schedule, or null. */
export function validateCron(expr: string): string | null {
  try {
    parseCron(expr);
    return null;
  } catch (e) {
    return (e as Error).message;
  }
}
