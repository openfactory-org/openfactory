// Cron validation. validateCron ports definition.ValidateCron, which checks
// an expression with github.com/robfig/cron/v3 ParseStandard. The parser and
// its error messages (including the strconv and time.ParseDuration texts) are
// reproduced so that schedule errors match the Go server.

import { goFields, goQuote, goTimeQuote, goTrimSpace } from './gofmt.ts';

interface Bounds {
  min: number;
  max: number;
  names: Record<string, number> | null;
}

const minutes: Bounds = { min: 0, max: 59, names: null };
const hours: Bounds = { min: 0, max: 23, names: null };
const dom: Bounds = { min: 1, max: 31, names: null };
const months: Bounds = {
  min: 1,
  max: 12,
  names: { jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12 },
};
const dow: Bounds = { min: 0, max: 6, names: { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 } };

class CronError extends Error {}

const MAX_INT64 = (1n << 63n) - 1n;

/** goAtoi ports strconv.Atoi with its error texts. */
function goAtoi(s: string): number {
  const fail = (why: string): never => {
    throw new CronError(`strconv.Atoi: parsing ${goQuote(s)}: ${why}`);
  };
  let body = s;
  let neg = false;
  if (body[0] === '+' || body[0] === '-') {
    neg = body[0] === '-';
    body = body.slice(1);
  }
  if (body === '' || !/^[0-9]+$/.test(body)) fail('invalid syntax');
  let v = BigInt(body);
  if (neg) v = -v;
  if (v > MAX_INT64 || v < -(1n << 63n)) fail('value out of range');
  return Number(v);
}

function mustParseInt(expr: string): number {
  let num: number;
  try {
    num = goAtoi(expr);
  } catch (e) {
    throw new CronError(`failed to parse int from ${expr}: ${(e as Error).message}`);
  }
  if (num < 0) throw new CronError(`negative number (${num}) not allowed: ${expr}`);
  return num;
}

function parseIntOrName(expr: string, names: Record<string, number> | null): number {
  if (names !== null) {
    const v = names[expr.toLowerCase()];
    if (v !== undefined) return v;
  }
  return mustParseInt(expr);
}

function getRange(expr: string, r: Bounds): void {
  const rangeAndStep = expr.split('/');
  const lowAndHigh = rangeAndStep[0]!.split('-');
  const singleDigit = lowAndHigh.length === 1;
  let start: number;
  let end: number;
  let step: number;
  if (lowAndHigh[0] === '*' || lowAndHigh[0] === '?') {
    start = r.min;
    end = r.max;
  } else {
    start = parseIntOrName(lowAndHigh[0]!, r.names);
    if (lowAndHigh.length === 1) end = start;
    else if (lowAndHigh.length === 2) end = parseIntOrName(lowAndHigh[1]!, r.names);
    else throw new CronError(`too many hyphens: ${expr}`);
  }
  if (rangeAndStep.length === 1) {
    step = 1;
  } else if (rangeAndStep.length === 2) {
    step = mustParseInt(rangeAndStep[1]!);
    if (singleDigit) end = r.max;
  } else {
    throw new CronError(`too many slashes: ${expr}`);
  }
  if (start < r.min) throw new CronError(`beginning of range (${start}) below minimum (${r.min}): ${expr}`);
  if (end > r.max) throw new CronError(`end of range (${end}) above maximum (${r.max}): ${expr}`);
  if (start > end) throw new CronError(`beginning of range (${start}) beyond end of range (${end}): ${expr}`);
  if (step === 0) throw new CronError(`step of range should be a positive number: ${expr}`);
}

function getField(field: string, r: Bounds): void {
  for (const expr of field.split(',')) {
    if (expr === '') continue;
    getRange(expr, r);
  }
}

const unitMap: Record<string, bigint> = {
  ns: 1n,
  us: 1000n,
  'µs': 1000n,
  'μs': 1000n,
  ms: 1000000n,
  s: 1000000000n,
  m: 60n * 1000000000n,
  h: 3600n * 1000000000n,
};

/** parseDuration ports time.ParseDuration; it returns an error message or null. */
export function parseDuration(orig: string): string | null {
  let s = orig;
  let d = 0n;
  const invalid = `time: invalid duration ${goTimeQuote(orig)}`;
  if (s !== '' && (s[0] === '-' || s[0] === '+')) s = s.slice(1);
  if (s === '0') return null;
  if (s === '') return invalid;
  const limit = 1n << 63n;
  while (s !== '') {
    let v = 0n;
    let f = 0n;
    let scale = 1;
    if (!(s[0] === '.' || (s[0]! >= '0' && s[0]! <= '9'))) return invalid;
    let i = 0;
    while (i < s.length && s[i]! >= '0' && s[i]! <= '9') {
      v = v * 10n + BigInt(s.charCodeAt(i) - 48);
      if (v > limit) return invalid;
      i++;
    }
    const pre = i > 0;
    s = s.slice(i);
    let post = false;
    if (s !== '' && s[0] === '.') {
      s = s.slice(1);
      let j = 0;
      let overflow = false;
      while (j < s.length && s[j]! >= '0' && s[j]! <= '9') {
        if (!overflow) {
          const nf = f * 10n + BigInt(s.charCodeAt(j) - 48);
          if (nf >= limit) overflow = true;
          else {
            f = nf;
            scale *= 10;
          }
        }
        j++;
      }
      post = j > 0;
      s = s.slice(j);
    }
    if (!pre && !post) return invalid;
    let k = 0;
    while (k < s.length && !(s[k] === '.' || (s[k]! >= '0' && s[k]! <= '9'))) k++;
    if (k === 0) return `time: missing unit in duration ${goTimeQuote(orig)}`;
    const u = s.slice(0, k);
    s = s.slice(k);
    const unit = unitMap[u];
    if (unit === undefined) return `time: unknown unit ${goTimeQuote(u)} in duration ${goTimeQuote(orig)}`;
    if (v > limit / unit) return invalid;
    v *= unit;
    if (f > 0n) {
      v += BigInt(Math.floor(Number(f) * (Number(unit) / scale)));
      if (v > limit) return invalid;
    }
    d += v;
    if (d > limit) return invalid;
  }
  const neg = orig[0] === '-';
  if (!neg && d > limit - 1n) return invalid;
  return null;
}

function parseDescriptor(descriptor: string): void {
  switch (descriptor) {
    case '@yearly':
    case '@annually':
    case '@monthly':
    case '@weekly':
    case '@daily':
    case '@midnight':
    case '@hourly':
      return;
  }
  const every = '@every ';
  if (descriptor.startsWith(every)) {
    const err = parseDuration(descriptor.slice(every.length));
    if (err !== null) throw new CronError(`failed to parse duration ${descriptor}: ${err}`);
    return;
  }
  throw new CronError(`unrecognized descriptor: ${descriptor}`);
}

function parseStandard(spec: string): void {
  if (spec.length === 0) throw new CronError('empty spec string');
  if (spec.startsWith('@')) {
    parseDescriptor(spec);
    return;
  }
  const fields = goFields(spec);
  if (fields.length !== 5) throw new CronError(`expected exactly 5 fields, found ${fields.length}: [${fields.join(' ')}]`);
  getField(fields[0]!, minutes);
  getField(fields[1]!, hours);
  getField(fields[2]!, dom);
  getField(fields[3]!, months);
  getField(fields[4]!, dow);
}

/**
 * validateCron validates a five-field UTC cron expression or a descriptor
 * such as @daily or @every 1h. It returns an error message, or null.
 */
export function validateCron(expr: string): string | null {
  const e = goTrimSpace(expr);
  if (e.startsWith('TZ=') || e.startsWith('CRON_TZ=')) return 'schedules are always UTC; time zone prefixes are not supported';
  if (!e.startsWith('@') && goFields(e).length !== 5) {
    return 'must be a five-field cron expression or a descriptor such as @daily or @every 1h';
  }
  try {
    parseStandard(e);
  } catch (err) {
    if (err instanceof CronError) return err.message;
    throw err;
  }
  return null;
}
