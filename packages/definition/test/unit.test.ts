// Ports of the Go table tests of internal/definition that do not go through
// Compile (the golden corpus replays every Compile call), and tests of the
// changes of the port (no cloudProviders.aws, Linux machines only).

import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { compile, normalizePath } from '../src/compile.ts';
import { defaultAgents, defaultDefinition, DEFAULT_AGENT_NAMES } from '../src/defaults.ts';
import { diff, diffCounts, diffEmpty, diffSummary } from '../src/diff.ts';
import { render } from '../src/docsgen.ts';
import { goFloat, goQuote } from '../src/gofmt.ts';
import { harnessForModel, defaultModel, normalizeType } from '../src/harness-models.ts';
import { keyLine } from '../src/locate.ts';
import { loadDir } from '../src/node.ts';
import { SCHEMA_BASE_URL, SCHEMA_NAMES, schema, schemaForPath, schemas, schemaVersions } from '../src/schema.ts';
import { generatedSchemas, goJSONIndent } from '../src/schemagen.ts';
import { validateSchema } from '../src/validate-schema.ts';
import { DEFAULT_AGENT_FILES } from '../src/default-agents.generated.ts';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '..', '..', '..');

const baseFactoryHead = 'schemaVersion: v1\nname: test\nrepositories:\n  - owner: acme\n    name: api\n';
const baseFactory = baseFactoryHead + 'agentDefaults:\n  model: claude-opus-5-5\n';
const foremanMD = '---\nagentType: FOREMAN\n---\nCoordinate the work.\n';
const linuxMachine = 'platform:\n  os: linux\n  linux:\n    image: ubuntu:24.04\n';
const agentMD = (fm: string, body = 'Do the work.') => '---\n' + fm + '---\n' + body + '\n';
const automationMD = (fm: string) => '---\n' + fm + '---\nHandle the event.\n';

function defTree(kv: Record<string, string | null> = {}): Record<string, string> {
  const f: Record<string, string> = { 'factory.yaml': baseFactory, 'agents/foreman/agent.md': foremanMD };
  for (const [k, v] of Object.entries(kv)) {
    if (v === null) delete f[k];
    else f[k] = v;
  }
  return f;
}

function mustCompile(files: Record<string, string>) {
  const r = compile(files);
  expect(r.errors).toEqual([]);
  expect(r.definition).not.toBeNull();
  return r.definition!;
}

describe('compile', () => {
  it('compiles the minimal definition', () => {
    const def = mustCompile(defTree());
    expect(def.schemaVersion).toBe('v1');
    expect(def.githubOnboardingNotices).toBe(true);
    expect(def.factoryConfigChecks).toBe('main');
    expect(def.integrationsSet).toBe(false);
    expect(def.repositories![0]!.codeForge).toBe('GITHUB');
    const f = def.agents!.find((a) => a.name === 'foreman')!;
    expect(f.type).toBe('FOREMAN');
    expect(f.instructions).toBe('Coordinate the work.');
    expect(f.harness).toEqual({ type: 'claude', model: 'claude-opus-5-5' });
    expect(f.spawnable_by).toBeNull();
    expect(f.network_policy.restricted).toBe(false);
  });

  it('rejects cloudProviders.aws', () => {
    const r = compile(defTree({ 'factory.yaml': baseFactory + 'cloudProviders:\n  aws:\n    roleArn: arn:aws:iam::1:role/x\n' }));
    expect(r.definition).toBeNull();
    expect(r.errors).toEqual([
      {
        file: 'factory.yaml',
        line: 9,
        field: 'cloudProviders.aws',
        message: 'cloudProviders.aws is not supported; use cloudProviders.gcp, or cloudProviders.audiences for a service that accepts OIDC tokens',
      },
    ]);
  });

  it.each(['macos', 'windows'])('rejects platform.os %s', (os) => {
    const r = compile(defTree({ 'machines/m.yaml': `platform:\n  os: ${os}\n  linux:\n    image: x\n` }));
    expect(r.errors).toEqual([
      { file: 'machines/m.yaml', line: 2, field: 'platform.os', message: `platform.os ${os} is not supported; machines run linux only` },
    ]);
  });

  it('accepts a Linux machine with aarch64', () => {
    const def = mustCompile(defTree({ 'machines/m.yaml': 'platform:\n  arch: aarch64\n  linux:\n    image: x\n' }));
    expect(def.machines!['m']).toEqual({ name: 'm', os: 'linux', arch: 'aarch64', image: 'x' });
  });

  it('keeps a pointer field with an empty value in automation overrides', () => {
    const def = mustCompile(
      defTree({
        'automations/a/automation.md': automationMD(
          'environmentId: ""\nworkerHost: ""\nidleTimeoutMinutes: 0\ntriggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: "@daily"}}\n',
        ),
      }),
    );
    expect(def.automations![0]!.overrides).toEqual({ environment_id: '', secrets: null, mcp_servers: null, worker_host: '', idle_timeout_minutes: 0 });
  });

  it('reports YAML syntax errors with a line', () => {
    const r = compile(defTree({ 'factory.yaml': baseFactory + '  - : [\n' }));
    expect(r.errors[0]!.file).toBe('factory.yaml');
    expect(r.errors[0]!.message.startsWith('invalid YAML:')).toBe(true);
  });

  it('accepts __proto__ as an environment key', () => {
    const def = mustCompile(defTree({ 'factory.yaml': baseFactory + 'mcpServers:\n  s:\n    command: x\n    env:\n      __proto__: v\n' }));
    expect(Object.keys(def.mcpServers!['s']!.env!)).toEqual(['__proto__']);
  });
});

describe('normalizePath', () => {
  it.each([
    ['a/b', 'a/b'],
    ['./a//b/', 'a/b'],
    ['..', ''],
    ['a/..', ''],
    ['a\\b', 'a/b'],
    ['../x', 'x'],
    ['', ''],
  ])('%s', (p, want) => expect(normalizePath(p)).toBe(want));
});

describe('keyLine', () => {
  const agent = '---\ndescription: x\nenvironmentId: env_x\nmcpServers:\n  docs:\n    managedId: m\n---\nBody\n';
  const auto =
    '---\ntriggers:\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids: [a]\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids:\n        in:\n          - b\n---\nGo.\n';
  const yml = 'agentDefaults:\n  environmentId: env_y\nsecrets: [A, B]\n';
  it.each([
    ['frontmatter key', 'agents/a/agent.md', agent, 'environmentId', 3],
    ['nested key', 'agents/a/agent.md', agent, 'mcpServers.docs.managedId', 6],
    ['list index', 'automations/x/automation.md', auto, 'triggers[0].filter.webhook_ids[0]', 6],
    ['in list item', 'automations/x/automation.md', auto, 'triggers[1].filter.webhook_ids.in[0]', 12],
    ['yaml file', 'factory.yaml', yml, 'agentDefaults.environmentId', 2],
    ['yaml list key', 'factory.yaml', yml, 'secrets', 3],
    ['missing key', 'factory.yaml', yml, 'nope', 0],
    ['index out of range', 'factory.yaml', yml, 'secrets[5]', 0],
    ['bad path', 'factory.yaml', yml, 'secrets[x]', 0],
    ['empty path', 'factory.yaml', yml, '', 0],
    ['no frontmatter', 'agents/a/agent.md', 'just text\n', 'environmentId', 0],
    ['bad yaml', 'factory.yaml', 'a: [\n', 'a', 0],
  ])('%s', (_n, file, content, path, want) => expect(keyLine(file, content, path)).toBe(want));
});

describe('diff', () => {
  const old = mustCompile(
    defTree({
      'machines/linux.yaml': linuxMachine,
      'agents/reviewer/agent.md': agentMD('agentType: REVIEW\nmodel: claude-sonnet-5-5\n'),
      'agents/gone/agent.md': agentMD(''),
      'automations/a/automation.md': automationMD('triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: "@daily"}}\n'),
      'skills/s/SKILL.md': '---\nname: s\ndescription: d\n---\nx\n',
    }),
  );
  const next = mustCompile(
    defTree({
      'factory.yaml': baseFactory + 'alias: newalias\n',
      'machines/linux.yaml': linuxMachine,
      'agents/reviewer/agent.md': agentMD('agentType: REVIEW\nmodel: claude-haiku-5-5\n', 'New instructions.'),
      'agents/added/agent.md': agentMD(''),
      'automations/a/automation.md': automationMD('triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: "@hourly"}}\n'),
      'webhooks/w.yaml': 'secretName: S\n',
    }),
  );
  it('reports added, removed and changed resources', () => {
    const d = diff(old, next);
    const got = Object.fromEntries(d.changes.map((c) => [c.kind + '/' + c.name, c]));
    expect(Object.fromEntries(Object.entries(got).map(([k, c]) => [k, c.action]))).toEqual({
      'factory/test': 'changed',
      'agent/added': 'added',
      'agent/gone': 'removed',
      'agent/reviewer': 'changed',
      'automation/a': 'changed',
      'webhook/w': 'added',
      'skill/s': 'removed',
    });
    expect(got['agent/reviewer']!.fields).toEqual(['harness', 'instructions']);
    expect(got['factory/test']!.fields).toEqual(['alias']);
    expect(diffCounts(d)).toEqual({ added: 2, removed: 2, changed: 3 });
    const sum = diffSummary(d);
    for (const want of ['2 added, 2 removed, 3 changed', '+ agent added', '- agent gone', '~ agent reviewer (harness, instructions)']) {
      expect(sum).toContain(want);
    }
  });
  it('is empty for the same definition', () => {
    expect(diffEmpty(diff(old, old))).toBe(true);
    expect(diffSummary(diff(old, old))).toBe('No changes to the factory definition.');
    expect(diff(null, old).changes.length).toBeGreaterThan(0);
  });
});

describe('schemas', () => {
  it('generator output equals schemas/definition/v1', () => {
    for (const [name, s] of Object.entries(generatedSchemas())) {
      const file = readFileSync(join(repo, 'schemas', 'definition', 'v1', name + '.schema.json'), 'utf8');
      expect(goJSONIndent(s), name).toBe(file);
    }
  });
  it('has the catalog', () => {
    const all = schemas();
    expect(Object.keys(all).length).toBe(SCHEMA_NAMES.length);
    for (const [name, raw] of Object.entries(all)) {
      const doc = JSON.parse(raw) as Record<string, unknown>;
      expect(doc['$schema']).toBe('https://json-schema.org/draft/2020-12/schema');
      expect(doc['$id']).toBe(SCHEMA_BASE_URL + name);
    }
    for (const n of ['factory', 'factory.schema.json', 'v1/factory.schema.json']) expect(schema(n)).not.toBeNull();
    expect(schema('nope')).toBeNull();
    expect(schemaVersions()).toEqual({ current: 'v1', versions: ['v1'] });
  });
  it.each([
    ['factory.yaml', 'factory'],
    ['agents/a/agent.md', 'agent'],
    ['automations/a/automation.md', 'automation'],
    ['scorers/s/scorer.md', 'scorer'],
    ['machines/r.yaml', 'machine'],
    ['routers/r.yaml', ''],
    ['webhooks/w.yaml', 'webhook'],
    ['benchmarks/b/suite.yaml', 'benchmark_suite'],
    ['benchmarks/b/tasks/t.yaml', 'benchmark_suite_task'],
    ['skills/s/SKILL.md', ''],
    ['README.md', ''],
    ['agents/a/skills/s/SKILL.md', ''],
  ])('schemaForPath(%s)', (p, want) => expect(schemaForPath(p)).toBe(want));
  it('validates the examples', () => {
    for (const ex of ['00-default-agents', '01-single-repo-quickstart', '03-multi-harness', '04-code-review-only', '05-incident-response', '06-common-automations', '07-self-hosted-worker']) {
      const files = loadDir(join(repo, 'examples', ex));
      for (const [p, c] of Object.entries(files)) {
        const r = validateSchema(p, c);
        expect(r.errors, `${ex}/${p}`).toEqual([]);
      }
    }
  });
  it('rejects macOS and aws documents', () => {
    expect(validateSchema('machines/r.yaml', 'platform: {os: macos, linux: {image: x}}\n').valid).toBe(false);
    expect(validateSchema('factory.yaml', baseFactory + 'cloudProviders: {aws: {roleArn: "arn:a:b:c:d:e"}}\n').valid).toBe(false);
    expect(validateSchema('factory.yaml', 'a: [\n').valid).toBe(false);
    expect(validateSchema('agents/a/agent.md', 'Just instructions.\n').valid).toBe(true);
  });
});

describe('docs generator', () => {
  it('renders every generated section and is idempotent', () => {
    const md = readFileSync(join(repo, 'docs', 'definition-reference.md'), 'utf8');
    const s: Record<string, Record<string, unknown>> = {};
    for (const n of SCHEMA_NAMES) s[n] = JSON.parse(schema(n)!) as Record<string, unknown>;
    const once = render(md, s);
    expect(render(once, s)).toBe(once);
    expect(once).not.toContain('| `aws.roleArn` |');
    expect(once).not.toContain('| `platform.mac` |');
  });
});

describe('defaults', () => {
  it('default agents equal examples/00-default-agents', () => {
    const files = loadDir(join(repo, 'examples', '00-default-agents'));
    const d = defaultAgents();
    expect(Object.keys(d).length).toBe(DEFAULT_AGENT_NAMES.length);
    for (const [p, c] of Object.entries(d)) expect(files[p], p).toBe(c);
  });
  it('embedded agents equal defaults/agents', () => {
    for (const [p, c] of Object.entries(DEFAULT_AGENT_FILES)) expect(readFileSync(join(here, '..', 'defaults', p), 'utf8')).toBe(c);
  });
  it('default definition compiles', () => {
    const def = mustCompile(defaultDefinition({ name: 'payments', alias: 'payments', repositories: [{ owner: 'acme', name: 'api' }, { owner: 'acme', name: 'web' }] }));
    expect(def.agents!.length).toBe(5);
    const impl = def.agents!.find((a) => a.type === 'IMPLEMENT')!.harness.model;
    const rev = def.agents!.find((a) => a.type === 'REVIEW')!.harness.model;
    expect(impl).not.toBe('');
    expect(impl).not.toBe(rev);
    expect(() => defaultDefinition({ name: '' })).toThrow(/factory name is required/);
    expect(() => defaultDefinition({ name: 'x', agents: ['nope'] })).toThrow(/unknown default agent "nope"/);
  });
});

describe('loadDir', () => {
  it('skips hidden files and directories', () => {
    const dir = mkdtempSync(join(tmpdir(), 'of-def-'));
    const must = (p: string, c: string) => {
      mkdirSync(dirname(join(dir, p)), { recursive: true });
      writeFileSync(join(dir, p), c);
    };
    must('factory.yaml', 'x');
    must('agents/a/agent.md', 'y');
    must('.git/config', 'z');
    must('.hidden', 'z');
    expect(loadDir(dir)).toEqual({ 'factory.yaml': 'x', 'agents/a/agent.md': 'y' });
    expect(() => loadDir(join(dir, 'missing'))).toThrow();
    expect(() => loadDir(join(dir, 'factory.yaml'))).toThrow();
  });
});

describe('helpers', () => {
  it.each([
    ['a"b\\', '"a\\"b\\\\"'],
    ['tab\there', '"tab\\there"'],
    ['\x01', '"\\x01"'],
    ['é', '"é"'],
    ['\u00ad', '"\\u00ad"'],
  ])('goQuote(%j)', (s, want) => expect(goQuote(s)).toBe(want));
  it.each([
    [0.5, '0.5'],
    [-5, '-5'],
    [1e6, '1e+06'],
    [123456, '123456'],
    [1e-7, '1e-07'],
    [0.0001, '0.0001'],
    [1234567.5, '1.2345675e+06'],
  ])('goFloat(%d)', (f, want) => expect(goFloat(f)).toBe(want));
  it('selects harnesses by model name', () => {
    expect(harnessForModel('claude-opus-5-5')).toBe('claude');
    expect(harnessForModel('Anthropic.claude-x')).toBe('claude');
    expect(harnessForModel('gpt-5')).toBe('codex');
    expect(harnessForModel('o3')).toBe('codex');
    expect(harnessForModel('auto')).toBe('');
    expect(normalizeType('claude-code')).toBe('claude');
    expect(defaultModel('codex')).toBe('gpt-5.3-codex');
  });
});
