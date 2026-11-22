// The model credential of a harness (harness.auth, the run environment, the
// default credential) and the helpers that collect secret values to redact.
import { OfError } from "@openfactory/core";
import type { Run } from "../../engine/types";
import { cfgOf, type PlannerDeps } from "./deps";

export const CREDENTIAL_MANAGED_SECRET = "managedSecret";
export const CREDENTIAL_WORKER_ENVIRONMENT = "workerEnvironment";
export const CREDENTIAL_DEFAULT = "default";
export const CREDENTIAL_NONE = "none";

export interface HarnessCredential {
  source: string;
  secret?: string;
  scope?: string;
  type?: string;
  reason?: string;
  candidates?: string[];
}

const CREDENTIAL_TYPES: Record<string, string[]> = {
  claude: ["anthropic_api_key", "anthropic_oauth_token"],
  codex: ["openai_api_key"],
};
const CREDENTIAL_ENV: Record<string, string[]> = {
  claude: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"],
  codex: ["OPENAI_API_KEY", "CODEX_API_KEY"],
};

const harnessName = (h: string) => (h || "claude").trim().toLowerCase();

/** harness.HasCredentialEnv */
export function hasCredentialEnv(h: string, env: Record<string, string>): boolean {
  return (CREDENTIAL_ENV[harnessName(h)] ?? []).some((k) => !!env[k]);
}

function looksOpenAI(model: string): boolean {
  const m = model.toLowerCase();
  return ["gpt", "o1", "o3", "o4", "openai/", "codex"].some((p) => m.startsWith(p));
}

function openAIEnv(h: string, v: string): Record<string, string> {
  return harnessName(h) === "codex" ? { OPENAI_API_KEY: v, CODEX_API_KEY: v } : { OPENAI_API_KEY: v };
}

/** HarnessAuthEnv: a credential as harness environment variables (model provider keys only; object storage uses R2 over S3 with SigV4). */
export function harnessAuthEnv(h: string, model: string, type: string, value: string): Record<string, string> {
  const v = value.trim();
  switch (type) {
    case "anthropic_api_key":
      return { ANTHROPIC_API_KEY: v };
    case "anthropic_oauth_token":
      return harnessName(h) === "claude" ? { CLAUDE_CODE_OAUTH_TOKEN: v } : {};
    case "openai_api_key":
      return openAIEnv(h, v);
  }
  switch (harnessName(h)) {
    case "codex":
      return openAIEnv(h, v);
    case "claude":
      return { ANTHROPIC_API_KEY: v };
  }
  return looksOpenAI(model) ? openAIEnv(h, v) : { ANTHROPIC_API_KEY: v };
}

/** headerSecrets: a header value and, for "Bearer <token>", the token alone. */
export function headerSecrets(v: string): string[] {
  const out = [v];
  const i = v.indexOf(" ");
  if (i > 0 && v.slice(i + 1)) {
    if (["bearer", "basic", "token"].includes(v.slice(0, i).toLowerCase())) out.push(v.slice(i + 1).trim());
  }
  return out;
}

/** redactList: distinct values of at least 4 characters, longest first. */
export function redactList(values: string[]): string[] {
  const set = new Set<string>();
  for (const v of values) if (v && v.length >= 4) set.add(v);
  return [...set].sort((a, b) => b.length - a.length || (a < b ? -1 : a > b ? 1 : 0));
}

/** findDefaultCredential: the one factory or team secret whose type the harness accepts. */
export async function findDefaultCredential(d: PlannerDeps, r: Run): Promise<HarnessCredential> {
  const none: HarnessCredential = { source: CREDENTIAL_NONE, reason: "no_secret" };
  const types = CREDENTIAL_TYPES[harnessName(r.config.harness?.type ?? "")] ?? [];
  if (types.length === 0) return none;
  const scopes: [string, string][] = r.factory_id
    ? [
        ["factory", r.factory_id],
        ["team", ""],
      ]
    : [["team", ""]];
  const shadowed = new Set<string>();
  for (const [scopeName, fid] of scopes) {
    const list = (await d.listSecrets(fid)).filter((s) => (s.factory_id ?? "") === fid);
    const fits = list.filter((s) => !shadowed.has(s.name) && types.includes(s.type));
    for (const s of list) shadowed.add(s.name);
    if (fits.length === 1)
      return { source: CREDENTIAL_DEFAULT, secret: fits[0]!.name, scope: scopeName, type: fits[0]!.type };
    if (fits.length > 1)
      return {
        source: CREDENTIAL_NONE,
        reason: "ambiguous",
        scope: scopeName,
        candidates: fits.map((s) => s.name).sort(),
      };
  }
  return none;
}

async function secretAuth(
  d: PlannerDeps,
  r: Run,
  factoryId: string,
  name: string,
): Promise<{ auth: Record<string, string>; type: string }> {
  const v = await d.secret(factoryId, name);
  if (v === undefined)
    throw new OfError("resource_not_found", `harness auth secret ${JSON.stringify(name)}: secret ${name} not found`);
  const list = await d.listSecrets(factoryId);
  const type = list.find((s) => s.name === name && (s.factory_id ?? "") === factoryId)?.type ?? "generic";
  return { auth: harnessAuthEnv(r.config.harness?.type ?? "", r.config.harness?.model ?? "", type, v), type };
}

/** resolveHarnessAuth: harness.auth, the run environment, the default credential, or nothing. */
export async function resolveHarnessAuth(
  d: PlannerDeps,
  r: Run,
  env: Record<string, string>,
): Promise<{ auth: Record<string, string>; credential: HarnessCredential }> {
  const a = cfgOf(r).harness?.auth;
  if (a) {
    if (a.source === CREDENTIAL_WORKER_ENVIRONMENT)
      return { auth: {}, credential: { source: CREDENTIAL_WORKER_ENVIRONMENT } };
    if (!a.secretName || (a.source && a.source !== CREDENTIAL_MANAGED_SECRET))
      return { auth: {}, credential: { source: CREDENTIAL_NONE } };
    const { auth, type } = await secretAuth(d, r, r.factory_id, a.secretName);
    return { auth, credential: { source: CREDENTIAL_MANAGED_SECRET, secret: a.secretName, type } };
  }
  if (hasCredentialEnv(r.config.harness?.type ?? "", env))
    return { auth: {}, credential: { source: CREDENTIAL_NONE, reason: "run_environment" } };
  const cred = await findDefaultCredential(d, r);
  if (cred.source !== CREDENTIAL_DEFAULT) return { auth: {}, credential: cred };
  const { auth } = await secretAuth(d, r, cred.scope === "factory" ? r.factory_id : "", cred.secret!);
  return { auth, credential: cred };
}
