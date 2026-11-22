// Trigger catalog and trigger parser. Port of internal/definition/triggers.go.

import { validateCron } from './cron.ts';
import { goQuote, goTrimSpace } from './gofmt.ts';
import type { Schedule, Trigger } from './types.ts';
import { type Doc, indexPath, isNull, put, resolve, sprintNode, toValue, type YNode } from './yamlnode.ts';

/** How a filter key's values are validated. */
export const ValueKind = {
  String: 0,
  UUID: 1,
  Int: 2,
  Enum: 3,
  SkipBots: 4,
  InOnlyOne: 5,
  InOnlyMany: 6,
  WebhookIDs: 7,
  Payload: 8,
  NameAlias: 9,
  NameAliasInt: 10,
} as const;
export type ValueKind = (typeof ValueKind)[keyof typeof ValueKind];

export interface KeySpec {
  kind: ValueKind;
  values?: string[];
}

export interface AliasSpec {
  native: string;
  /** true: a rename of the native key; false: a name lookup alias. */
  rename: boolean;
  kind: ValueKind;
}

export type FilterRule = 'optional' | 'required' | 'forbidden';

export interface ProviderSpec {
  events: Map<string, string[]>;
  keys: Map<string, KeySpec>;
  aliases: Map<string, AliasSpec>;
  managed: Set<string>;
  unavailable: Map<string, string>;
  requireKeys: string[];
  filter: FilterRule;
}

export const PROVIDER_GITHUB = 'github';
export const PROVIDER_GITLAB = 'gitlab';
export const PROVIDER_AZURE_DEVOPS = 'azure_devops';
export const PROVIDER_SLACK = 'slack';
export const PROVIDER_TEAMS = 'microsoft_teams';
export const PROVIDER_LINEAR = 'linear';
export const PROVIDER_JIRA = 'jira';
export const PROVIDER_SCHEDULE = 'schedule';
export const PROVIDER_WEBHOOK = 'webhook';

/** WEBHOOK_UID_PREFIX starts the UIDs that the server gives to webhooks. */
export const WEBHOOK_UID_PREFIX = 'whk_';

export const SCHEDULE_CONCURRENCY_ALLOW = 'allow';
export const SCHEDULE_CONCURRENCY_SKIP = 'skip';

function spec(p: Partial<ProviderSpec> & { events: Map<string, string[]> }): ProviderSpec {
  return {
    keys: new Map(),
    aliases: new Map(),
    managed: new Set(),
    unavailable: new Map(),
    requireKeys: [],
    filter: 'optional',
    ...p,
  };
}

function events(m: Map<string, string[]>, names: string[], ...keys: string[]): void {
  for (const n of names) m.set(n, keys);
}

function alias(native: string, kind: ValueKind, rename = false): AliasSpec {
  return { native, rename, kind };
}

function buildTriggerCatalog(): Map<string, ProviderSpec> {
  const cat = new Map<string, ProviderSpec>();

  const gh = new Map<string, string[]>();
  events(gh, ['issue_created'], 'repos', 'labels', 'assignees', 'authors', 'keywords', 'skip_bots');
  events(gh, ['issue_labeled', 'issue_assigned', 'pull_request_assigned'], 'repos', 'labels', 'assignees', 'authors', 'skip_bots');
  events(gh, ['issue_commented', 'pull_request_commented'], 'repos', 'labels', 'authors', 'keywords', 'skip_bots');
  events(gh, ['issue_mentioned', 'pull_request_mentioned'], 'repos', 'mentioned', 'labels', 'authors', 'keywords', 'skip_bots');
  events(
    gh,
    ['pull_request_opened', 'pull_request_ready', 'pull_request_reopened'],
    'repos',
    'base_branches',
    'pr_numbers',
    'paths',
    'assignees',
    'authors',
    'labels',
    'keywords',
    'skip_bots',
  );
  events(
    gh,
    ['pull_request_closed', 'pull_request_dequeued', 'pull_request_enqueued', 'pull_request_labeled', 'pull_request_merged', 'pull_request_synchronized'],
    'repos',
    'base_branches',
    'pr_numbers',
    'paths',
    'assignees',
    'authors',
    'labels',
    'skip_bots',
  );
  events(gh, ['pull_request_review_requested'], 'repos', 'reviewers', 'reviewer_teams', 'authors', 'skip_bots');
  events(gh, ['pull_request_review_submitted'], 'repos', 'mentioned', 'labels', 'review_states', 'authors', 'keywords', 'skip_bots');
  events(gh, ['push'], 'repos', 'branches', 'paths', 'skip_bots');
  events(gh, ['check_suite_completed'], 'repos', 'conclusions', 'branches', 'labels', 'authors', 'skip_bots');
  events(gh, ['workflow_run_completed'], 'repos', 'conclusions', 'branches', 'workflows', 'labels', 'authors', 'skip_bots');
  events(gh, ['commit_status'], 'repos', 'states', 'contexts', 'skip_bots');
  events(gh, ['check_run_rerequested', 'check_suite_rerequested'], 'repos', 'skip_bots');
  cat.set(
    PROVIDER_GITHUB,
    spec({
      events: gh,
      keys: new Map<string, KeySpec>([
        ['skip_bots', { kind: ValueKind.SkipBots }],
        ['pr_numbers', { kind: ValueKind.Int }],
        ['review_states', { kind: ValueKind.Enum, values: ['approved', 'changes_requested', 'commented'] }],
      ]),
      aliases: new Map([
        ['baseBranches', alias('base_branches', ValueKind.String, true)],
        ['prNumbers', alias('pr_numbers', ValueKind.String, true)],
      ]),
    }),
  );

  const gl = new Map<string, string[]>();
  events(gl, ['bot_mentioned'], 'repos');
  events(gl, ['merge_request'], 'repos', 'actions', 'base_branches');
  events(gl, ['push'], 'repos', 'branches');
  cat.set(PROVIDER_GITLAB, spec({ events: gl, managed: new Set(['mentioned']) }));

  const ado = new Map<string, string[]>();
  events(
    ado,
    ['pull_request_created', 'pull_request_updated', 'pull_request_commented', 'pull_request_closed', 'pull_request_merged'],
    'repos',
    'base_branches',
  );
  events(ado, ['pull_request_mentioned'], 'repos', 'mentioned');
  events(ado, ['work_item_created', 'work_item_assigned', 'work_item_labeled'], 'work_item_types', 'labels', 'assignees', 'authors');
  events(ado, ['work_item_mentioned'], 'mentioned');
  cat.set(
    PROVIDER_AZURE_DEVOPS,
    spec({
      events: ado,
      keys: new Map([
        ['repos', { kind: ValueKind.UUID }],
        ['mentioned', { kind: ValueKind.UUID }],
      ]),
      managed: new Set(['source_repos']),
      unavailable: new Map([['push', 'azure_devops push events are not available for factory automations']]),
    }),
  );

  const lin = new Map<string, string[]>();
  const issueKeys = ['team_ids', 'project_ids', 'labels', 'state_ids', 'assignee_ids', 'mentioned_user_ids', 'creator_ids'];
  events(lin, ['issue_labeled', 'issue_created', 'issue_assigned', 'issue_state_changed'], ...issueKeys);
  events(lin, ['comment_created'], 'team_ids', 'project_ids', 'labels', 'state_ids', 'issue_ids', 'mentioned_user_ids', 'creator_ids');
  events(lin, ['agent_session_created'], 'team_ids', 'project_ids', 'labels', 'creator_ids', 'keywords');
  cat.set(
    PROVIDER_LINEAR,
    spec({
      events: lin,
      keys: new Map(['team_ids', 'project_ids', 'state_ids', 'issue_ids', 'assignee_ids', 'creator_ids'].map((k) => [k, { kind: ValueKind.UUID }])),
      aliases: new Map([
        ['teams', alias('team_ids', ValueKind.NameAlias)],
        ['projects', alias('project_ids', ValueKind.NameAlias)],
        ['states', alias('state_ids', ValueKind.NameAlias)],
        ['issues', alias('issue_ids', ValueKind.NameAliasInt)],
      ]),
    }),
  );

  const sl = new Map<string, string[]>();
  events(sl, ['app_mention', 'message_dm', 'message_im', 'message_mpim', 'message_posted'], 'channel_ids', 'user_ids', 'keywords');
  events(sl, ['reaction_added'], 'channel_ids', 'user_ids', 'keywords', 'emojis', 'item_user_ids');
  events(sl, ['member_joined_channel'], 'channel_ids', 'user_ids');
  cat.set(
    PROVIDER_SLACK,
    spec({
      events: sl,
      aliases: new Map([
        ['channels', alias('channel_ids', ValueKind.NameAlias)],
        ['users', alias('user_ids', ValueKind.NameAlias)],
        ['itemUsers', alias('item_user_ids', ValueKind.NameAlias)],
      ]),
    }),
  );

  const jr = new Map<string, string[]>();
  events(jr, ['issue_created', 'issue_labeled'], 'project_keys', 'labels');
  events(jr, ['status_changed'], 'project_keys', 'status_ids');
  events(jr, ['agent_session_created'], 'project_keys', 'labels', 'keywords');
  cat.set(
    PROVIDER_JIRA,
    spec({
      events: jr,
      aliases: new Map([
        ['projects', alias('project_keys', ValueKind.NameAlias)],
        ['statuses', alias('status_ids', ValueKind.NameAlias)],
      ]),
    }),
  );

  const tm = new Map<string, string[]>();
  events(tm, ['app_mention', 'message_posted'], 'team_ids', 'channel_ids', 'user_ids', 'keywords');
  cat.set(
    PROVIDER_TEAMS,
    spec({
      events: tm,
      keys: new Map([
        ['team_ids', { kind: ValueKind.InOnlyOne }],
        ['channel_ids', { kind: ValueKind.InOnlyMany }],
        ['user_ids', { kind: ValueKind.UUID }],
      ]),
      aliases: new Map([
        ['channels', alias('channel_ids', ValueKind.NameAlias)],
        ['users', alias('user_ids', ValueKind.NameAlias)],
      ]),
      filter: 'required',
      requireKeys: ['team_ids', 'channel_ids'],
    }),
  );

  cat.set(PROVIDER_SCHEDULE, spec({ events: new Map([['cron_fired', []]]), managed: new Set(['schedule_ids']), filter: 'forbidden' }));
  cat.set(
    PROVIDER_WEBHOOK,
    spec({
      events: new Map([['received', ['webhook_ids', 'payload']]]),
      keys: new Map([
        ['webhook_ids', { kind: ValueKind.WebhookIDs }],
        ['payload', { kind: ValueKind.Payload }],
      ]),
      filter: 'required',
      requireKeys: ['webhook_ids'],
    }),
  );
  return cat;
}

export const triggerCatalog = buildTriggerCatalog();

/** normalizeProvider maps an authored provider name to its canonical form. */
export function normalizeProvider(p: string): string {
  return p === 'teams' || p === 'microsoft_teams' ? PROVIDER_TEAMS : p;
}

function integrationFor(provider: string): string {
  switch (provider) {
    case PROVIDER_SLACK:
      return 'slack';
    case PROVIDER_TEAMS:
      return 'microsoft-teams';
    case PROVIDER_LINEAR:
      return 'linear';
    case PROVIDER_JIRA:
      return 'jira';
  }
  return '';
}

/** EventSpec describes one authorable provider event and its filter keys. */
export interface EventSpec {
  event: string;
  filter_fields: string[];
  /** authoring alias → native key */
  aliases?: Record<string, string>;
  filter: FilterRule;
}

/** triggerEvents returns the authorable events of a provider ("teams" accepted), or null. */
export function triggerEvents(provider: string): EventSpec[] | null {
  const ps = triggerCatalog.get(normalizeProvider(provider));
  if (!ps) return null;
  const out: EventSpec[] = [];
  for (const [ev, keys] of ps.events) {
    const es: EventSpec = { event: ev, filter_fields: [...keys], filter: ps.filter };
    for (const [a, s] of ps.aliases) {
      if (keys.includes(s.native)) (es.aliases ??= {})[a] = s.native;
    }
    out.push(es);
  }
  out.sort((x, y) => (x.event < y.event ? -1 : x.event > y.event ? 1 : 0));
  return out;
}

/** triggerProviders returns every canonical trigger provider name. */
export function triggerProviders(): string[] {
  return [...triggerCatalog.keys()].sort();
}

export const uuidPattern = '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$';
const uuidRe = new RegExp(uuidPattern);

/** TriggerEnv is the definition context a trigger is validated against. */
export interface TriggerEnv {
  integrationsSet: boolean;
  integrations: Set<string>;
  /** Names of the webhooks/<name>.yaml files. */
  webhooks: Set<string>;
}

function knownWebhooks(env: TriggerEnv): string {
  if (env.webhooks.size === 0) return 'none; add webhooks/<name>.yaml';
  return [...env.webhooks].sort().join(', ');
}

export function parseTrigger(c: Doc, n: YNode | null, path: string, env: TriggerEnv): [Trigger, boolean] {
  const t: Trigger = { provider: '', event: '' };
  const m = c.mapping(n, path);
  if (m === null) return [t, false];
  try {
    let ok = true;
    const pv = m.require('provider');
    const ev = m.require('event');
    const fv = m.get('filter');
    const sv = m.get('schedule');
    const cv = m.get('concurrency');
    if (pv === null || ev === null) return [t, false];
    const [provider, good] = c.nonEmptyStr(pv, path + '.provider');
    const [event, good2] = c.nonEmptyStr(ev, path + '.event');
    if (!good || !good2) return [t, false];
    t.provider = normalizeProvider(provider);
    t.event = event;
    const ps = triggerCatalog.get(t.provider);
    if (!ps) {
      c.errf(
        pv,
        path + '.provider',
        `unknown trigger provider ${goQuote(provider)} (expected one of github, gitlab, azure_devops, slack, teams, linear, jira, schedule, webhook)`,
      );
      return [t, false];
    }
    const why = ps.unavailable.get(event);
    if (why !== undefined) {
      c.errf(ev, path + '.event', why);
      return [t, false];
    }
    const keys = ps.events.get(event);
    if (keys === undefined) {
      const names = [...ps.events.keys()].sort();
      c.errf(ev, path + '.event', `unknown ${provider} event ${goQuote(event)} (expected one of ${names.join(', ')})`);
      return [t, false];
    }
    const integ = integrationFor(t.provider);
    if (integ !== '' && env.integrationsSet && !env.integrations.has(integ)) {
      c.errf(pv, path + '.provider', `${provider} triggers require the ${integ} integration; add it to integrations in factory.yaml`);
      ok = false;
    }

    if (t.provider === PROVIDER_SCHEDULE) {
      if (sv === null || isNull(sv)) {
        c.errf(m.node, path + '.schedule', `${path}.schedule is required for schedule/cron_fired triggers`);
        ok = false;
      } else {
        const s = parseSchedule(c, sv, path + '.schedule');
        if (s !== null) t.schedule = s;
        else ok = false;
      }
    } else if (sv !== null && !isNull(sv)) {
      c.errf(sv, path + '.schedule', `${path}.schedule is only valid for schedule/cron_fired triggers`);
      ok = false;
    }
    if (cv !== null && !isNull(cv)) {
      const [v, g] = c.nonEmptyStr(cv, path + '.concurrency');
      if (!g) {
        ok = false;
      } else if (t.provider !== PROVIDER_SCHEDULE) {
        c.errf(cv, path + '.concurrency', `${path}.concurrency is only valid for schedule/cron_fired triggers`);
        ok = false;
      } else if (v !== SCHEDULE_CONCURRENCY_ALLOW && v !== SCHEDULE_CONCURRENCY_SKIP) {
        c.errf(cv, path + '.concurrency', `${path}.concurrency must be allow or skip, got ${goQuote(v)}`);
        ok = false;
      } else {
        t.concurrency = v;
      }
    }

    const hasFilter = fv !== null && !isNull(fv);
    if (ps.filter === 'forbidden' && fv !== null) {
      c.errf(fv, path + '.filter', `${provider} triggers accept no filter`);
      return [t, false];
    }
    if (ps.filter === 'required' && !hasFilter) {
      c.errf(m.node, path + '.filter', `${path}.filter is required for ${provider}/${event} triggers`);
      return [t, false];
    }
    if (!hasFilter) return [t, ok];
    const [filter, fgood] = parseFilter(c, fv, path + '.filter', t.provider, ps, keys, env);
    if (!fgood) return [t, false];
    if (filter !== null && Object.keys(filter).length > 0) t.filter = filter;
    return [t, ok];
  } finally {
    m.finish();
  }
}

function parseSchedule(c: Doc, n: YNode, path: string): Schedule | null {
  const m = c.mapping(n, path);
  if (m === null) return null;
  try {
    const s: Schedule = { cron: '' };
    const nv = m.get('name');
    if (nv !== null && !isNull(nv)) {
      const [name, ok] = c.nonEmptyStr(nv, path + '.name');
      if (!ok) return null;
      s.name = name;
    }
    const v = m.require('cron');
    if (v === null) return null;
    const [expr, ok] = c.nonEmptyStr(v, path + '.cron');
    if (!ok) return null;
    const err = validateCron(expr);
    if (err !== null) {
      c.errf(v, path + '.cron', `${path + '.cron'} ${goQuote(expr)}: ${err}`);
      return null;
    }
    s.cron = goTrimSpace(expr);
    return s;
  } finally {
    m.finish();
  }
}

function keySpecFor(ps: ProviderSpec, key: string): KeySpec {
  return ps.keys.get(key) ?? { kind: ValueKind.String };
}

function acceptedKeys(ps: ProviderSpec, keys: string[]): string[] {
  const out = [...keys];
  for (const [a, s] of ps.aliases) if (keys.includes(s.native)) out.push(a);
  return out.sort();
}

function parseFilter(
  c: Doc,
  n: YNode,
  path: string,
  provider: string,
  ps: ProviderSpec,
  keys: string[],
  env: TriggerEnv,
): [Record<string, unknown> | null, boolean] {
  const m = c.mapping(n, path);
  if (m === null) return [null, false];
  let ok = true;
  const out: Record<string, unknown> = {};
  for (const key of m.order) {
    const v = m.get(key);
    const f = m.field(key);
    const kn = m.keyNode(key);
    if (ps.managed.has(key)) {
      c.errf(kn, f, `${f} is server-managed; don't set it`);
      ok = false;
      continue;
    }
    const a = ps.aliases.get(key);
    if (a !== undefined && keys.includes(a.native)) {
      if (m.has(a.native)) {
        c.errf(kn, f, `don't combine ${key} with its native key ${a.native}`);
        ok = false;
        continue;
      }
      if (a.rename) {
        const [val, good] = validateMatcher(c, v, f, keySpecFor(ps, a.native), env);
        ok = ok && good;
        if (val !== null) put(out, a.native, val);
        continue;
      }
      const [val, good] = validateMatcher(c, v, f, { kind: a.kind }, env);
      ok = ok && good;
      if (val !== null) put(out, key, val);
      continue;
    }
    if (!keys.includes(key)) {
      c.errf(kn, f, `unknown filter key ${goQuote(key)} for ${provider} triggers (accepted: ${acceptedKeys(ps, keys).join(', ')})`);
      ok = false;
      continue;
    }
    const [val, good] = validateMatcher(c, v, f, keySpecFor(ps, key), env);
    ok = ok && good;
    if (val !== null) put(out, key, val);
  }
  for (const rk of ps.requireKeys) {
    let present = Object.hasOwn(out, rk);
    for (const [a, s] of ps.aliases) {
      if (Object.hasOwn(out, a) && s.native === rk) present = true;
    }
    if (!present && ok) {
      c.errf(m.node, path + '.' + rk, `${path}.${rk} is required`);
      ok = false;
    }
  }
  return [out, ok];
}

function validateMatcher(c: Doc, n0: YNode | null, path: string, ks: KeySpec, env: TriggerEnv): [unknown, boolean] {
  const n = resolve(n0);
  if (isNull(n) || n === null) return [null, true];
  if (ks.kind === ValueKind.SkipBots) {
    const [s, ok] = c.enum(n, path, 'all', 'self', 'none');
    return ok ? [s, true] : [null, false];
  }
  if (ks.kind === ValueKind.Payload) {
    const leaves = { n: 0 };
    if (!validatePayloadPattern(c, n, path, 1, leaves)) return [null, false];
    return [toValue(n), true];
  }
  const plainOnly = ks.kind === ValueKind.NameAlias || ks.kind === ValueKind.NameAliasInt;
  const inOnly = ks.kind === ValueKind.InOnlyOne || ks.kind === ValueKind.InOnlyMany || ks.kind === ValueKind.WebhookIDs;
  let inNode: YNode | null = null;
  let notInNode: YNode | null = null;
  if (n.kind === 'seq') {
    inNode = n;
  } else if (n.kind === 'map') {
    if (plainOnly) {
      c.errf(n, path, `${path} accepts only a plain list of names`);
      return [null, false];
    }
    const m = c.mapping(n, path);
    if (m === null) return [null, false];
    for (const k of m.order) {
      if (k === 'in') {
        inNode = m.get(k);
      } else if (k === 'not_in') {
        notInNode = m.get(k);
        if (inOnly) {
          c.errf(m.keyNode(k), path + '.not_in', `${path} supports only in`);
          return [null, false];
        }
      } else {
        m.get(k);
        c.errf(m.keyNode(k), path + '.' + k, `unknown matcher operator ${goQuote(k)} (expected in or not_in)`);
        return [null, false];
      }
    }
  } else {
    c.errf(n, path, `${path} must be a list of values or a matcher object with in/not_in`);
    return [null, false];
  }
  let ok = true;
  const collect = (node: YNode | null, p: string): (string | number)[] => {
    if (isNull(node)) return [];
    const items = c.seq(node, p) ?? [];
    const vals: (string | number)[] = [];
    items.forEach((it, i) => {
      const [v, good] = validateValue(c, it, indexPath(p, i), ks, env);
      if (good && v !== null) vals.push(v);
      else ok = false;
    });
    return vals;
  };
  const inPath = n.kind === 'map' ? path + '.in' : path;
  const inVals = collect(inNode, inPath);
  const notIn = collect(notInNode, path + '.not_in');
  if (!ok) return [null, false];
  for (const a of inVals) {
    for (const b of notIn) {
      if (String(a) === String(b)) {
        c.errf(n, path, `${String(a)} appears in both in and not_in of ${path}`);
        return [null, false];
      }
    }
  }
  if (ks.kind === ValueKind.InOnlyOne && inVals.length !== 1) {
    c.errf(n, path, `${path} must include exactly one value`);
    return [null, false];
  }
  if (ks.kind === ValueKind.InOnlyMany && inVals.length < 1) {
    c.errf(n, path, `${path} must include at least one value`);
    return [null, false];
  }
  if (ks.kind === ValueKind.WebhookIDs && inVals.length !== 1) {
    c.errf(n, path, `${path} must be a one-item list with one webhook; add one trigger per webhook`);
    return [null, false];
  }
  if (inVals.length === 0 && notIn.length === 0) return [null, true];
  if (n.kind === 'seq') return [inVals, true];
  const out: Record<string, unknown> = {};
  if (inVals.length > 0) out['in'] = inVals;
  if (notIn.length > 0) out['not_in'] = notIn;
  return [out, true];
}

function validateValue(c: Doc, n: YNode, path: string, ks: KeySpec, env: TriggerEnv): [string | number | null, boolean] {
  if (ks.kind === ValueKind.Int || ks.kind === ValueKind.NameAliasInt) {
    const [i, ok] = c.integer(n, path);
    if (ok && i <= 0) {
      c.errf(n, path, ks.kind === ValueKind.Int ? `${path} must be a positive integer` : `${path} must be a positive issue number such as 42`);
      return [null, false];
    }
    return [i, ok];
  }
  let [s, ok] = c.nonEmptyStr(n, path);
  if (!ok) return [null, false];
  switch (ks.kind) {
    case ValueKind.UUID:
      if (!uuidRe.test(s)) {
        c.errf(n, path, `${path} ${goQuote(s)} must be a UUID`);
        return [null, false];
      }
      break;
    case ValueKind.Enum:
      if (!(ks.values ?? []).includes(s)) {
        c.errf(n, path, `${path} must be one of ${(ks.values ?? []).join(', ')} (got ${goQuote(s)})`);
        return [null, false];
      }
      break;
    case ValueKind.NameAlias:
      s = goTrimSpace(s);
      break;
    case ValueKind.WebhookIDs:
      if (s.endsWith('.yaml') || s.endsWith('.yml') || s.includes('/')) {
        c.errf(n, path, `${path} takes a webhook UID or webhook name, not a file name (got ${goQuote(s)})`);
        return [null, false];
      }
      if (!env.webhooks.has(s) && !s.startsWith(WEBHOOK_UID_PREFIX)) {
        c.errf(n, path, `webhook ${goQuote(s)} does not exist; known: ${knownWebhooks(env)}`);
        return [null, false];
      }
      break;
  }
  return [s, true];
}

/** Payload pattern limits. */
export const MAX_PATTERN_DEPTH = 10;
export const MAX_PATTERN_LEAVES = 64;
export const MAX_PATTERN_VALUES = 32;

function validatePayloadPattern(c: Doc, n0: YNode, path: string, depth: number, leaves: { n: number }): boolean {
  const n = resolve(n0)!;
  if (depth > MAX_PATTERN_DEPTH) {
    c.errf(n, path, `payload pattern nests deeper than ${MAX_PATTERN_DEPTH} levels`);
    return false;
  }
  if (n.kind !== 'map') {
    c.errf(n, path, `${path} must be an object`);
    return false;
  }
  const m = c.mapping(n, path);
  if (m === null) return false;
  let ok = true;
  for (const k of m.order) {
    const v = resolve(m.get(k))!;
    const f = path + '.' + k;
    if (v.kind === 'seq') {
      leaves.n++;
      if (!checkPatternValues(c, v, f)) ok = false;
    } else if (v.kind === 'map' && isOperatorObject(v)) {
      leaves.n++;
      if (!checkOperatorObject(c, v, f)) ok = false;
    } else if (v.kind === 'map') {
      if (v.content.length === 0) {
        c.errf(v, f, `${f} must not be an empty object`);
        ok = false;
        continue;
      }
      if (!validatePayloadPattern(c, v, f, depth + 1, leaves)) ok = false;
    } else {
      c.errf(v, f, `${f} must be a list of allowed values, an operator object (in, not_in, exists) or a nested pattern`);
      ok = false;
    }
    if (leaves.n > MAX_PATTERN_LEAVES) {
      c.errf(v, f, `payload pattern holds more than ${MAX_PATTERN_LEAVES} leaf fields`);
      return false;
    }
  }
  return ok;
}

function isOperatorObject(n: YNode): boolean {
  if (n.content.length === 0) return false;
  for (let i = 0; i < n.content.length; i += 2) {
    const k = resolve(n.content[i])?.value;
    if (k !== 'in' && k !== 'not_in' && k !== 'exists') return false;
  }
  return true;
}

function checkPatternValues(c: Doc, n: YNode, path: string): boolean {
  if (n.content.length > MAX_PATTERN_VALUES) {
    c.errf(n, path, `${path} lists more than ${MAX_PATTERN_VALUES} values`);
    return false;
  }
  for (let i = 0; i < n.content.length; i++) {
    const it = resolve(n.content[i])!;
    if (it.kind !== 'scalar') {
      c.errf(it, indexPath(path, i), 'payload pattern values must be strings, numbers, booleans or null');
      return false;
    }
  }
  return true;
}

function checkOperatorObject(c: Doc, n: YNode, path: string): boolean {
  const m = c.mapping(n, path);
  if (m === null) return false;
  let inVals: string[] = [];
  let notIn: string[] = [];
  let existsFalse = false;
  let ok = true;
  for (const k of m.order) {
    const v = m.get(k);
    if (k === 'in' || k === 'not_in') {
      if (isNull(v)) continue;
      const rv = resolve(v)!;
      if (rv.kind !== 'seq') {
        c.errf(rv, `${path}.${k}`, `${path}.${k} must be a list`);
        ok = false;
        continue;
      }
      if (!checkPatternValues(c, rv, `${path}.${k}`)) {
        ok = false;
        continue;
      }
      const vals = rv.content.map((x) => sprintNode(x));
      if (k === 'in') inVals = vals;
      else notIn = vals;
    } else if (k === 'exists') {
      const [b, good] = c.boolean(v, path + '.exists');
      if (!good) ok = false;
      existsFalse = good && !b;
    }
  }
  for (const a of inVals) {
    for (const b of notIn) {
      if (a === b) {
        c.errf(n, path, `${a} appears in both in and not_in of ${path}`);
        ok = false;
      }
    }
  }
  if (existsFalse && inVals.length > 0) {
    c.errf(n, path, `${path} combines exists: false with a non-empty in`);
    ok = false;
  }
  return ok;
}
