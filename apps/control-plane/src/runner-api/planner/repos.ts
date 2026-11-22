// The repositories of a run: repository specs, the forge credential strategy,
// and checkouts whose clone URLs hold a forge credential.
import { invalid } from "@openfactory/core";
import type { Run } from "../../engine/types";
import type { Forge } from "../../forges/forge";
import { wrap, type Environment, type PlannerDeps, type PlannerState, type Snapshot } from "./deps";

// The forge of a repository is a free string here: a repository spec may
// name any forge, and the forge resolver decides whether it exists.
export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
}
export interface RepoAtCommit {
  codeForge: string;
  owner: string;
  repo: string;
  ref: string;
}
export interface RepoStart extends RepoAtCommit {
  path?: string;
  resolved?: boolean;
}

export interface Checkout {
  forge: string;
  owner: string;
  name: string;
  path: string;
  ref: string;
  cloneUrl: string;
}

export const CREDENTIAL_EXECUTOR = "EXECUTOR";
export const CREDENTIAL_CREATOR = "CREATOR";

/** ParseRepoSpec: "forge:owner/name", "owner/name" (GitHub) or a forge URL. */
export function parseRepoSpec(spec: string): RepoRef {
  let s = spec.trim();
  let forge = "GITHUB";
  const i = s.indexOf(":");
  if (i > 0 && !s.slice(0, i).includes("/")) {
    const f = s.slice(0, i).replaceAll("-", "_").toUpperCase();
    if (f === "HTTPS" || f === "HTTP") {
      try {
        const u = new URL(s);
        s = u.pathname.replace(/^\//, "");
        if (u.host.includes("gitlab")) forge = "GITLAB";
        else if (u.host.includes("dev.azure.com") || u.host.includes("visualstudio.com")) forge = "AZURE_DEVOPS";
      } catch {
        /* keep s */
      }
    } else {
      forge = f;
      s = s.slice(i + 1);
    }
  }
  s = s.replace(/^\/+|\/+$/g, "");
  if (s.endsWith(".git")) s = s.slice(0, -4);
  const j = s.lastIndexOf("/");
  if (j <= 0 || j === s.length - 1) throw invalid(`invalid repository ${JSON.stringify(s)} (want forge:owner/name)`);
  return { codeForge: forge, owner: s.slice(0, j), name: s.slice(j + 1) };
}

export function isCommitSha(s: string): boolean {
  return /^[0-9a-f]{40}$/.test(s);
}

/** EffectiveCredentialStrategy: the agent's, else the factory's, else EXECUTOR. */
export function effectiveCredentialStrategy(agent: string | undefined, factory: string | undefined): string {
  let s = (agent ?? "").trim().toUpperCase();
  if (!s) s = (factory ?? "").trim().toUpperCase();
  return s === CREDENTIAL_CREATOR ? CREDENTIAL_CREATOR : CREDENTIAL_EXECUTOR;
}

/** credentialScope: the repositories of a run by forge, as "owner/name". */
export function credentialScope(refs: RepoRef[], start: RepoAtCommit[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  const seen = new Set<string>();
  const add = (forge: string, owner: string, name: string) => {
    const f = forge.toUpperCase();
    const full = `${owner}/${name}`;
    const k = `${f}|${full.toLowerCase()}`;
    if (seen.has(k)) return;
    seen.add(k);
    (out[f] ??= []).push(full);
  };
  for (const r of refs) add(r.codeForge, r.owner, r.name);
  for (const r of start) add(r.codeForge, r.owner, r.repo);
  return out;
}

/** splitCloneURL: the URL without user info, and the user info. */
export function splitCloneUrl(raw: string): { clean: string; username?: string; password?: string } {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    if (raw.includes("@")) throw new Error("clone URL cannot be parsed and may hold credentials");
    return { clean: raw };
  }
  const username = u.username ? decodeURIComponent(u.username) : undefined;
  const password = u.password ? decodeURIComponent(u.password) : undefined;
  u.username = "";
  u.password = "";
  return {
    clean: u.toString(),
    ...(username !== undefined ? { username } : {}),
    ...(password !== undefined ? { password } : {}),
  };
}

/** applyRepoRefs: pins checkouts to the starting commits; adds missing repositories. */
export async function applyRepoRefs(
  repos: Checkout[],
  refs: RepoAtCommit[],
  cloneUrl: (forge: string, owner: string, name: string) => Promise<string>,
): Promise<Checkout[]> {
  for (const ref of refs) {
    const forge = ref.codeForge.toUpperCase();
    let found = false;
    for (const r of repos) {
      if (
        r.forge.toUpperCase() === forge &&
        r.owner.toLowerCase() === ref.owner.toLowerCase() &&
        r.name.toLowerCase() === ref.repo.toLowerCase()
      ) {
        r.ref = ref.ref;
        found = true;
      }
    }
    if (found) continue;
    let u: string;
    try {
      u = await cloneUrl(forge, ref.owner, ref.repo);
    } catch (e) {
      throw wrap(e, `starting repository ${ref.owner}/${ref.repo}`);
    }
    repos.push({ forge, owner: ref.owner, name: ref.repo, cloneUrl: u, ref: ref.ref, path: ref.repo });
  }
  return repos;
}

/** credentialUser: the person whose forge credential a CREATOR run uses, else "". */
export function credentialUser(r: Run, snap: Snapshot, st: PlannerState): string {
  if (
    effectiveCredentialStrategy(r.config.credential_strategy, snap.runtime.credential_strategy) !== CREDENTIAL_CREATOR
  )
    return "";
  if (r.run_as) return r.run_as.type === "user" && r.run_as.id ? r.run_as.id : "";
  return st.workItem?.creator_id ?? "";
}

async function forgeFor(
  d: PlannerDeps,
  r: Run,
  codeForge: string,
  repo: string,
  userId: string,
  scope: string[] | undefined,
): Promise<Forge> {
  const l = { teamId: r.team_id, factoryId: r.factory_id, codeForge, repo, ...(scope ? { scope } : {}) };
  if (userId && d.forges.forgeForUser) {
    const res = await d.forges.forgeForUser(l, userId);
    if (res.userToken) return res.forge;
  }
  return d.forges.forge(l);
}

/**
 * A filter on repository names for the last element of a clone URL path.
 * Every forge puts the repository name last in its clone URL, so a
 * repository whose name differs cannot have that URL. Names compare without
 * case: a starting commit pins a configured repository without case, and the
 * filter must keep or drop both together.
 */
export function repoNamed(urlPath: string): (name: string) => boolean {
  const last = urlPath.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
  const raw = last.slice(last.lastIndexOf("/") + 1).toLowerCase();
  let decoded = raw;
  try {
    decoded = decodeURIComponent(raw);
  } catch {
    /* keep raw */
  }
  return (name) => {
    const n = name.toLowerCase();
    return n === raw || n === decoded;
  };
}

/**
 * checkouts: the repositories of a run, each with a clone URL that holds a
 * credential. With only, the forge mints clone URLs for the matching
 * repositories alone.
 */
export async function checkouts(
  d: PlannerDeps,
  r: Run,
  snap: Snapshot,
  st: PlannerState,
  envr?: Environment,
  only?: (name: string) => boolean,
): Promise<Checkout[]> {
  const refs: RepoRef[] = envr ? (envr.repos ?? []).map(parseRepoSpec) : (snap.runtime.repositories ?? []);
  const user = credentialUser(r, snap, st);
  // The scope of a forge token covers every repository of the run, also when
  // only some of them get a clone URL.
  const scope = credentialScope(refs, st.repoRefs);
  const repos: Checkout[] = [];
  for (const ref of refs) {
    if (only && !only(ref.name)) continue;
    const forge = ref.codeForge.toUpperCase();
    const full = `${ref.owner}/${ref.name}`;
    let url: string;
    try {
      const f = await forgeFor(d, r, forge, full, user, scope[forge]);
      url = await f.cloneURL(ref.owner, ref.name);
    } catch (e) {
      throw wrap(e, `repository ${full}`);
    }
    repos.push({ forge, owner: ref.owner, name: ref.name, path: ref.name, ref: "", cloneUrl: url });
  }
  const starts = only ? st.repoRefs.filter((x) => only(x.repo)) : st.repoRefs;
  if (starts.length > 0) {
    await applyRepoRefs(repos, starts, async (forge, owner, name) => {
      const f = await forgeFor(d, r, forge, `${owner}/${name}`, user, scope[forge.toUpperCase()]);
      return f.cloneURL(owner, name);
    });
  }
  return repos;
}
