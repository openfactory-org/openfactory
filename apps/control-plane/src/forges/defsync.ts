// Keeps git-backed factory definitions in step with their production branch
// and publishes the definition check on pull requests. Port of
// internal/server/defsync.go. Definitions are read with the forge content
// APIs only (no git). Network I/O happens here, in the Worker; the apply
// itself is one TeamDO transaction with no network I/O inside.
import {
  OfError,
  clipBytes,
  factoryLink,
  statusCode,
  utf8,
  type Integration,
  type ValidationError,
} from "@openfactory/core";
import type { Forge, ForgeResolver } from "./forge";
import { checkName, normalizeRepo } from "./github/events";
import type { DefinitionPR } from "./github/webhook";
import { isNoIntegration } from "./util/select";
import { splitRepo } from "@openfactory/core";

export interface GitLocation {
  codeForge: string;
  owner: string;
  repo: string;
}

/**
 * Maps git_repo ("github.com/acme/defs", "https://gitlab.example.com/g/sub/defs.git",
 * "acme/defs") to a forge and repository. Bare owner/name means GitHub;
 * unknown hosts map to GIT.
 */
export function parseGitRepo(raw: string): GitLocation {
  const s = raw.trim();
  if (s === "") throw new OfError("invalid_request", "empty git repository");
  let host = "";
  let path = s;
  let parsed: URL | null;
  try {
    parsed = /^[a-z][a-z0-9+.-]*:\/\//i.test(s) ? new URL(s) : null;
  } catch {
    parsed = null;
  }
  if (parsed && parsed.host) {
    host = parsed.hostname;
    path = decodeURIComponent(parsed.pathname);
  } else {
    const i = s.indexOf("/");
    if (i > 0 && s.slice(0, i).includes(".")) {
      host = s.slice(0, i);
      path = s.slice(i + 1);
    }
  }
  path = path.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
  const [owner0, name] = splitRepo(path);
  let owner = owner0;
  if (!owner || !name) throw new OfError("invalid_request", `git repository "${raw}" must name owner/repository`);
  const h = host.toLowerCase();
  let forge: string;
  if (h === "" || h.includes("github")) forge = "GITHUB";
  else if (h.includes("gitlab")) forge = "GITLAB";
  else if (h.includes("dev.azure.com") || h.includes("visualstudio.com")) {
    forge = "AZURE_DEVOPS";
    owner = owner.replace("/_git", "");
  } else forge = "GIT";
  return { codeForge: forge, owner, repo: name };
}

/** A git-backed factory as defsync needs it (core.Factory JSON fields). */
export interface GitFactory {
  id: string;
  team_id: string;
  source?: string;
  git_repo?: string;
  git_branch?: string;
  git_path?: string;
  active_version_id?: string;
}

export const branchOf = (f: GitFactory): string => f.git_branch || "main";

/** Result of the compiler and preflight for one tree, with the diff against the active version. */
export interface CheckResult {
  valid: boolean;
  errors: ValidationError[];
  warnings: ValidationError[];
  skipped: { check: string; reason: string }[];
  diff?: { empty: boolean; added: number; removed: number; changed: number; summary: string };
}

export interface DefSyncDeps {
  resolver: ForgeResolver;
  publicURL: string;
  /** The last commit applied or attempted (TeamDO KV "defsync:<factory>:last_sha"). */
  lastSha: { get(f: GitFactory): Promise<string>; set(f: GitFactory, sha: string): Promise<void> };
  /** commit_sha of the active version ("" when unknown). */
  activeCommitSha?(f: GitFactory): Promise<string>;
  /** Applies files as a new version. invalid: the definition has validation errors. */
  apply(
    f: GitFactory,
    files: Record<string, string>,
    message: string,
    sha: string,
  ): Promise<{ versionId: string } | { invalid: true; errors: ValidationError[] }>;
  check(f: GitFactory, files: Record<string, string>): Promise<CheckResult>;
}

function short(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha;
}

async function forgeOf(deps: Pick<DefSyncDeps, "resolver">, f: GitFactory, loc: GitLocation): Promise<Forge> {
  return deps.resolver.forge({
    teamId: f.team_id,
    factoryId: f.id,
    codeForge: loc.codeForge,
    repo: `${loc.owner}/${loc.repo}`,
  });
}

/** Fetches the production branch and applies it when its commit is new. */
export async function sync(deps: DefSyncDeps, f: GitFactory): Promise<void> {
  if (f.source !== "git" || !f.git_repo) return;
  const loc = parseGitRepo(f.git_repo);
  const fg = await forgeOf(deps, f, loc);
  let got: { files: Record<string, string>; sha: string };
  try {
    got = await fg.getFiles(loc.owner, loc.repo, branchOf(f), f.git_path ?? "");
  } catch (e) {
    throw new Error(`fetch definition: ${(e as Error).message}`, { cause: e });
  }
  const { files, sha } = got;
  if (sha !== "") {
    if ((await deps.lastSha.get(f)) === sha) return;
    if (f.active_version_id && deps.activeCommitSha && (await deps.activeCommitSha(f)) === sha) {
      await deps.lastSha.set(f, sha);
      return;
    }
  }
  let msg = "Sync " + branchOf(f);
  if (sha !== "") msg += " @ " + short(sha);
  const r = await deps.apply(f, files, msg, sha);
  if ("invalid" in r) {
    if (sha !== "") await deps.lastSha.set(f, sha);
    console.warn("git definition invalid; last valid version kept", f.id, sha, r.errors.length);
    return;
  }
  if (sha !== "") await deps.lastSha.set(f, sha);
  console.info("git definition applied", f.id, sha, r.versionId);
}

/** Syncs each git-backed factory of a push to its production branch. */
export async function onPush(deps: DefSyncDeps, factories: GitFactory[], repo: string, branch: string): Promise<void> {
  const want = normalizeRepo(repo);
  for (const f of factories) {
    if (f.source !== "git" || !f.git_repo || normalizeRepo(f.git_repo) !== want || branchOf(f) !== branch) continue;
    try {
      await sync(deps, f);
    } catch (e) {
      console.warn("sync git definition after push", f.id, (e as Error).message);
    }
  }
}

export function issueText(e: ValidationError): string {
  let loc = e.file;
  if (e.line && e.line > 0) loc = `${e.file}:${e.line}`;
  if (e.field) loc += " " + e.field;
  return loc + ": " + e.message;
}

interface DefinitionCheckResult {
  conclusion: "success" | "failure" | "error";
  title: string;
  summary: string;
}

/** Renders a check result for a check run or a commit status. */
export function renderCheck(chk: CheckResult): DefinitionCheckResult {
  let b = "";
  const writeIssues = (head: string, list: ValidationError[]) => {
    if (list.length === 0) return;
    b += `\n${head}:\n`;
    for (const e of list) b += `- ${issueText(e)}\n`;
  };
  if (!chk.valid) {
    let title = `${chk.errors.length} error(s)`;
    if (chk.errors.length > 0) title += ": " + issueText(chk.errors[0]!);
    writeIssues("Errors", chk.errors);
    writeIssues("Warnings", chk.warnings);
    return { conclusion: "failure", title, summary: b.trim() };
  }
  let title = "Definition is valid";
  if (!chk.diff || chk.diff.empty) title += "; no resource changes";
  else title += `: ${chk.diff.added} added, ${chk.diff.removed} removed, ${chk.diff.changed} changed`;
  b += chk.diff?.summary ?? "";
  writeIssues("Warnings", chk.warnings);
  if (chk.skipped.length > 0) {
    b += "\nChecks not done:\n";
    for (const s of chk.skipped) b += `- ${s.check}: ${s.reason}\n`;
  }
  return { conclusion: "success", title, summary: b.trim() };
}

/** Collapses spaces and cuts to 140 bytes with "...". */
export function truncateDesc(s0: string): string {
  const s = s0
    .split(/\s+/)
    .filter((x) => x)
    .join(" ");
  if (utf8(s).length <= 140) return s;
  return clipBytes(s, 137) + "...";
}

/** The branch of a git source must compile and pass the preflight before a factory switches to git. */
export async function checkBranch(deps: Pick<DefSyncDeps, "resolver" | "check">, f: GitFactory): Promise<void> {
  const loc = parseGitRepo(f.git_repo ?? "");
  const fg = await forgeOf(deps, f, loc);
  let files: Record<string, string>;
  try {
    files = (await fg.getFiles(loc.owner, loc.repo, branchOf(f), f.git_path ?? "")).files;
  } catch (e) {
    throw new OfError(
      "invalid_request",
      `could not read the definition from ${f.git_repo} branch "${branchOf(f)}": ${(e as Error).message}`,
      { cause: e },
    );
  }
  const chk = await deps.check(f, files);
  if (!chk.valid) {
    let msg = `the definition on the branch has ${chk.errors.length} error(s)`;
    if (chk.errors.length > 0) msg += ": " + issueText(chk.errors[0]!);
    throw new OfError("invalid_request", msg, { extra: { errors: chk.errors } });
  }
}

/**
 * checkGitBranch (internal/api/factories.go): checkBranch for the factory
 * routes. outcome says what the refusal left as it was. Errors on the branch
 * come back as a validation error; a source that cannot be read gives a
 * short message for people (gitSourceProblem), and the cause goes to the log.
 */
export async function checkGitSource(
  deps: Pick<DefSyncDeps, "resolver" | "check">,
  f: GitFactory,
  outcome: string,
  integrations: readonly Integration[] = [],
): Promise<void> {
  try {
    await checkBranch(deps, f);
  } catch (e) {
    const errs = e instanceof OfError ? (e.extra?.errors as ValidationError[] | undefined) : undefined;
    if (errs) {
      throw new OfError(
        "invalid_request",
        `the definition on branch "${branchOf(f)}" of ${f.git_repo} is invalid; ${outcome}`,
        { extra: { errors: errs } },
      );
    }
    console.warn("git definition source check failed", f.id, f.git_repo, branchOf(f), (e as Error)?.message);
    throw new OfError(
      "invalid_request",
      `${gitSourceProblem(f, e, gitSourceIntegration(f, integrations))} ${outcome.charAt(0).toUpperCase() + outcome.slice(1)}.`,
      { cause: e },
    );
  }
}

const FORGE_PROVIDERS = ["github", "gitlab", "azure_devops"];

/**
 * gitSourceIntegration: the code host record that serves the owner of the git
 * source of f: the record of the factory, else the team record, else undefined.
 */
export function gitSourceIntegration(f: GitFactory, integrations: readonly Integration[]): Integration | undefined {
  const owner = gitRepoOwner(f.git_repo ?? "");
  if (!owner) return undefined;
  let team: Integration | undefined;
  for (const i of integrations) {
    if (!FORGE_PROVIDERS.includes(i.provider) || String(i.config?.["org"] ?? "").toLowerCase() !== owner.toLowerCase())
      continue;
    if (i.factory_id === f.id) return i;
    if (!i.factory_id && !team) team = i;
  }
  return team;
}

/** gitRepoOwner: "acme" for "acme/defs", "https://github.com/acme/defs" and "github.com/acme/defs". */
export function gitRepoOwner(raw: string): string {
  let path = raw.trim();
  let isUrl = false;
  try {
    const u = new URL(path);
    if (u.host) {
      path = u.pathname;
      isUrl = true;
    }
  } catch {
    /* not a URL */
  }
  if (!isUrl) {
    const i = path.indexOf("/");
    if (i > 0 && path.slice(0, i).includes(".")) path = path.slice(i + 1);
  }
  return path.replace(/^\/+|\/+$/g, "").split("/")[0] ?? "";
}

/** gitSourceProblem: why the git source of f could not be read, in words that say what to fix. */
export function gitSourceProblem(f: GitFactory, err: unknown, integration?: Pick<Integration, "name">): string {
  const which = integration ? `of integration ${integration.name}` : "of its integration";
  const repo = f.git_repo ?? "";
  const branch = branchOf(f);
  const status = statusCode(err);
  if (isNoIntegration(err) || isNoIntegration((err as { cause?: unknown })?.cause))
    return `No code host integration covers ${repo}. Connect one under Integrations.`;
  if (err instanceof OfError && err.code === "integration_disabled")
    return integration
      ? `The code host integration ${integration.name} is not active. Fix it under Integrations.`
      : `The code host integration for ${repo} is not active. Fix it under Integrations.`;
  if (status === 401) return `The code host refused the token ${which} (HTTP 401). Check the token secret.`;
  if (status === 403) return `The token ${which} cannot read ${repo} (HTTP 403). Check the permissions of the token.`;
  if (status === 404)
    return `The code host did not find branch "${branch}" of ${repo}, or the token ${which} cannot see the repository (HTTP 404). Check the repository, the branch and the path.`;
  if (status !== 0)
    return `The code host answered HTTP ${status} for branch "${branch}" of ${repo}. Try again later; the server log has the details.`;
  if (err instanceof OfError && err.code === "invalid_request" && !err.cause) {
    const msg = err.detail;
    if (msg && msg.length <= 200 && !msg.includes(": ") && !msg.includes("://"))
      return msg.charAt(0).toUpperCase() + msg.slice(1).replace(/\.$/, "") + ".";
  }
  return `The definition on branch "${branch}" of ${repo} could not be read. The server log has the details.`;
}

/** Validates a pull request's definition and publishes the definition check. */
export async function checkPullRequest(deps: DefSyncDeps, f: GitFactory, pr: DefinitionPR): Promise<void> {
  const fg = await deps.resolver.forge({ teamId: f.team_id, factoryId: f.id, codeForge: "GITHUB", repo: pr.repo });
  const [owner, name] = splitRepo(pr.repo);
  const cname = pr.checkName || checkName(f.git_path ?? "");
  const target = factoryLink(deps.publicURL, f.id);
  const publish = async (res: DefinitionCheckResult) => {
    if (fg.publishCheckRun) {
      await fg.publishCheckRun(owner, name, {
        name: cname,
        headSha: pr.headSha,
        status: "completed",
        conclusion: res.conclusion === "error" ? "failure" : res.conclusion,
        title: truncateDesc(res.title),
        summary: res.summary,
        detailsUrl: target,
      });
      return;
    }
    await fg.setCommitStatus(owner, name, pr.headSha, {
      context: cname,
      state: res.conclusion,
      description: truncateDesc(res.title),
      targetUrl: target,
    });
  };
  try {
    if (fg.publishCheckRun) {
      await fg.publishCheckRun(owner, name, {
        name: cname,
        headSha: pr.headSha,
        status: "in_progress",
        title: "Validating factory definition",
        detailsUrl: target,
      });
    } else {
      await fg.setCommitStatus(owner, name, pr.headSha, {
        context: cname,
        state: "pending",
        description: "Validating factory definition",
        targetUrl: target,
      });
    }
  } catch {
    /* the final result is published below */
  }
  let files: Record<string, string>;
  try {
    files = (await fg.getFiles(owner, name, pr.headSha, f.git_path ?? "")).files;
  } catch (e) {
    return publish({
      conclusion: "error",
      title: "Could not read the definition: " + (e as Error).message,
      summary: "",
    });
  }
  let chk: CheckResult;
  try {
    chk = await deps.check(f, files);
  } catch (e) {
    return publish({
      conclusion: "error",
      title: "Could not check the definition: " + (e as Error).message,
      summary: "",
    });
  }
  await publish(renderCheck(chk));
}
