// Field tables of docs/definition-reference.md from the JSON Schemas. Port of
// scripts/definition-docs/main.go. cloudProviders.aws and the macOS settings
// of machines are removed.

import { goQuote } from './gofmt.ts';

type Obj = Record<string, unknown>;

interface Section {
  doc: string;
  pointer?: string;
  prefix?: string;
  expand?: string[];
  order?: string[];
}

export const sections: Record<string, Section> = {
  factory: {
    doc: 'factory',
    expand: ['spendLimit'],
    order: [
      'schemaVersion', 'name', 'description', 'alias', 'credentialStrategy', 'maxConcurrentRuns', 'approvers',
      'restrictFollowupsToStarter', 'requireLinkedActor', 'allowAgentMerge', 'spendLimit', 'spendLimit.daily', 'spendLimit.monthly',
      'codeForges', 'repositories', 'benchmarkRepoSubstitutions', 'secrets', 'networkPolicy', 'mcpServers',
      'cloudProviders', 'integrations', 'automations', 'github', 'scorerDefaults', 'selfImprovement', 'agentDefaults',
    ],
  },
  'factory.repositories': { doc: 'factory', pointer: '/properties/repositories/items', order: ['codeForge', 'owner', 'name'] },
  'factory.benchmarkRepoSubstitutions': {
    doc: 'factory',
    pointer: '/properties/benchmarkRepoSubstitutions/items',
    expand: ['source', 'target'],
    order: ['source', 'source.codeForge', 'source.owner', 'source.name', 'target', 'target.codeForge', 'target.owner', 'target.name'],
  },
  'factory.cloudProviders': {
    doc: 'factory',
    pointer: '/properties/cloudProviders',
    expand: ['gcp'],
    order: ['gcp', 'gcp.projectNumber', 'gcp.workloadIdentityFederationPoolId', 'gcp.workloadIdentityFederationProviderId', 'gcp.serviceAccountEmail', 'audiences'],
  },
  'factory.integrations': {
    doc: 'factory',
    pointer: '/properties/integrations/anyOf/0/items',
    expand: ['slack', 'microsoft-teams', 'linear', 'jira'],
    order: ['type', 'slack', 'microsoft-teams', 'linear', 'jira'],
  },
  'factory.automations': { doc: 'factory', pointer: '/properties/automations', prefix: 'automations.' },
  'factory.github': { doc: 'factory', pointer: '/properties/github', prefix: 'github.' },
  'factory.scorerDefaults': { doc: 'factory', pointer: '/properties/scorerDefaults', order: ['harness', 'model', 'machine', 'secrets', 'mcpServers'] },
  'factory.selfImprovement': {
    doc: 'factory',
    pointer: '/properties/selfImprovement',
    order: ['failedRunThreshold', 'reviewerType', 'reviewerEmails', 'analysisModel'],
  },
  'factory.agentDefaults': {
    doc: 'factory',
    pointer: '/properties/agentDefaults',
    order: ['model', 'harness', 'machine', 'environmentId', 'secrets', 'mcpServers', 'workerHost'],
  },
  harness: { doc: 'common', pointer: '/$defs/harness', order: ['type', 'model', 'reasoningLevel', 'auth'] },
  harnessAuth: { doc: 'common', pointer: '/$defs/harnessAuth', order: ['source', 'secretName'] },
  agent: {
    doc: 'agent',
    order: [
      'description', 'agentType', 'credentialStrategy', 'spawnableBy', 'networkPolicy', 'snapshot',
      'model', 'harness', 'machine', 'environmentId', 'secrets', 'mcpServers', 'workerHost', 'idleTimeoutMinutes',
    ],
  },
  automation: {
    doc: 'automation',
    order: [
      'enabled', 'displayName', 'agent', 'runAs', 'triggers',
      'model', 'harness', 'machine', 'environmentId', 'secrets', 'mcpServers', 'workerHost', 'idleTimeoutMinutes',
    ],
  },
  'automation.trigger': {
    doc: 'automation',
    pointer: '/$defs/trigger',
    expand: ['schedule'],
    order: ['provider', 'event', 'filter', 'schedule', 'schedule.cron', 'schedule.name', 'concurrency'],
  },
  machine: {
    doc: 'machine',
    expand: ['instanceShape', 'platform', 'platform.linux'],
    order: [
      'description', 'setupCommands', 'failureSessionRetentionMinutes', 'instanceShape', 'instanceShape.vcpus', 'instanceShape.memoryGb',
      'platform', 'platform.os', 'platform.arch', 'platform.linux', 'platform.linux.image',
      'platform.linux.registryCredentialSecretName', 'platform.linux.user',
    ],
  },
  webhook: { doc: 'webhook', order: ['authMode', 'signatureScheme', 'secretName', 'deliveryIdHeader', 'enabled'] },
  scorer: {
    doc: 'scorer',
    expand: ['labels'],
    order: [
      'name', 'description', 'output', 'agents', 'labels', 'labels[].value', 'labels[].description', 'labels[].score',
      'passingScore', 'samplingRate', 'model', 'harness', 'selfImprovement', 'machine', 'secrets', 'mcpServers',
    ],
  },
  benchmark_suite: {
    doc: 'benchmark_suite',
    expand: ['configurations', 'configurations[].agents'],
    order: [
      'name', 'description', 'agent', 'configurations', 'configurations[].name', 'configurations[].agents',
      'configurations[].agents[].agent', 'configurations[].agents[].model', 'configurations[].agents[].harness',
      'configurations[].machine', 'tasks',
    ],
  },
  benchmark_suite_task: {
    doc: 'benchmark_suite_task',
    order: ['title', 'prompt', 'successCriteria', 'sourceRunId', 'tags', 'trackerSeed', 'linearSeed', 'startingRepoRefs'],
  },
  mcpServer: { doc: 'common', pointer: '/$defs/mcpServer', order: ['managedId', 'command', 'args', 'env', 'url', 'headers', 'allowWrite'] },
  networkPolicy: { doc: 'common', pointer: '/$defs/networkPolicy', order: ['extends', 'allowedHosts', 'deniedHosts'] },
};

const defLabels: Record<string, string> = {
  resourceName: 'a [resource name](#naming-rules)',
  secretName: 'a [secret name](#secret-names)',
  secretList: '[secret names](#secret-names), no duplicates',
  harness: 'a complete [harness object](#harness-and-model)',
  harnessOverride: 'a sparse [harness object](#harness-and-model)',
  harnessAuth: 'an [auth object](#harness-and-model)',
  mcpServers: '[MCP server objects](#mcp-server-object) by name; the name `factory` is reserved',
  networkPolicy: 'a [networkPolicy object](#networkpolicy-object)',
  host: 'host names or literal IP addresses',
  trigger: '[trigger objects](#automationsnameautomationmd)',
};

const beginRe = /^<!-- BEGIN GENERATED: (\S+) \(make docs-gen\) -->$/;

function trimEOL(s: string): string {
  return s.replace(/[\r\n]+$/, '');
}

/** render replaces the content of every generated section of md. */
export function render(md: string, schemas: Record<string, Obj>): string {
  const lines = md.split(/(?<=\n)/);
  let b = '';
  const used = new Set<string>();
  for (let i = 0; i < lines.length; i++) {
    b += lines[i];
    const m = beginRe.exec(trimEOL(lines[i]!));
    if (!m) continue;
    const id = m[1]!;
    const sec = sections[id];
    if (!sec) throw new Error(`line ${i + 1}: unknown generated section ${goQuote(id)}`);
    if (used.has(id)) throw new Error(`line ${i + 1}: generated section ${goQuote(id)} occurs twice`);
    used.add(id);
    const end = '<!-- END GENERATED: ' + id + ' -->';
    let j = i + 1;
    while (j < lines.length && trimEOL(lines[j]!) !== end) {
      if (beginRe.test(trimEOL(lines[j]!))) throw new Error(`line ${i + 1}: section ${goQuote(id)} has no end marker`);
      j++;
    }
    if (j === lines.length) throw new Error(`line ${i + 1}: section ${goQuote(id)} has no end marker ${goQuote(end)}`);
    let table: string;
    try {
      table = new Generator(schemas).table(sec);
    } catch (e) {
      throw new Error(`section ${goQuote(id)}: ${(e as Error).message}`);
    }
    b += table + lines[j];
    i = j;
  }
  const missing = Object.keys(sections)
    .filter((id) => !used.has(id))
    .sort();
  if (missing.length > 0) throw new Error(`no markers for the sections ${missing.join(', ')}`);
  return b;
}

interface Node {
  doc: string;
  s: Obj;
}

interface Row {
  field: string;
  typ: string;
  required: string;
  def: string;
  allowed: string;
  desc: string;
}

class Generator {
  schemas: Record<string, Obj>;
  constructor(schemas: Record<string, Obj>) {
    this.schemas = schemas;
  }

  table(sec: Section): string {
    const root = this.schemas[sec.doc];
    if (!root) throw new Error(`unknown schema ${goQuote(sec.doc)}`);
    const n = this.unwrap(this.pointer({ doc: sec.doc, s: root }, sec.pointer ?? ''));
    const expand = new Set(sec.expand ?? []);
    const order = new Map((sec.order ?? []).map((o, i) => [o, i]));
    const seen = new Set<string>();
    const rows: Row[] = [];
    this.rows(n, '', sec, expand, order, seen, rows);
    for (const e of sec.expand ?? []) if (!seen.has(e)) throw new Error(`expand ${goQuote(e)} names no object field`);
    for (const o of sec.order ?? []) if (!seen.has(o)) throw new Error(`order lists ${goQuote(o)}, which is not a field`);
    let b = '| Field | Type | Required | Default | Allowed values | Description |\n|---|---|---|---|---|---|\n';
    for (const r of rows) {
      b += `| \`${r.field}\` | ${cell(r.typ)} | ${cell(r.required)} | ${cell(r.def)} | ${cell(r.allowed)} | ${cell(text(r.desc))} |\n`;
    }
    return b;
  }

  rows(n: Node, path: string, sec: Section, expand: Set<string>, order: Map<string, number>, seen: Set<string>, out: Row[]): void {
    const props = (n.s['properties'] ?? {}) as Record<string, Obj>;
    if (Object.keys(props).length === 0) throw new Error(`${goQuote(path)} is not an object with properties`);
    const required = new Set(((n.s['required'] ?? []) as unknown[]).map((r) => String(r)));
    const full = (k: string): string => path + k;
    const names = Object.keys(props).sort((a, b) => {
      const oa = order.get(full(a));
      const ob = order.get(full(b));
      if (oa !== undefined && ob !== undefined) return oa - ob;
      if ((oa !== undefined) !== (ob !== undefined)) return oa !== undefined ? -1 : 1;
      return a < b ? -1 : a > b ? 1 : 0;
    });
    for (const k of names) {
      const p: Node = { doc: n.doc, s: props[k]! };
      const name = full(k);
      seen.add(name);
      const desc = typeof p.s['description'] === 'string' ? (p.s['description'] as string) : '';
      if (desc === '') throw new Error(`field ${goQuote(name)} has no description in the schema`);
      out.push({
        field: (sec.prefix ?? '') + name,
        typ: this.typeOf(p),
        required: required.has(k) ? 'yes' : '',
        def: defaultOf(p.s),
        allowed: this.allowed(p),
        desc,
      });
      if (!expand.has(name)) continue;
      let child = this.unwrap(this.resolve(p));
      let childPath = name + '.';
      if (child.s['type'] === 'array') {
        const items = child.s['items'];
        if (typeof items !== 'object' || items === null) throw new Error(`${goQuote(name)} has no items schema`);
        child = this.unwrap(this.resolve({ doc: child.doc, s: items as Obj }));
        childPath = name + '[].';
      }
      this.rows(child, childPath, sec, expand, order, seen, out);
    }
  }

  pointer(n: Node, ptr: string): Node {
    if (ptr === '') return n;
    let cur: unknown = n.s;
    for (let tok of ptr.replace(/^\//, '').split('/')) {
      tok = tok.replaceAll('~1', '/').replaceAll('~0', '~');
      if (Array.isArray(cur)) {
        const i = Number(tok);
        if (!/^[0-9]+$/.test(tok) || i >= cur.length) throw new Error(`pointer ${ptr}: bad index ${goQuote(tok)}`);
        cur = cur[i];
      } else if (cur !== null && typeof cur === 'object') {
        cur = (cur as Obj)[tok];
      } else {
        cur = undefined;
      }
      if (cur === undefined || cur === null) throw new Error(`pointer ${ptr} of ${n.doc} does not resolve`);
    }
    if (typeof cur !== 'object' || Array.isArray(cur)) throw new Error(`pointer ${ptr} of ${n.doc} is not a schema`);
    return { doc: n.doc, s: cur as Obj };
  }

  resolve(n: Node): Node {
    for (let i = 0; i < 10; i++) {
      const ref = n.s['$ref'];
      if (typeof ref !== 'string') return n;
      const hash = ref.indexOf('#');
      const doc = hash >= 0 ? ref.slice(0, hash) : ref;
      const ptr = hash >= 0 ? ref.slice(hash + 1) : '';
      const target = doc !== '' ? doc.replace(/\.schema\.json$/, '') : n.doc;
      try {
        n = this.pointer({ doc: target, s: this.schemas[target] ?? {} }, ptr);
      } catch {
        return n;
      }
    }
    return n;
  }

  unwrap(n0: Node): Node {
    const n = this.resolve(n0);
    const bs = branches(n);
    if (bs.length !== 2) return n;
    if (isNullNode(bs[1]!)) return this.resolve(bs[0]!);
    if (isNullNode(bs[0]!)) return this.resolve(bs[1]!);
    return n;
  }

  typeOf(n0: Node): string {
    const n = this.resolve(n0);
    const bs = branches(n);
    if (bs.length > 0) {
      const parts: string[] = [];
      for (const b of bs) {
        const t = this.typeOf(b);
        if (!parts.includes(t)) parts.push(t);
      }
      return parts.join(' or ');
    }
    if ('const' in n.s) return jsonType(n.s['const']);
    const t = n.s['type'];
    if (typeof t === 'string') {
      if (t === 'array') {
        const items = n.s['items'];
        if (items !== null && typeof items === 'object' && !Array.isArray(items)) {
          const it = this.typeOf({ doc: n.doc, s: items as Obj });
          if (['string', 'integer', 'number', 'boolean', 'object'].includes(it)) return 'list of ' + it + 's';
        }
        return 'list';
      }
      if (t === 'object') {
        const ap = n.s['additionalProperties'];
        if (ap !== null && typeof ap === 'object' && !Array.isArray(ap)) {
          return this.typeOf({ doc: n.doc, s: ap as Obj }) === 'string' ? 'map of string to string' : 'map of objects';
        }
        return 'object';
      }
      return t;
    }
    if (Array.isArray(t)) return t.map((x) => String(x)).join(' or ');
    return 'any';
  }

  allowed(n: Node): string {
    const name = refName(n);
    if (name !== '') {
      const l = defLabels[name];
      if (l !== undefined) return l;
      return this.allowed(this.resolve(n));
    }
    const bs = branches(n);
    if (bs.length > 0) {
      const parts: string[] = [];
      for (const b of bs) {
        if (isNullNode(b)) continue;
        let a = this.allowed(b);
        if (a === '') {
          a = 'any ' + this.typeOf(b);
          if (this.resolve(b).s['properties'] !== undefined) a = 'an object';
        }
        parts.push(a);
      }
      if (parts.length === 1 && parts[0]!.startsWith('any ')) return '';
      return parts.join('; or ');
    }
    const parts: string[] = [];
    if ('const' in n.s) parts.push(literal(n.s['const']));
    if (Array.isArray(n.s['enum'])) parts.push((n.s['enum'] as unknown[]).map(literal).join(', '));
    if (n.s['minLength'] === 1 && !('pattern' in n.s)) parts.push('non-empty');
    if (typeof n.s['pattern'] === 'string') parts.push('matches ' + code(n.s['pattern'] as string));
    const r = rangeText(n.s, 'minimum', 'maximum');
    if (r !== '') parts.push(r);
    if (typeof n.s['exclusiveMinimum'] === 'number') parts.push('more than ' + num(n.s['exclusiveMinimum'] as number));
    if (n.s['type'] === 'array') {
      const c = countText(n.s);
      if (c !== '') parts.push(c);
      if (n.s['uniqueItems'] === true) parts.push('no duplicates');
      const items = n.s['items'];
      if (items !== null && typeof items === 'object' && !Array.isArray(items)) {
        const a = this.allowed({ doc: n.doc, s: items as Obj });
        if (a !== '') parts.push('each entry: ' + a);
      }
    } else if (n.s['type'] === 'object') {
      const ap = n.s['additionalProperties'];
      if (ap !== null && typeof ap === 'object' && !Array.isArray(ap)) {
        const a = this.allowed({ doc: n.doc, s: ap as Obj });
        if (a !== '') parts.push('each value: ' + a);
      }
    }
    return parts.join('; ');
  }
}

function refName(n: Node): string {
  const ref = n.s['$ref'];
  if (typeof ref !== 'string') return '';
  const i = ref.lastIndexOf('/');
  return i >= 0 ? ref.slice(i + 1) : '';
}

function branches(n: Node): Node[] {
  for (const k of ['anyOf', 'oneOf']) {
    const list = n.s[k];
    if (Array.isArray(list) && n.s['type'] === undefined && n.s['properties'] === undefined) {
      return list.filter((b) => b !== null && typeof b === 'object').map((b) => ({ doc: n.doc, s: b as Obj }));
    }
  }
  return [];
}

function isNullNode(n: Node): boolean {
  return n.s['type'] === 'null';
}

function jsonType(v: unknown): string {
  if (typeof v === 'string') return 'string';
  if (typeof v === 'boolean') return 'boolean';
  if (typeof v === 'number') return 'number';
  return 'any';
}

function rangeText(s: Obj, minKey: string, maxKey: string): string {
  const lo = s[minKey];
  const hi = s[maxKey];
  const hasMin = typeof lo === 'number';
  const hasMax = typeof hi === 'number';
  if (hasMin && hasMax) return num(lo) + '–' + num(hi);
  if (hasMin) return num(lo) + ' or more';
  if (hasMax) return 'at most ' + num(hi);
  return '';
}

function countText(s: Obj): string {
  const lo = s['minItems'];
  const hi = s['maxItems'];
  const hasMin = typeof lo === 'number';
  const hasMax = typeof hi === 'number';
  if (hasMin && hasMax && lo === hi) return 'exactly ' + entries(lo);
  if (hasMin && hasMax) return num(lo) + '–' + num(hi) + ' entries';
  if (hasMin) return 'at least ' + entries(lo);
  if (hasMax) return 'at most ' + entries(hi);
  return '';
}

function entries(n: number): string {
  return n === 1 ? '1 entry' : num(n) + ' entries';
}

function num(f: number): string {
  return String(f);
}

const goFloatSyntax = /^[+-]?((\d[\d_]*(\.[\d_]*)?|\.\d[\d_]*)([eE][+-]?\d+)?|0[xX][0-9a-fA-F_]*\.?[0-9a-fA-F_]*([pP][+-]?\d+)?|inf|infinity|nan)$/i;

function literal(v: unknown): string {
  if (typeof v === 'string') return goFloatSyntax.test(v) ? code(goQuote(v)) : code(v);
  if (typeof v === 'number') return code(num(v));
  if (v === null || v === undefined) return code('null');
  return code(String(v));
}

function code(s: string): string {
  return s.includes('`') ? '`` ' + s + ' ``' : '`' + s + '`';
}

function defaultOf(s: Obj): string {
  return 'default' in s ? literal(s['default']) : '';
}

function text(s: string): string {
  return s.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

function cell(s: string): string {
  return s.replaceAll('\n', ' ').replaceAll('|', '\\|');
}
