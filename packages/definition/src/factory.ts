// factory.yaml parser. Port of internal/definition/factory.go.

import {
  copyMCP,
  emptyExec,
  emailRe,
  type ExecSettings,
  type HarnessOverride,
  aliasRe,
  checkModelHarness,
  parseExec,
  parseHarness,
  parseMCPServers,
  parseNetworkPolicy,
  SCHEMA_VERSION,
  secretList,
  toHarness,
} from './common.ts';
import { goFloat, goQuote, goTrimSpace } from './gofmt.ts';
import type {
  CompiledDefinition,
  GCPProvider,
  HarnessConfig,
  IntegrationDecl,
  MCPServer,
  NetworkPolicy,
  RepoRef,
  SelfImprovementConfig,
  SpendLimit,
} from './types.ts';
import { type Diag, Doc, indexPath, isNull, type Mapping, resolve, type YNode } from './yamlnode.ts';

export const codeForgeValues = ['GITHUB', 'GITLAB', 'AZURE_DEVOPS'];
export const integrationTypes = ['slack', 'microsoft-teams', 'linear', 'jira'];
export const FACTORY_FILE = 'factory.yaml';

export interface ScorerDefaultsSpec {
  machine: string;
  machineNode: YNode | null;
  secrets: string[] | null;
  mcp: Record<string, MCPServer> | null;
  model: string | null;
  harness: HarnessOverride | null;
}

interface RepoSubstitutionSpec {
  node: YNode;
  source: RepoRef;
  target: RepoRef;
}

/** FactorySpec is the parsed factory.yaml. */
export interface FactorySpec {
  c: Doc;
  def: CompiledDefinition;
  networkPolicy: NetworkPolicy | null;
  mcp: Record<string, MCPServer> | null;
  defaults: ExecSettings;
  defaultHarness: HarnessConfig;
  harnessOK: boolean;
  scorerDefaults: ScorerDefaultsSpec | null;
  repoSubs: RepoSubstitutionSpec[];
}

export function newDefinition(): CompiledDefinition {
  return {
    schemaVersion: '',
    name: '',
    repositories: null,
    integrationsSet: false,
    githubOnboardingNotices: true,
    factoryConfigChecks: 'main',
    agents: null,
    automations: null,
    machines: null,
    scorers: null,
    webhooks: null,
    benchmarks: null,
    skills: null,
  };
}

const emptyRepo = (): RepoRef => ({ codeForge: '', owner: '', name: '' });

export function parseFactory(d: Diag, content: string): FactorySpec | null {
  const c = new Doc(d, FACTORY_FILE);
  const root = c.parse(content);
  if (root === null) return null;
  const m = c.mapping(root, '');
  if (m === null) return null;
  const f: FactorySpec = {
    c,
    def: newDefinition(),
    networkPolicy: null,
    mcp: null,
    defaults: emptyExec(c),
    defaultHarness: { type: '', model: '' },
    harnessOK: false,
    scorerDefaults: null,
    repoSubs: [],
  };
  try {
    const def = f.def;
    let v = m.require('schemaVersion');
    if (v !== null) {
      const [s, ok] = c.str(v, 'schemaVersion');
      if (ok && s !== SCHEMA_VERSION) c.errf(v, 'schemaVersion', `schemaVersion must be ${SCHEMA_VERSION} (got ${goQuote(s)})`);
    }
    def.schemaVersion = SCHEMA_VERSION;
    v = m.require('name');
    if (v !== null) def.name = c.nonEmptyStr(v, 'name')[0];
    v = m.get('description');
    if (v !== null && !isNull(v)) def.description = c.str(v, 'description')[0];
    v = m.get('alias');
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.str(v, 'alias');
      if (ok) {
        if (!aliasRe.test(s) || goTrimSpace(s) === '') {
          c.errf(v, 'alias', `alias must be 1-60 letters, numbers, spaces, '.', '_' or '-' (got ${goQuote(s)})`);
        } else {
          def.alias = s;
        }
      }
    }
    v = m.get('credentialStrategy');
    if (v !== null && !isNull(v)) def.credentialStrategy = c.enum(v, 'credentialStrategy', 'EXECUTOR', 'CREATOR')[0];
    v = m.get('maxConcurrentRuns');
    if (v !== null && !isNull(v)) {
      const [i, ok] = c.integer(v, 'maxConcurrentRuns');
      if (ok) {
        if (i < 1) c.errf(v, 'maxConcurrentRuns', `maxConcurrentRuns must be 1 or more (got ${i})`);
        else def.maxConcurrentRuns = i;
      }
    }
    v = m.get('approvers');
    if (v !== null && !isNull(v)) {
      const [list, ok] = c.uniqueStrList(v, 'approvers');
      if (ok && list !== null) {
        if (list.length === 0) c.errf(v, 'approvers', 'approvers must list at least one user email or service account name');
        else def.approvers = list;
      }
    }
    v = m.get('restrictFollowupsToStarter');
    if (v !== null && !isNull(v)) def.restrictFollowupsToStarter = c.boolean(v, 'restrictFollowupsToStarter')[0];
    v = m.get('requireLinkedActor');
    if (v !== null && !isNull(v)) def.requireLinkedActor = c.boolean(v, 'requireLinkedActor')[0];
    v = m.get('allowAgentMerge');
    if (v !== null && !isNull(v)) def.allowAgentMerge = c.boolean(v, 'allowAgentMerge')[0];
    v = m.get('spendLimit');
    if (v !== null && !isNull(v)) {
      const sm = c.mapping(v, 'spendLimit');
      if (sm !== null) {
        const lim: SpendLimit = {};
        for (const k of ['daily', 'monthly'] as const) {
          const n = sm.get(k);
          if (n === null || isNull(n)) continue;
          const [x, ok] = c.number(n, 'spendLimit.' + k);
          if (!ok) continue;
          if (x <= 0) {
            c.errf(n, 'spendLimit.' + k, `spendLimit.${k} must be more than 0 US dollars (got ${goFloat(x)})`);
            continue;
          }
          lim[k] = x;
        }
        sm.finish();
        if ((lim.daily ?? 0) > 0 || (lim.monthly ?? 0) > 0) def.spendLimit = lim;
      }
    }
    v = m.get('automations');
    if (v !== null && !isNull(v)) {
      const am = c.mapping(v, 'automations');
      if (am !== null) {
        const g = am.get('githubOnboardingNotices');
        if (g !== null && !isNull(g)) {
          const [b, ok] = c.boolean(g, 'automations.githubOnboardingNotices');
          if (ok) def.githubOnboardingNotices = b;
        }
        am.finish();
      }
    }
    v = m.get('github');
    if (v !== null && !isNull(v)) {
      const gm = c.mapping(v, 'github');
      if (gm !== null) {
        const g = gm.get('factoryConfigChecks');
        if (g !== null && !isNull(g)) {
          const [s, ok] = c.enum(g, 'github.factoryConfigChecks', 'main', 'all');
          if (ok) def.factoryConfigChecks = s;
        }
        gm.finish();
      }
    }

    parseRepositories(f, m);

    v = m.get('secrets');
    if (v !== null && !isNull(v)) {
      const [list] = secretList(c, v, 'secrets');
      if (list !== null) def.secrets = list;
    }
    if (m.has('networkPolicy')) f.networkPolicy = parseNetworkPolicy(c, m.get('networkPolicy'), 'networkPolicy')[0];
    v = m.get('mcpServers');
    if (v !== null && !isNull(v)) {
      f.mcp = parseMCPServers(c, v, 'mcpServers')[0];
      const cp = copyMCP(f.mcp);
      if (cp !== null) def.mcpServers = cp;
    }
    parseCloudProviders(f, m);
    parseIntegrations(f, m);
    v = m.get('scorerDefaults');
    if (v !== null && !isNull(v)) parseScorerDefaults(f, v);
    v = m.get('selfImprovement');
    if (v !== null && !isNull(v)) {
      const si = parseSelfImprovement(c, v);
      if (si !== null) def.selfImprovement = si;
    }

    v = m.require('agentDefaults');
    if (v !== null) {
      const am = c.mapping(v, 'agentDefaults');
      if (am !== null) {
        f.defaults = parseExec(c, am, true, false);
        am.finish();
        if (f.defaults.model === null && f.defaults.harness === null && !am.present('model') && !am.present('harness')) {
          c.errf(am.node, 'agentDefaults', 'agentDefaults must set exactly one of model or harness');
        } else {
          const h = toHarness(f.defaults.harness, f.defaults.model);
          if (h !== null) {
            f.defaultHarness = h;
            f.harnessOK = true;
          }
        }
        if (f.defaults.workerSet && f.defaults.worker === '') f.defaults.workerSet = false;
        if (f.harnessOK && f.defaultHarness.auth && f.defaultHarness.auth.source === 'workerEnvironment' && f.defaults.worker === '') {
          c.errf(
            f.defaults.harness?.authNode ?? null,
            'agentDefaults.harness.auth.source',
            'auth.source workerEnvironment requires agentDefaults.workerHost to select a self-hosted worker',
          );
        }
        if (f.defaults.secretsSet && f.defaults.secrets === null) f.defaults.secrets = [];
      }
    }
    if (f.mcp !== null && f.defaults.mcpSet) {
      for (const name of Object.keys(f.defaults.mcp ?? {})) {
        if (Object.hasOwn(f.mcp, name)) {
          c.errf(f.defaults.mcpNode, 'agentDefaults.mcpServers.' + name, `MCP server ${goQuote(name)} is already granted factory-wide by mcpServers`);
        }
      }
    }
    return f;
  } finally {
    m.finish();
  }
}

function parseRepositories(f: FactorySpec, m: Mapping): void {
  const { c, def } = f;
  let forges: string[] = [];
  let forgesSet = false;
  let v = m.get('codeForges');
  if (v !== null && !isNull(v)) {
    const [list, ok0] = c.uniqueStrList(v, 'codeForges');
    let ok = ok0;
    if (ok && list !== null) {
      const items = resolve(v)!.content;
      list.forEach((s, i) => {
        if (!codeForgeValues.includes(s)) {
          c.errf(items[i], indexPath('codeForges', i), `codeForges values must be one of ${codeForgeValues.join(', ')} (got ${goQuote(s)})`);
          ok = false;
        }
      });
    }
    if (ok && list !== null) {
      forges = list;
      forgesSet = true;
      def.codeForges = list;
    }
  }
  v = m.require('repositories');
  if (v === null) {
    def.repositories = [];
    return;
  }
  const items = c.seq(v, 'repositories') ?? [];
  def.repositories = [];
  if (forgesSet && forges.length === 0 && items.length > 0) c.errf(v, 'repositories', 'repositories must be empty when codeForges is an empty list');
  const seen = new Set<string>();
  items.forEach((it, i) => {
    const p = indexPath('repositories', i);
    const rm = c.mapping(it, p);
    if (rm === null) return;
    const r = emptyRepo();
    const cv = rm.get('codeForge');
    if (cv !== null && !isNull(cv)) {
      r.codeForge = c.enum(cv, p + '.codeForge', ...codeForgeValues)[0];
      if (r.codeForge !== '' && forgesSet && !forges.includes(r.codeForge)) {
        c.errf(cv, p + '.codeForge', `${p + '.codeForge'} ${r.codeForge} is not listed in codeForges`);
      }
    } else if (forges.length > 1) {
      c.errf(rm.node, p + '.codeForge', `${p}.codeForge is required when codeForges lists more than one code host`);
    }
    if (r.codeForge === '') r.codeForge = forges.length === 1 ? forges[0]! : 'GITHUB';
    const ov = rm.require('owner');
    if (ov !== null) {
      const [s, ok] = c.nonEmptyStr(ov, p + '.owner');
      if (ok) {
        if (!validOwner(r.codeForge, s)) c.errf(ov, p + '.owner', `${p + '.owner'} ${goQuote(s)} is not a valid owner for ${r.codeForge}`);
        r.owner = s;
      }
    }
    const nv = rm.require('name');
    if (nv !== null) {
      const [s, ok] = c.nonEmptyStr(nv, p + '.name');
      if (ok) {
        if (/[/ \t]/.test(s)) c.errf(nv, p + '.name', `${p + '.name'} ${goQuote(s)} must be a single repository name`);
        r.name = s;
      }
    }
    rm.finish();
    const key = (r.codeForge + ':' + r.owner + '/' + r.name).toLowerCase();
    if (r.owner !== '' && r.name !== '') {
      if (seen.has(key)) c.errf(rm.node, p, `repository ${r.owner}/${r.name} is listed more than once`);
      seen.add(key);
    }
    def.repositories!.push(r);
  });

  const sv = m.get('benchmarkRepoSubstitutions');
  if (sv !== null && !isNull(sv)) {
    (c.seq(sv, 'benchmarkRepoSubstitutions') ?? []).forEach((it, i) => {
      const p = indexPath('benchmarkRepoSubstitutions', i);
      const sm = c.mapping(it, p);
      if (sm === null) return;
      const sub: RepoSubstitutionSpec = { node: sm.node, source: emptyRepo(), target: emptyRepo() };
      let n = sm.require('source');
      if (n !== null) sub.source = parseRepoTriple(f, n, p + '.source');
      n = sm.require('target');
      if (n !== null) sub.target = parseRepoTriple(f, n, p + '.target');
      sm.finish();
      if (sub.source.owner !== '' && sub.target.owner !== '') {
        if (!ownsRepo(f, sub.source)) {
          c.errf(sm.node, p + '.source', `${p}.source ${sub.source.owner}/${sub.source.name} must be one of the factory's repositories`);
        }
        if (ownsRepo(f, sub.target)) {
          c.errf(sm.node, p + '.target', `${p}.target ${sub.target.owner}/${sub.target.name} must not be one of the factory's repositories`);
        }
      }
      f.repoSubs.push(sub);
      (f.def.benchmarkRepoSubstitutions ??= []).push({ source: sub.source, target: sub.target });
    });
  }
}

function parseRepoTriple(f: FactorySpec, n: YNode, p: string): RepoRef {
  const c = f.c;
  const m = c.mapping(n, p);
  if (m === null) return emptyRepo();
  try {
    const r = emptyRepo();
    let v = m.require('codeForge');
    if (v !== null) {
      const [s, ok] = c.enum(v, p + '.codeForge', ...codeForgeValues);
      if (ok) {
        if (s !== 'GITHUB') c.errf(v, p + '.codeForge', `${p}.codeForge must be GITHUB for benchmark repository substitutions`);
        r.codeForge = s;
      }
    }
    v = m.require('owner');
    if (v !== null) r.owner = c.nonEmptyStr(v, p + '.owner')[0];
    v = m.require('name');
    if (v !== null) r.name = c.nonEmptyStr(v, p + '.name')[0];
    return r;
  } finally {
    m.finish();
  }
}

function eqFold(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

function ownsRepo(f: FactorySpec, r: RepoRef): boolean {
  return (f.def.repositories ?? []).some((x) => eqFold(x.codeForge, r.codeForge) && eqFold(x.owner, r.owner) && eqFold(x.name, r.name));
}

function validOwner(forge: string, s: string): boolean {
  if (/[ \t]/.test(s) || s.startsWith('/') || s.endsWith('/') || s.includes('//')) return false;
  if (forge === 'GITLAB' || forge === 'AZURE_DEVOPS') return true;
  return !s.includes('/');
}

function parseCloudProviders(f: FactorySpec, m: Mapping): void {
  const c = f.c;
  const key = 'cloudProviders';
  const v = m.get(key);
  if (v === null || isNull(v)) return;
  const cm = c.mapping(v, key);
  if (cm === null) return;
  try {
    const cp: NonNullable<CompiledDefinition['cloudProviders']> = {};
    const g = cm.get('gcp');
    if (g !== null && !isNull(g)) {
      const p = key + '.gcp';
      const gm = c.mapping(g, p);
      if (gm !== null) {
        const gcp: GCPProvider = { projectNumber: '', workloadIdentityFederationPoolId: '', workloadIdentityFederationProviderId: '' };
        let n = gm.require('projectNumber');
        if (n !== null) {
          const [s, ok] = c.nonEmptyStr(n, p + '.projectNumber');
          if (ok) {
            if (!/^[0-9]*$/.test(s)) c.errf(n, p + '.projectNumber', `${p}.projectNumber must contain only digits`);
            gcp.projectNumber = s;
          }
        }
        n = gm.require('workloadIdentityFederationPoolId');
        if (n !== null) gcp.workloadIdentityFederationPoolId = c.nonEmptyStr(n, p + '.workloadIdentityFederationPoolId')[0];
        n = gm.require('workloadIdentityFederationProviderId');
        if (n !== null) gcp.workloadIdentityFederationProviderId = c.nonEmptyStr(n, p + '.workloadIdentityFederationProviderId')[0];
        n = gm.get('serviceAccountEmail');
        if (n !== null && !isNull(n)) {
          const [s, ok] = c.str(n, p + '.serviceAccountEmail');
          if (ok) {
            if (!emailRe.test(s)) c.errf(n, p + '.serviceAccountEmail', `${p}.serviceAccountEmail ${goQuote(s)} is not an email address`);
            gcp.serviceAccountEmail = s;
          }
        }
        gm.finish();
        cp.gcp = gcp;
      }
    }
    // cloudProviders.aws is not supported: the control plane issues cloud
    // identity tokens for Google Cloud and for more audiences only.
    const aws = cm.get('aws');
    if (aws !== null && !isNull(aws)) {
      c.errf(
        cm.keyNode('aws'),
        key + '.aws',
        `${key}.aws is not supported; use ${key}.gcp, or ${key}.audiences for a service that accepts OIDC tokens`,
      );
    }
    const n = cm.get('audiences');
    if (n !== null && !isNull(n)) {
      const [list, ok] = c.uniqueStrList(n, key + '.audiences');
      if (ok && list !== null) {
        for (const a of list) {
          if (goTrimSpace(a) === '') c.errf(n, key + '.audiences', `${key}.audiences must not contain an empty audience`);
        }
        cp.audiences = list;
      }
    }
    f.def.cloudProviders = cp;
  } finally {
    cm.finish();
  }
}

function parseIntegrations(f: FactorySpec, m: Mapping): void {
  const { c, def } = f;
  if (!m.has('integrations')) return;
  const v = m.get('integrations');
  def.integrationsSet = true;
  def.integrations = [];
  if (isNull(v)) return;
  const seen = new Map<string, YNode>();
  (c.seq(v, 'integrations') ?? []).forEach((it, i) => {
    const p = indexPath('integrations', i);
    const im = c.mapping(it, p);
    if (im === null) return;
    const [typ, ok] = c.enum(im.require('type'), p + '.type', ...integrationTypes);
    const decl: IntegrationDecl = { type: typ };
    for (const t of integrationTypes) {
      const sv = im.get(t);
      if (sv === null || isNull(sv)) continue;
      if (t !== typ) {
        c.errf(im.keyNode(t), p + '.' + t, `${p}.${t} settings are only valid on an integration of type ${t}`);
        continue;
      }
      const settings = parseIntegrationSettings(c, t, sv, p + '.' + t);
      if (settings !== null) decl.settings = settings;
      else delete decl.settings;
    }
    im.finish();
    if (!ok) return;
    if (seen.has(typ)) {
      c.errf(im.node, p + '.type', `integration ${typ} is declared more than once`);
      return;
    }
    seen.set(typ, im.node);
    def.integrations!.push(decl);
  });
  if (seen.has('linear') && seen.has('jira')) {
    c.errf(seen.get('jira'), 'integrations', 'linear and jira are mutually exclusive; declare at most one issue tracker');
  }
}

function parseIntegrationSettings(c: Doc, typ: string, n: YNode, p: string): Record<string, string> | null {
  const m = c.mapping(n, p);
  if (m === null) return null;
  try {
    const out: Record<string, string> = {};
    switch (typ) {
      case 'slack':
      case 'microsoft-teams': {
        const v = m.get('autoRespondToThreadReplies');
        if (v !== null && !isNull(v)) {
          const [b, ok] = c.boolean(v, p + '.autoRespondToThreadReplies');
          if (ok) out['autoRespondToThreadReplies'] = b ? 'true' : 'false';
        }
        break;
      }
      case 'linear': {
        const v = m.get('teamIds');
        if (v !== null && !isNull(v)) c.strList(v, p + '.teamIds');
        break;
      }
      case 'jira': {
        const v = m.get('projectKeys');
        if (v !== null && !isNull(v)) c.strList(v, p + '.projectKeys');
        break;
      }
    }
    return Object.keys(out).length === 0 ? null : out;
  } finally {
    m.finish();
  }
}

function parseScorerDefaults(f: FactorySpec, n: YNode): void {
  const c = f.c;
  const m = c.mapping(n, 'scorerDefaults');
  if (m === null) return;
  try {
    const sd: ScorerDefaultsSpec = { machine: '', machineNode: null, secrets: null, mcp: null, model: null, harness: null };
    let v = m.get('machine');
    if (v !== null && !isNull(v)) {
      sd.machine = c.nonEmptyStr(v, 'scorerDefaults.machine')[0];
      sd.machineNode = v;
    }
    v = m.get('secrets');
    if (v !== null && !isNull(v)) sd.secrets = secretList(c, v, 'scorerDefaults.secrets')[0];
    v = m.get('mcpServers');
    if (v !== null && !isNull(v)) sd.mcp = parseMCPServers(c, v, 'scorerDefaults.mcpServers')[0];
    v = m.get('model');
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.nonEmptyStr(v, 'scorerDefaults.model');
      if (ok && checkModelHarness(c, v, 'scorerDefaults.model', s)) sd.model = s;
    }
    v = m.get('harness');
    if (v !== null && !isNull(v)) sd.harness = parseHarness(c, v, 'scorerDefaults.harness', false);
    f.scorerDefaults = sd;
    f.def.scorerDefaults = { machine: sd.machine, secrets: sd.secrets, mcp_servers: sd.mcp };
    if (sd.model !== null) f.def.scorerDefaults.model = sd.model;
  } finally {
    m.finish();
  }
}

function parseSelfImprovement(c: Doc, n: YNode): SelfImprovementConfig | null {
  const m = c.mapping(n, 'selfImprovement');
  if (m === null) return null;
  try {
    const si: SelfImprovementConfig = { reviewerType: 'admins' };
    let v = m.get('failedRunThreshold');
    if (v !== null && !isNull(v)) si.failedRunThreshold = c.intRange(v, 'selfImprovement.failedRunThreshold', 1, 50)[0];
    v = m.get('reviewerType');
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.enum(v, 'selfImprovement.reviewerType', 'admins', 'team', 'custom', 'none');
      if (ok) si.reviewerType = s;
    }
    const ev = m.get('reviewerEmails');
    const hasEmails = ev !== null && !isNull(ev);
    if (si.reviewerType === 'custom' && !hasEmails) {
      c.errf(m.node, 'selfImprovement.reviewerEmails', 'selfImprovement.reviewerEmails is required when reviewerType is custom');
    } else if (si.reviewerType !== 'custom' && hasEmails) {
      c.errf(ev, 'selfImprovement.reviewerEmails', 'selfImprovement.reviewerEmails is only valid when reviewerType is custom');
    } else if (hasEmails) {
      const [list, ok] = c.uniqueStrList(ev, 'selfImprovement.reviewerEmails');
      if (ok && list !== null && list.length === 0) {
        c.errf(ev, 'selfImprovement.reviewerEmails', 'selfImprovement.reviewerEmails must list at least one email');
      }
      (list ?? []).forEach((e, i) => {
        if (!emailRe.test(e)) c.errf(resolve(ev)!.content[i], indexPath('selfImprovement.reviewerEmails', i), `${goQuote(e)} is not an email address`);
      });
      if (list !== null) si.reviewerEmails = list;
    }
    v = m.get('analysisModel');
    if (v !== null && !isNull(v)) si.analysisModel = c.nonEmptyStr(v, 'selfImprovement.analysisModel')[0];
    return si;
  } finally {
    m.finish();
  }
}
