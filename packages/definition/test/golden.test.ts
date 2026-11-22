// Golden tests: the TypeScript port must give the outputs of the Go reference
// (tag go-final) captured by test/golden/capture/capture.sh.
//
// Accepted differences, each checked explicitly:
//   - cloudProviders.aws and platform.os macos/windows are rejected by the
//     port (Linux only, no AWS); the Go reference accepted them.
//   - YAML syntax error messages come from a different parser: only the file
//     is compared.
//   - Webhook created_at (a zero time in Go) is not part of the port output.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { compile } from '../src/compile.ts';
import { validateCron } from '../src/cron.ts';
import { defaultDefinition } from '../src/defaults.ts';
import { diff, diffSummary } from '../src/diff.ts';
import { parseRepoAtCommit, roundHundredths } from '../src/extras.ts';
import { normalizeHost, resolvePolicy } from '../src/networkpolicy.ts';
import { loadDir } from '../src/node.ts';
import { validDeliveryIdHeader, validURLTokenSecret } from '../src/resources.ts';
import { triggerEvents, triggerProviders } from '../src/triggers.ts';
import type { CompiledDefinition, ValidationError } from '../src/types.ts';
import { validateSchema } from '../src/validate-schema.ts';

const here = dirname(fileURLToPath(import.meta.url));
const goldenDir = join(here, 'golden');
const repo = join(here, '..', '..', '..');

interface Rec {
  kind: string;
  files?: Record<string, string>;
  compiled?: unknown;
  errors?: ValidationError[] | null;
  old?: unknown;
  new?: unknown;
  changes?: unknown[];
  summary?: string;
  path?: string;
  content?: string;
  valid?: boolean;
  leaves?: { instance: string; keyword: string }[];
}

const corpus = JSON.parse(readFileSync(join(goldenDir, 'corpus.json'), 'utf8')) as Rec[];
const vectors = JSON.parse(readFileSync(join(goldenDir, 'vectors.json'), 'utf8')) as { kind: string; input: unknown; output: unknown }[];

/** stripGo removes fields of the Go JSON that the port does not produce. */
function stripGo(def: unknown): unknown {
  if (def === null || def === undefined) return null;
  const d = structuredClone(def) as Record<string, unknown>;
  for (const w of (d['webhooks'] as Record<string, unknown>[] | null) ?? []) delete w['created_at'];
  return d;
}

const deviationRe = /cloudProviders:[\s\S]*\baws:|\bos:\s*["']?(macos|windows)\b/;
const deviationMsg = /cloudProviders\.aws is not supported|platform\.os (macos|windows) is not supported/;

function isYamlSyntax(m: string): boolean {
  return m.startsWith('invalid YAML') || m.startsWith('yaml:');
}

function errKey(e: ValidationError): string {
  return isYamlSyntax(e.message) ? `${e.file}|yaml` : `${e.file}|${e.line ?? 0}|${e.field ?? ''}|${e.message}`;
}

function sortedKeys(errs: ValidationError[] | null | undefined): string[] {
  return (errs ?? [])
    .map((e) => ({ ...e, message: e.message.replace('must be one of linux, macos, windows', 'must be one of linux') }))
    .map(errKey)
    .sort();
}

function hasDeviation(files: Record<string, string>): boolean {
  return Object.values(files).some((c) => deviationRe.test(c));
}

describe('examples', () => {
  const dirs = readdirSync(join(goldenDir, 'examples')).map((f) => f.replace(/\.json$/, ''));
  it('has a golden for every example tree', () => {
    const trees = readdirSync(join(repo, 'examples'), { withFileTypes: true })
      .filter((e) => e.isDirectory())
      .map((e) => e.name)
      .filter((n) => {
        try {
          readFileSync(join(repo, 'examples', n, 'factory.yaml'));
          return true;
        } catch {
          return false;
        }
      });
    expect(dirs.sort()).toEqual(trees.sort());
  });
  for (const name of dirs) {
    it(name, () => {
      const g = JSON.parse(readFileSync(join(goldenDir, 'examples', name + '.json'), 'utf8')) as Rec;
      const files = loadDir(join(repo, 'examples', name));
      const got = compile(files);
      if (hasDeviation(files)) {
        expect(got.errors.some((e) => deviationMsg.test(e.message))).toBe(true);
        return;
      }
      expect(sortedKeys(got.errors)).toEqual(sortedKeys(g.errors));
      expect(got.definition).toEqual(stripGo(g.compiled));
    });
  }
});

describe('compile corpus (every Compile call of the Go tests)', () => {
  const recs = corpus.filter((r) => r.kind === 'compile');
  it('replays every record', () => {
    const failures: string[] = [];
    let deviations = 0;
    for (const [i, r] of recs.entries()) {
      const files = r.files ?? {};
      const got = compile(files);
      if (hasDeviation(files) && got.errors.some((e) => deviationMsg.test(e.message))) {
        deviations++;
        continue;
      }
      const want = sortedKeys(r.errors);
      const have = sortedKeys(got.errors);
      if (JSON.stringify(want) !== JSON.stringify(have)) {
        failures.push(`#${i} errors\n  want ${JSON.stringify(want)}\n  got  ${JSON.stringify(have)}\n  files ${JSON.stringify(files).slice(0, 600)}`);
        continue;
      }
      const wantDef = stripGo(r.compiled);
      if (JSON.stringify(canon(got.definition)) !== JSON.stringify(canon(wantDef))) {
        failures.push(`#${i} definition differs\n  files ${JSON.stringify(files).slice(0, 400)}`);
      }
    }
    expect(failures.slice(0, 10).join('\n\n')).toBe('');
    // aws/macOS/windows inputs of the Go tests; every other record is equal.
    expect(deviations).toBeLessThan(15);
    expect(recs.length).toBeGreaterThan(300);
  });
});

function canon(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(canon);
  if (v !== null && typeof v === 'object') {
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(v).sort()) out[k] = canon((v as Record<string, unknown>)[k]);
    return out;
  }
  return v;
}

describe('diff corpus', () => {
  for (const [i, r] of corpus.filter((x) => x.kind === 'diff').entries()) {
    it(`diff #${i}`, () => {
      const got = diff(stripGo(r.old) as CompiledDefinition | null, stripGo(r.new) as CompiledDefinition | null);
      expect(got.changes).toEqual(r.changes ?? []);
      expect(diffSummary(got)).toBe(r.summary ?? 'No changes to the factory definition.');
    });
  }
});

describe('schema corpus', () => {
  it('agrees with the Go validator on valid and invalid documents', () => {
    const failures: string[] = [];
    for (const r of corpus.filter((x) => x.kind === 'schema')) {
      if (deviationRe.test(r.content ?? '')) continue;
      const got = validateSchema(r.path ?? '', r.content ?? '');
      if (got.valid !== r.valid) {
        failures.push(`${r.path}: valid=${got.valid}, Go ${r.valid}\n${r.content}\n${JSON.stringify(got.errors.slice(0, 3))}`);
        continue;
      }
      if (!r.valid && r.leaves && r.leaves.length > 0 && got.errors.length > 0 && got.errors[0]!.keyword !== 'yaml') {
        const goPaths = new Set(r.leaves.map((l) => l.instance));
        const tsPaths = got.errors.map((e) => e.instance);
        const overlap = tsPaths.some((p) => goPaths.has(p) || [...goPaths].some((g) => g.startsWith(p) || p.startsWith(g)));
        if (!overlap) failures.push(`${r.path}: no common error path (Go ${[...goPaths].join(',')}, TS ${tsPaths.join(',')})`);
      }
    }
    expect(failures.join('\n\n')).toBe('');
  });
});

describe('vectors', () => {
  const by = (k: string) => vectors.filter((v) => v.kind === k);
  it('cron', () => {
    for (const v of by('cron')) expect([v.input, validateCron(v.input as string) ?? '']).toEqual([v.input, v.output]);
  });
  it('policy references', () => {
    for (const v of by('resolve')) {
      const r = resolvePolicy(v.input as string);
      const got = r.error !== undefined ? { error: r.error } : { version: r.version, hosts: r.hosts };
      expect([v.input, got]).toEqual([v.input, v.output]);
    }
  });
  it('hosts', () => {
    for (const v of by('host')) {
      const r = normalizeHost(v.input as string);
      expect([v.input, r.error !== undefined ? { error: r.error } : { host: r.host }]).toEqual([v.input, v.output]);
    }
  });
  it('samplingRate rounding', () => {
    for (const v of by('round')) expect([v.input, roundHundredths(v.input as string, Number((v.input as string).replaceAll('_', '')))]).toEqual([v.input, v.output]);
  });
  it('webhook helpers', () => {
    for (const v of by('header')) expect([v.input, validDeliveryIdHeader(v.input as string)]).toEqual([v.input, v.output]);
    for (const v of by('urltoken')) expect([v.input, validURLTokenSecret(v.input as string)]).toEqual([v.input, v.output]);
  });
  it('repo at commit', () => {
    for (const v of by('repoat')) {
      const r = parseRepoAtCommit(v.input as string);
      const got = { ok: r !== null, ref: r ?? { codeForge: '', owner: '', repo: '', ref: '' } };
      expect([v.input, got]).toEqual([v.input, v.output]);
    }
  });
  it('trigger catalog', () => {
    for (const v of by('events')) expect([v.input, triggerEvents(v.input as string)]).toEqual([v.input, v.output]);
    expect(triggerProviders()).toEqual(by('providers')[0]!.output);
  });
  it('default definitions', () => {
    for (const v of by('default')) {
      const i = v.input as { name: string; description: string; alias: string; model: string; agents: string[] | null; repositories: { codeForge: string; owner: string; name: string }[] | null };
      let got: unknown;
      try {
        got = {
          files: defaultDefinition({
            name: i.name,
            description: i.description,
            alias: i.alias,
            model: i.model,
            agents: i.agents ?? [],
            repositories: i.repositories ?? [],
          }),
        };
      } catch (e) {
        got = { error: (e as { detail?: string }).detail ?? (e as Error).message };
      }
      expect(got).toEqual(v.output);
    }
  });
});
