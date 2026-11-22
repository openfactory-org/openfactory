// Resolves authenticated forge clients for a team from its integration
// records, and the forge responder that posts replies as comments. Port of
// internal/forge/{forge.go,reviewers.go,usertokens.go (ForgeForUser)}.
//
// GitHub is built in. GitLab and Azure DevOps are plugin Workers behind
// service bindings (PLUGIN_GITLAB, PLUGIN_AZUREDEVOPS). The generic git forge
// is not available on Workers (it shelled out to git).
import {
  OfError,
  integrationActive,
  integrationStatus,
  type Integration,
  type OutboundMessage,
  type PullRequestRef,
  type SourceRef,
} from "@openfactory/core";
import { isNotSupported, type ForgePlugin, type ForgeTarget as PluginTarget } from "@openfactory/plugin-api";
import { GitHubClient, parseInstallationId, type TokenCache } from "./github/client";
import { AZURE_DEVOPS, GIT, GITHUB, GITLAB, type Forge, type ForgeLookup, type ForgeResolver } from "./forge";
import { statusCode, type FetchFn } from "@openfactory/core";
import { repoOwner, selectIntegration } from "./util/select";
import { formatOutbound, splitRepo, type Responder } from "@openfactory/core";

/** Maps a codeForge (GITHUB, gitlab, azure-devops...) to the integration provider. */
export function integrationProvider(codeForge: string): string {
  switch (codeForge.trim().replace(/-/g, "_").toUpperCase()) {
    case GITHUB:
    case "":
      return "github";
    case GITLAB:
      return "gitlab";
    case AZURE_DEVOPS:
      return "azure_devops";
    case GIT:
    case "OTHER":
    case "GENERIC":
      return "git";
  }
  return codeForge.toLowerCase();
}

/** Maps an integration or source provider to its forge identifier. */
export function forgeFromProvider(provider: string): string {
  switch (provider.replace(/-/g, "_").toLowerCase()) {
    case "github":
      return GITHUB;
    case "gitlab":
      return GITLAB;
    case "azure_devops":
    case "azuredevops":
      return AZURE_DEVOPS;
    case "git":
      return GIT;
  }
  return provider.toUpperCase();
}

/** Integration providers served by first-party plugin Workers: provider -> plugin name. */
export const PLUGIN_FORGES: Record<string, string> = { gitlab: "gitlab", azure_devops: "azuredevops" };

/** Read access to the integration records of a team (TeamDO integrations module, WS10b). */
export interface IntegrationSource {
  list(teamId: string, provider?: string): Promise<Integration[]>;
  /** Sets the status; returns true when it changed. */
  setStatus?(id: string, status: string, reason: string): Promise<boolean>;
}

/** Secret values of a team (TeamDO secrets module, WS4). */
export interface SecretSource {
  get(teamId: string, factoryId: string, name: string): Promise<string | undefined>;
}

/** Personal forge tokens of team members (see src/forges/usertokens.ts). */
export interface UserTokenSource {
  get(teamId: string, userId: string, codeForge: string): Promise<string | undefined>;
}

/** Plugin forge Workers by plugin name. */
export type PluginForges = Record<string, ForgePlugin | undefined>;

export interface ResolverOptions {
  integrations: IntegrationSource;
  secrets: SecretSource;
  userTokens?: UserTokenSource;
  plugins?: PluginForges;
  fetch?: FetchFn;
  now?: () => number;
  /** How long built clients are reused (default 10 minutes). */
  cacheTtlMs?: number;
  tokenCache?: TokenCache;
}

interface Cached {
  forge: Forge;
  teamId: string;
  expires: number;
}

function disabledError(in_: Integration): OfError {
  let msg = `forge: integration ${in_.id} (${in_.provider}) is ${integrationStatus(in_)}`;
  if (in_.status_reason) msg += ": " + in_.status_reason;
  return new OfError("integration_disabled", msg);
}

/** Resolver implements ForgeResolver: one record per action by the owner of the repository. */
export class Resolver implements ForgeResolver {
  private readonly cache = new Map<string, Cached>();
  private readonly now: () => number;

  constructor(private readonly opts: ResolverOptions) {
    this.now = opts.now ?? Date.now;
  }

  /** Drops cached clients of a team (after integrations or secrets change). */
  invalidate(teamId: string): void {
    for (const [k, c] of this.cache) if (c.teamId === teamId) this.cache.delete(k);
  }

  /** The record that serves a forge action (no status check). */
  async select(l: ForgeLookup): Promise<Integration> {
    const provider = integrationProvider(l.codeForge);
    const ints = (await this.opts.integrations.list(l.teamId, provider)).filter(
      (x) => x.provider.toLowerCase() === provider,
    );
    try {
      return selectIntegration(ints, provider, l.factoryId ?? "", repoOwner(provider, l.repo));
    } catch (e) {
      const err = e as OfError;
      throw new OfError(err.code, `forge: team ${l.teamId}, repository ${l.repo}: ${err.detail}`, {
        cause: e,
        ...(err.reason ? { reason: err.reason } : {}),
      });
    }
  }

  async forge(l: ForgeLookup): Promise<Forge> {
    const in_ = await this.select(l);
    if (!integrationActive(in_)) throw disabledError(in_);
    const f = await this.client(in_);
    if (f instanceof GitHubClient && l.scope && l.scope.length > 0) {
      const owner = repoOwner("github", l.repo).toLowerCase();
      return f.scoped(l.scope.filter((x) => owner === "" || repoOwner("github", x).toLowerCase() === owner));
    }
    return f;
  }

  private async client(in_: Integration): Promise<Forge> {
    const c = this.cache.get(in_.id);
    if (c && this.now() < c.expires) return c.forge;
    const f = await this.build(in_);
    this.cache.set(in_.id, {
      forge: f,
      teamId: in_.team_id,
      expires: this.now() + (this.opts.cacheTtlMs ?? 10 * 60_000),
    });
    return f;
  }

  private async secret(in_: Integration, name: string): Promise<string> {
    if (!name) return "";
    const v = await this.opts.secrets.get(in_.team_id, "", name);
    if (v === undefined)
      throw new OfError("resource_not_found", `forge: integration ${in_.id} secret ${name}: not found`);
    return v;
  }

  private async authFailed(in_: Integration, cause: unknown): Promise<void> {
    let reason = "the provider refused the credential";
    const code = statusCode(cause);
    if (code) reason += ` (HTTP ${code})`;
    try {
      await this.opts.integrations.setStatus?.(in_.id, "auth_failed", reason);
    } catch (e) {
      console.warn("forge: set integration status", in_.id, e);
    }
    this.invalidate(in_.team_id);
  }

  /** Builds a client for one record (no cache). */
  async build(in_: Integration): Promise<Forge> {
    const cfg = in_.config ?? {};
    const provider = in_.provider.toLowerCase();
    if (provider === "github") {
      let inst: number;
      try {
        inst = parseInstallationId(cfg["installation_id"]);
      } catch (e) {
        throw new Error(`forge: github integration ${in_.id}: invalid installation_id: ${(e as Error).message}`, {
          cause: e,
        });
      }
      const base = {
        baseURL: cfg["base_url"] ?? "",
        webURL: cfg["web_url"] ?? "",
        installationId: inst,
        onAuthFailure: (err: unknown) => void this.authFailed(in_, err),
        ...(this.opts.fetch ? { fetch: this.opts.fetch } : {}),
        ...(this.opts.now ? { now: this.opts.now } : {}),
        ...(this.opts.tokenCache ? { tokenCache: this.opts.tokenCache } : {}),
      };
      if (cfg["app_id"]) {
        const key = await this.secret(in_, cfg["private_key_ref"] || in_.secret_ref || "");
        return GitHubClient.create({ ...base, appId: cfg["app_id"], privateKey: key });
      }
      if (in_.secret_ref) return GitHubClient.create({ ...base, token: await this.secret(in_, in_.secret_ref) });
      throw new Error(`forge: github integration ${in_.id} has no credentials`);
    }
    if (provider === "git") {
      throw new OfError(
        "operation_not_supported",
        "forge: the generic git forge is not available on this server; use a GitHub, GitLab or Azure DevOps integration",
      );
    }
    const t: PluginTarget = {
      provider,
      integrationId: in_.id,
      teamId: in_.team_id,
      config: { ...cfg },
      token: "",
      userToken: false,
    };
    if (this.plugin(provider)) t.token = await this.secret(in_, in_.secret_ref ?? "");
    return this.pluginForge(t);
  }

  private plugin(provider: string): ForgePlugin | undefined {
    const name = PLUGIN_FORGES[provider] ?? provider;
    return this.opts.plugins?.[name];
  }

  private pluginForge(t: PluginTarget): Forge {
    const p = this.plugin(t.provider);
    if (p) return new PluginForge(p, t);
    const name = PLUGIN_FORGES[t.provider];
    if (name) {
      throw new OfError(
        "feature_not_available",
        `forge: ${forgeFromProvider(t.provider)} is provided by the "${name}" plugin Worker; deploy that plugin Worker and bind it to the control plane`,
        { reason: "plugin_not_installed" },
      );
    }
    throw new OfError("feature_not_available", `forge: unsupported provider "${t.provider}" (no plugin serves it)`, {
      reason: "plugin_not_installed",
    });
  }

  /**
   * A client that acts as userId (credentialStrategy CREATOR). When the record
   * supports personal tokens and the user has none, it fails with
   * external_authentication_required; there is no silent fallback.
   */
  async forgeForUser(l: ForgeLookup, userId: string): Promise<{ forge: Forge; userToken: boolean }> {
    if (!userId || !this.opts.userTokens || !supportsUserTokens(l.codeForge))
      return { forge: await this.forge(l), userToken: false };
    const provider = integrationProvider(l.codeForge);
    const in_ = await this.select(l);
    if (!integrationActive(in_)) throw disabledError(in_);
    const cfg = in_.config ?? {};
    if ((cfg["user_tokens"] ?? "").toLowerCase() === "false") return { forge: await this.forge(l), userToken: false };
    const tok = await this.opts.userTokens.get(l.teamId, userId, l.codeForge);
    if (!tok) {
      throw new OfError(
        "external_authentication_required",
        `user ${userId} has no linked ${l.codeForge.toLowerCase()} account: link it in the dashboard (Settings, Accounts)`,
        { reason: "no_linked_account", provider },
      );
    }
    if (provider === "github") {
      const f = await GitHubClient.create({
        baseURL: cfg["base_url"] ?? "",
        webURL: cfg["web_url"] ?? "",
        token: tok,
        ...(this.opts.fetch ? { fetch: this.opts.fetch } : {}),
      });
      return { forge: f, userToken: true };
    }
    return {
      forge: this.pluginForge({
        provider,
        integrationId: in_.id,
        teamId: l.teamId,
        config: { ...cfg },
        token: tok,
        userToken: true,
      }),
      userToken: true,
    };
  }

  /** Asks a team member for a review of a pull request (port of reviewers.go). */
  async requestReview(teamId: string, pr: PullRequestRef, userId: string): Promise<void> {
    const codeForge = pr.forge || GITHUB;
    const [owner, repo] = splitRepo(pr.repo);
    if (!owner || !repo || !(pr.number > 0)) {
      throw new OfError("invalid_request", `pull request "${pr.url}" needs a repository (owner/name) and a number`);
    }
    const l: ForgeLookup = { teamId, codeForge, repo: pr.repo };
    const { forge: uf } = await this.forgeForUser(l, userId);
    if (!uf.login)
      throw new OfError("operation_not_supported", `${codeForge.toLowerCase()} cannot read the login of a user`);
    let login: string;
    try {
      login = await uf.login();
    } catch (e) {
      if (isNotSupported(e)) throw e;
      throw new Error(`read the forge login of user ${userId}: ${(e as Error).message}`, { cause: e });
    }
    const tf = await this.forge(l);
    if (!tf.requestReviewers)
      throw new OfError("operation_not_supported", `${codeForge.toLowerCase()} cannot request a review`);
    await tf.requestReviewers(owner, repo, pr.number, [login]);
  }
}

/** Forges whose integrations support personal tokens. */
export const USER_TOKEN_FORGES = [GITHUB, GITLAB, AZURE_DEVOPS];

export function supportsUserTokens(codeForge: string): boolean {
  const p = integrationProvider(codeForge);
  return p === "github" || p === "gitlab" || p === "azure_devops";
}

/** A Forge backed by a plugin Worker (port of internal/plugin/adapters.go). */
export class PluginForge implements Forge {
  constructor(
    private readonly p: ForgePlugin,
    private readonly t: PluginTarget,
  ) {}
  private forgeName = "";
  name(): string {
    return this.forgeName || forgeFromProvider(this.t.provider);
  }
  cloneURL(owner: string, repo: string) {
    return this.p.cloneUrl(this.t, owner, repo);
  }
  defaultBranch(owner: string, repo: string) {
    return this.p.defaultBranch(this.t, owner, repo);
  }
  async createPullRequest(r: {
    owner: string;
    repo: string;
    head: string;
    base: string;
    title: string;
    body: string;
    draft?: boolean;
  }) {
    const pr = await this.p.createPullRequest(this.t, r.owner, r.repo, {
      head: r.head,
      base: r.base,
      title: r.title,
      body: r.body,
      draft: !!r.draft,
    });
    return { url: "", branch: "", state: "", ...pr } as PullRequestRef;
  }
  async getPullRequest(owner: string, repo: string, number: number) {
    const pr = await this.p.getPullRequest(this.t, owner, repo, number);
    return { url: "", branch: "", state: "", ...pr } as PullRequestRef;
  }
  comment(owner: string, repo: string, number: number, body: string) {
    return this.p.comment(this.t, owner, repo, number, body);
  }
  async commentIssue(owner: string, repo: string, number: number, body: string): Promise<void> {
    try {
      await this.p.commentIssue(this.t, owner, repo, number, body);
    } catch (e) {
      if (!isNotSupported(e)) throw e;
      await this.p.comment(this.t, owner, repo, number, body);
    }
  }
  commentWorkItem(project: string, id: number, body: string) {
    return this.p.commentWorkItem(this.t, project, id, body);
  }
  setCommitStatus(
    owner: string,
    repo: string,
    sha: string,
    st: { context: string; state: string; description?: string; targetUrl?: string },
  ) {
    return this.p.setCommitStatus(this.t, owner, repo, sha, st as Parameters<ForgePlugin["setCommitStatus"]>[4]);
  }
  async getFiles(owner: string, repo: string, ref: string, path: string) {
    const r = await this.p.getFiles(this.t, owner, repo, ref, path);
    return { files: r.files, sha: r.commitSha };
  }
  replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string) {
    return this.p.replyInThread(this.t, owner, repo, number, threadRef, body);
  }
  resolveThread(owner: string, repo: string, number: number, threadRef: string) {
    return this.p.resolveThread(this.t, owner, repo, number, threadRef);
  }
  submitReview(r: {
    owner: string;
    repo: string;
    number: number;
    event?: string;
    body: string;
    comments?: { path: string; line: number; side?: string; body: string }[];
  }) {
    return this.p.submitReview(this.t, r.owner, r.repo, r.number, {
      event: r.event ?? "",
      body: r.body,
      comments: r.comments ?? [],
    } as never);
  }
  markReady(owner: string, repo: string, number: number) {
    return this.p.markReady(this.t, owner, repo, number);
  }
  setWorkItemState(project: string, id: number, state: string) {
    return this.p.setWorkItemState(this.t, project, id, state);
  }
  attachPullRequestToWorkItem(project: string, id: number, pr: PullRequestRef) {
    return this.p.attachPullRequestToWorkItem(this.t, project, id, pr as never);
  }
  requestReviewers(owner: string, repo: string, number: number, logins: string[]) {
    return this.p.requestReviewers(this.t, owner, repo, number, logins);
  }
  login() {
    return this.p.login(this.t);
  }
  async reviewThread(owner: string, repo: string, number: number, threadRef: string) {
    const th = await this.p.getReviewThread(this.t, owner, repo, number, threadRef);
    return {
      ref: th.ref,
      ...(th.path ? { path: th.path } : {}),
      ...(th.line ? { line: th.line } : {}),
      resolved: th.resolved,
      comments: th.comments.map((c) => ({
        id: c.id,
        author: c.author,
        body: c.body,
        path: c.path ?? "",
        line: c.line ?? 0,
        created_at: c.createdAt,
      })),
    };
  }
  async fileDiff(owner: string, repo: string, number: number, path: string) {
    return (await this.p.getFileDiff(this.t, owner, repo, number, path)) as {
      path: string;
      patch: string;
      truncated?: boolean;
    };
  }
}

// ---------- Responder ----------

/** Posts replies as comments on the originating issue, pull request or work item. */
export class ForgeResponder implements Responder {
  private readonly p: string;
  constructor(
    private readonly resolver: ForgeResolver,
    provider: string,
  ) {
    this.p = provider.toLowerCase();
  }

  provider(): string {
    return this.p;
  }

  private async workItemTracker(teamId: string, issue: SourceRef, factoryId?: string): Promise<Forge> {
    if (issue.kind !== "work_item" || !issue.number || !issue.repo) {
      throw new OfError("operation_not_supported", `${this.p}: ${issue.kind} is not a tracker issue`);
    }
    const f = await this.resolver.forge({
      teamId,
      ...(factoryId ? { factoryId } : {}),
      codeForge: forgeFromProvider(this.p),
      repo: issue.repo,
    });
    if (!f.setWorkItemState || !f.attachPullRequestToWorkItem)
      throw new OfError("operation_not_supported", `forge ${f.name()} has no work items`);
    return f;
  }

  async setIssueState(teamId: string, issue: SourceRef, state: string, factoryId?: string): Promise<void> {
    const f = await this.workItemTracker(teamId, issue, factoryId);
    await f.setWorkItemState!(issue.repo!, issue.number!, state);
  }

  async attachPullRequest(teamId: string, issue: SourceRef, pr: PullRequestRef, factoryId?: string): Promise<void> {
    const f = await this.workItemTracker(teamId, issue, factoryId);
    await f.attachPullRequestToWorkItem!(issue.repo!, issue.number!, pr);
  }

  /** Sources without a comment thread (pushes, CI runs) are a no-op. */
  async reply(teamId: string, msg: OutboundMessage): Promise<void> {
    const src = msg.source;
    if (!src.number || !src.repo) return;
    if (src.kind !== "issue" && src.kind !== "pull_request" && src.kind !== "work_item") return;
    const f = await this.resolver.forge({
      teamId,
      ...(msg.factory_id ? { factoryId: msg.factory_id } : {}),
      codeForge: forgeFromProvider(this.p),
      repo: src.repo,
    });
    const body = formatOutbound(msg);
    if (src.kind === "work_item") {
      if (!f.commentWorkItem) throw new Error(`forge ${f.name()} cannot comment on work items`);
      await f.commentWorkItem(src.repo, src.number, body);
      return;
    }
    const [owner, name] = splitRepo(src.repo);
    if (src.kind === "issue") {
      if (f.commentIssue) await f.commentIssue(owner, name, src.number, body);
      else await f.comment(owner, name, src.number, body);
      return;
    }
    const thread = msg.thread_ref || src.thread_ref || "";
    if (thread !== "") {
      try {
        await f.replyInThread(owner, name, src.number, thread, body);
        return;
      } catch (e) {
        // A plugin Worker's OfError crosses RPC as a plain Error: match the code, not the class.
        if (!isNotSupported(e)) throw e;
      }
    }
    await f.comment(owner, name, src.number, body);
  }
}

/** Responders for every first-class forge. */
export function forgeResponders(resolver: ForgeResolver): Responder[] {
  return ["github", "gitlab", "azure_devops"].map((p) => new ForgeResponder(resolver, p));
}
