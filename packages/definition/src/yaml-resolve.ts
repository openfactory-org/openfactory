// yaml.v3 scalar resolution. The Go compiler decides types from the tags that
// gopkg.in/yaml.v3 gives to plain scalars; this module reproduces those rules
// (resolve.go and the scalar decoder) so that "must be a string" and
// "must be an integer" errors match.

export type Resolved =
  | { tag: '!!null'; value: null }
  | { tag: '!!bool'; value: boolean }
  | { tag: '!!int'; value: number; big: bigint }
  | { tag: '!!float'; value: number }
  | { tag: '!!timestamp'; value: string }
  | { tag: '!!merge'; value: string }
  | { tag: '!!str'; value: string }
  | { tag: string; value: string };

const MAX_INT64 = (1n << 63n) - 1n;
const MIN_INT64 = -(1n << 63n);
const MAX_UINT64 = (1n << 64n) - 1n;

const resolveMap: Record<string, Resolved> = {};
for (const s of ['true', 'True', 'TRUE']) resolveMap[s] = { tag: '!!bool', value: true };
for (const s of ['false', 'False', 'FALSE']) resolveMap[s] = { tag: '!!bool', value: false };
for (const s of ['', '~', 'null', 'Null', 'NULL']) resolveMap[s] = { tag: '!!null', value: null };
for (const s of ['.nan', '.NaN', '.NAN']) resolveMap[s] = { tag: '!!float', value: NaN };
for (const s of ['.inf', '.Inf', '.INF', '+.inf', '+.Inf', '+.INF']) resolveMap[s] = { tag: '!!float', value: Infinity };
for (const s of ['-.inf', '-.Inf', '-.INF']) resolveMap[s] = { tag: '!!float', value: -Infinity };
resolveMap['<<'] = { tag: '!!merge', value: '<<' };

function hint(s: string): string {
  if (s === '') return 'N';
  const c = s[0] ?? '';
  if (c === '+' || c === '-') return 'S';
  if (c >= '0' && c <= '9') return 'D';
  if ('yYnNtTfFoO~'.includes(c)) return 'M';
  if (c === '.') return '.';
  return '';
}

const longTagPrefix = 'tag:yaml.org,2002:';

/** shortTag maps tag:yaml.org,2002:x to !!x, like yaml.v3. */
export function shortTag(tag: string): string {
  if (tag.startsWith(longTagPrefix)) return '!!' + tag.slice(longTagPrefix.length);
  return tag;
}

function digitValue(c: string): number {
  const code = c.charCodeAt(0);
  if (code >= 48 && code <= 57) return code - 48;
  if (code >= 97 && code <= 122) return code - 97 + 10;
  if (code >= 65 && code <= 90) return code - 65 + 10;
  return 99;
}

function parseDigits(s: string, base: number): bigint | null {
  if (s === '') return null;
  let n = 0n;
  const b = BigInt(base);
  for (const c of s) {
    const d = digitValue(c);
    if (d >= base) return null;
    n = n * b + BigInt(d);
  }
  return n;
}

/** goParseUint parses like strconv.ParseUint(s, base, 64); base 0 detects the prefix. */
export function goParseUint(s: string, base: number): bigint | null {
  if (s === '') return null;
  let body = s;
  let b = base;
  if (base === 0) {
    b = 10;
    if (body[0] === '0') {
      const p = (body[1] ?? '').toLowerCase();
      if (body.length >= 3 && p === 'b') {
        b = 2;
        body = body.slice(2);
      } else if (body.length >= 3 && p === 'o') {
        b = 8;
        body = body.slice(2);
      } else if (body.length >= 3 && p === 'x') {
        b = 16;
        body = body.slice(2);
      } else {
        b = 8;
        body = body.slice(1);
        if (body === '') return 0n;
      }
    }
  }
  const n = parseDigits(body, b);
  if (n === null || n > MAX_UINT64) return null;
  return n;
}

/** goParseInt parses like strconv.ParseInt(s, base, 64). */
export function goParseInt(s: string, base: number): bigint | null {
  if (s === '') return null;
  let body = s;
  let neg = false;
  if (body[0] === '+' || body[0] === '-') {
    neg = body[0] === '-';
    body = body.slice(1);
  }
  const u = goParseUint(body, base);
  if (u === null) return null;
  const v = neg ? -u : u;
  if (v > MAX_INT64 || v < MIN_INT64) return null;
  return v;
}

const yamlStyleFloat = /^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$/;

function parseFloatStrict(s: string): number | null {
  const f = Number(s);
  if (!Number.isFinite(f)) return null;
  return f;
}

function daysIn(year: number, month: number): number {
  return new Date(Date.UTC(year, month, 0)).getUTCDate();
}

const tsDate = /^(\d{4})-(\d{1,2})-(\d{1,2})$/;
const tsSpace = /^(\d{4})-(\d{1,2})-(\d{1,2}) (\d{1,2}):(\d{1,2}):(\d{1,2})(\.\d+)?$/;
const tsT = /^(\d{4})-(\d{1,2})-(\d{1,2})[Tt](\d{1,2}):(\d{1,2}):(\d{1,2})(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

function validDate(m: RegExpMatchArray, withTime: boolean): boolean {
  const y = Number(m[1]);
  const mo = Number(m[2]);
  const d = Number(m[3]);
  if (mo < 1 || mo > 12 || d < 1 || d > daysIn(y, mo)) return false;
  if (withTime) {
    if (Number(m[4]) > 23 || Number(m[5]) > 59 || Number(m[6]) > 59) return false;
    const zone = m[8];
    if (zone && zone !== 'Z') {
      const zh = Number(zone.slice(1, 3));
      const zm = Number(zone.slice(4, 6));
      if (zh > 24 || zm > 59) return false;
    }
  }
  return true;
}

function isTimestamp(s: string): boolean {
  let i = 0;
  while (i < s.length && s[i]! >= '0' && s[i]! <= '9') i++;
  if (i !== 4 || i === s.length || s[i] !== '-') return false;
  let m = tsT.exec(s);
  if (m) return validDate(m, true);
  m = tsSpace.exec(s);
  if (m) return validDate(m, true);
  m = tsDate.exec(s);
  return m !== null && validDate(m, false);
}

function intResult(v: bigint): Resolved {
  return { tag: '!!int', value: Number(v), big: v };
}

/** resolvePlain gives the tag of a plain scalar without an explicit tag. */
export function resolvePlain(input: string): Resolved {
  return resolveWith('', input);
}

function resolvable(tag: string): boolean {
  return ['', '!!str', '!!bool', '!!int', '!!float', '!!null', '!!timestamp'].includes(tag);
}

function resolveWith(tag: string, input: string): Resolved {
  if (!resolvable(tag)) return { tag, value: input };
  const h = hint(input);
  if (h !== '' && tag !== '!!str' && tag !== '!!binary') {
    const item = resolveMap[input];
    if (item) return item;
    switch (h) {
      case 'M':
        break;
      case '.': {
        if (/^\.[0-9]+([eE][-+]?[0-9]+)?$/.test(input)) {
          const f = parseFloatStrict(input);
          if (f !== null) return { tag: '!!float', value: f };
        }
        break;
      }
      case 'D':
      case 'S': {
        if ((tag === '' || tag === '!!timestamp') && isTimestamp(input)) return { tag: '!!timestamp', value: input };
        const plain = input.replaceAll('_', '');
        const iv = goParseInt(plain, 0);
        if (iv !== null) return intResult(iv);
        const uv = goParseUint(plain, 0);
        if (uv !== null) return intResult(uv);
        if (yamlStyleFloat.test(plain)) {
          const f = parseFloatStrict(plain);
          if (f !== null) return { tag: '!!float', value: f };
        }
        if (plain.startsWith('0b')) {
          const v = goParseInt(plain.slice(2), 2) ?? goParseUint(plain.slice(2), 2);
          if (v !== null) return intResult(v);
        } else if (plain.startsWith('-0b')) {
          const v = goParseInt('-' + plain.slice(3), 2);
          if (v !== null) return intResult(v);
        }
        if (plain.startsWith('0o')) {
          const v = goParseInt(plain.slice(2), 8) ?? goParseUint(plain.slice(2), 8);
          if (v !== null) return intResult(v);
        } else if (plain.startsWith('-0o')) {
          const v = goParseInt('-' + plain.slice(3), 8);
          if (v !== null) return intResult(v);
        }
        break;
      }
    }
  }
  return { tag: '!!str', value: input };
}

/**
 * decodeAs resolves a scalar of tag `tag` for a typed decode, like the
 * yaml.v3 decoder does with node.Decode. It returns null when the decode
 * fails ("cannot decode ... as a ...").
 */
export function decodeAs(tag: string, value: string, indicatedString: boolean): Resolved | null {
  if (indicatedString) return { tag: '!!str', value };
  const t = shortTag(tag);
  const r = resolveWith(t, value);
  if (t === '' || t === r.tag || t === '!!str' || t === '!!binary' || !resolvable(t)) return r;
  if (t === '!!float' && r.tag === '!!int') return { tag: '!!float', value: r.value as number };
  return null;
}

const yaml11True = ['y', 'Y', 'yes', 'Yes', 'YES', 'on', 'On', 'ON'];
const yaml11False = ['n', 'N', 'no', 'No', 'NO', 'off', 'Off', 'OFF'];

/** decodeBool decodes like node.Decode(&b) into a Go bool. */
export function decodeBool(tag: string, value: string, indicatedString: boolean): boolean | null {
  const r = decodeAs(tag, value, indicatedString);
  if (r === null) return null;
  if (r.tag === '!!bool') return r.value as boolean;
  if (typeof r.value === 'string') {
    if (yaml11True.includes(r.value)) return true;
    if (yaml11False.includes(r.value)) return false;
  }
  return null;
}

/** decodeInt decodes like node.Decode(&i) into a Go int (64-bit). */
export function decodeInt(tag: string, value: string, indicatedString: boolean): number | null {
  const r = decodeAs(tag, value, indicatedString);
  if (r === null) return null;
  if (r.tag === '!!int' && 'big' in r) {
    const b = r.big as bigint;
    if (b > MAX_INT64 || b < MIN_INT64) return null;
    return Number(b);
  }
  if (r.tag === '!!float' && typeof r.value === 'number') {
    const f = r.value;
    if (Number.isFinite(f) && f <= 9223372036854775807 && f >= -9223372036854775808) return Math.trunc(f);
  }
  return null;
}

/** decodeFloat decodes like node.Decode(&f) into a Go float64. */
export function decodeFloat(tag: string, value: string, indicatedString: boolean): number | null {
  const r = decodeAs(tag, value, indicatedString);
  if (r === null) return null;
  if ((r.tag === '!!int' || r.tag === '!!float') && typeof r.value === 'number') return r.value;
  return null;
}
