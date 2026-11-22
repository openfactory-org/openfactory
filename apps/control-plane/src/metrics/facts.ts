// Pull request facts: port of internal/metrics/facts.go. RecordEvent folded
// forge events into the table pull_request_facts. IndexDO stores the events
// (index.prFacts) in order; `FactsBook` replays them with the same rules, so
// the dashboard has the same facts that Go stored.
import {
  PUSH_BY_AGENT,
  PUSH_BY_BOT,
  PUSH_BY_PERSON,
  pullRequestFactsId,
  pullRequestOpen,
  sourceKey,
  type Actor,
  type PullRequestFacts,
  type PullRequestPush,
  type SourceRef,
} from "@openfactory/core";

/** The part of a forge event that the facts need (core.Event and CoreEvent both fit). */
export interface FactEvent {
  team_id: string;
  provider: string;
  type: string;
  actor: Pick<Actor, "handle" | "is_bot"> & { is_self?: boolean };
  source: Record<string, unknown> | SourceRef;
  attributes: Record<string, unknown> | null;
  received_at: string;
}

const forgeProviders = new Set(["github", "gitlab", "azure_devops"]);

function forgeProvider(p: string): boolean {
  return forgeProviders.has(p.trim().replaceAll("-", "_").toLowerCase());
}

function actorKind(a: FactEvent["actor"]): string {
  if (a.is_self) return PUSH_BY_AGENT;
  if (a.is_bot) return PUSH_BY_BOT;
  return PUSH_BY_PERSON;
}

function attrString(ev: FactEvent, k: string): string {
  const v = ev.attributes?.[k];
  return typeof v === "string" ? v : "";
}

function attrInt(ev: FactEvent, k: string): number | undefined {
  const v = ev.attributes?.[k];
  return typeof v === "number" && Number.isFinite(v) ? Math.trunc(v) : undefined;
}

function attrTime(ev: FactEvent, k: string): string | undefined {
  const s = attrString(ev, k);
  if (s === "") return undefined;
  const t = Date.parse(s);
  return Number.isNaN(t) ? undefined : new Date(t).toISOString();
}

function eventAction(ev: FactEvent): string {
  return attrString(ev, "action").toLowerCase();
}

const lower = (s: string): string => s.toLowerCase();

function isOpenedEvent(ev: FactEvent): boolean {
  switch (lower(ev.type)) {
    case "pull_request_opened":
    case "pull_request_created":
      return true;
    case "merge_request":
      return eventAction(ev) === "open";
  }
  return false;
}

function isMergedEvent(ev: FactEvent): boolean {
  const t = lower(ev.type);
  if (t.endsWith("_merged")) return true;
  if (ev.attributes?.["merged"] === true) return true;
  return t === "merge_request" && eventAction(ev) === "merge";
}

function isClosedEvent(ev: FactEvent): boolean {
  const t = lower(ev.type);
  return t.endsWith("_closed") || (t === "merge_request" && eventAction(ev) === "close");
}

function isReviewEvent(ev: FactEvent): boolean {
  const t = lower(ev.type);
  return t === "pull_request_review_submitted" || (t === "merge_request" && eventAction(ev) === "approved");
}

const isSynchronizeEvent = (ev: FactEvent): boolean => lower(ev.type) === "pull_request_synchronized";

const iso = (ms: number): string => new Date(ms).toISOString();
const msOf = (s: string): number => Date.parse(s);

/** addPush appends p unless a push with the same commit (or the same time and actor) exists. */
export function addPush(f: PullRequestFacts, p: PullRequestPush): boolean {
  for (const x of f.pushes ?? []) {
    if ((p.sha && x.sha === p.sha) || (!p.sha && msOf(x.at) === msOf(p.at) && x.actor === p.actor)) return false;
  }
  f.pushes = [...(f.pushes ?? []), p];
  return true;
}

function factsEqual(a: PullRequestFacts, b: PullRequestFacts): boolean {
  const eqT = (x?: string, y?: string): boolean => (x === undefined) === (y === undefined) && (x === undefined || msOf(x) === msOf(y!));
  return (
    (a.url ?? "") === (b.url ?? "") &&
    (a.head_branch ?? "") === (b.head_branch ?? "") &&
    (a.opener ?? "") === (b.opener ?? "") &&
    (a.opener_kind ?? "") === (b.opener_kind ?? "") &&
    eqT(a.opened_at, b.opened_at) &&
    eqT(a.first_review_at, b.first_review_at) &&
    eqT(a.merged_at, b.merged_at) &&
    eqT(a.closed_at, b.closed_at) &&
    a.additions === b.additions &&
    a.deletions === b.deletions &&
    (a.pushes ?? []).length === (b.pushes ?? []).length
  );
}

/** The facts of a team, built from forge events in the order they arrived. */
export class FactsBook {
  private readonly byId = new Map<string, PullRequestFacts>();

  /** All facts, in the order they were first seen. */
  list(): PullRequestFacts[] {
    return [...this.byId.values()];
  }

  get(teamId: string, key: string): PullRequestFacts | undefined {
    return this.byId.get(pullRequestFactsId(teamId, key));
  }

  /** Updates the facts from one forge event. Events of other providers and without a team are skipped. */
  record(ev: FactEvent, nowMs: number = Date.now()): void {
    if (!ev || ev.team_id === "" || !forgeProvider(ev.provider)) return;
    const recv = msOf(ev.received_at);
    const atMs = Number.isNaN(recv) || recv <= 0 ? nowMs : recv;
    const at = iso(atMs);
    if (lower(ev.type) === "push") {
      this.recordPush(ev, at, nowMs);
      return;
    }
    const src = ev.source as Partial<SourceRef>;
    if (src.kind !== "pull_request" || !src.number || !src.repo) return;
    const key = sourceKey({ ...(src as SourceRef), thread_ref: undefined });
    const id = pullRequestFactsId(ev.team_id, key);
    const existing = this.byId.get(id);
    const f: PullRequestFacts = existing
      ? structuredClone(existing)
      : {
          id,
          team_id: ev.team_id,
          key,
          provider: (src.provider ?? "").toLowerCase(),
          repo: src.repo,
          number: src.number,
          updated_at: "",
        };
    const before = structuredClone(f);
    if (src.url) f.url = src.url;
    const branch = attrString(ev, "head_branch");
    if (branch) f.head_branch = branch;
    const adds = attrInt(ev, "additions");
    if (adds !== undefined) f.additions = adds;
    const dels = attrInt(ev, "deletions");
    if (dels !== undefined) f.deletions = dels;
    if (!f.opened_at) {
      const created = attrTime(ev, "pr_created_at");
      if (created) f.opened_at = created;
      else if (isOpenedEvent(ev)) f.opened_at = at;
    }
    if (!f.opener) {
      if (isOpenedEvent(ev)) {
        f.opener = ev.actor.handle;
        f.opener_kind = actorKind(ev.actor);
      } else if (attrString(ev, "pr_opener") !== "") {
        f.opener = attrString(ev, "pr_opener");
        const isBot = ev.attributes?.["pr_opener_is_bot"] === true;
        if (lower(f.opener) === lower(ev.actor.handle)) f.opener_kind = actorKind(ev.actor);
        else if (isBot) f.opener_kind = PUSH_BY_BOT;
        else f.opener_kind = PUSH_BY_PERSON;
      }
    }
    if (isSynchronizeEvent(ev)) {
      addPush(f, { at, actor: ev.actor.handle, kind: actorKind(ev.actor), sha: attrString(ev, "head_sha") || undefined });
    } else if (isReviewEvent(ev)) {
      // The first review by someone other than the opener and the product's own bot.
      if (!f.first_review_at && !ev.actor.is_self && lower(ev.actor.handle) !== lower(f.opener ?? "")) f.first_review_at = at;
    }
    if (isMergedEvent(ev) && !f.merged_at) {
      f.merged_at = attrTime(ev, "merged_at") ?? at;
    } else if (isClosedEvent(ev) && !isMergedEvent(ev) && !f.closed_at && !f.merged_at) {
      f.closed_at = at;
    }
    if (existing && factsEqual(before, f) && before.updated_at !== "") return;
    f.updated_at = iso(nowMs);
    this.byId.set(id, f);
  }

  /** A push goes to the open pull requests whose head branch got it. */
  private recordPush(ev: FactEvent, at: string, nowMs: number): void {
    const branch = attrString(ev, "branch");
    const repo = (ev.source as Partial<SourceRef>).repo ?? "";
    if (branch === "" || repo === "") return;
    for (const f of this.byId.values()) {
      if (f.team_id !== ev.team_id || lower(f.provider) !== lower(ev.provider) || lower(f.repo) !== lower(repo)) continue;
      if (f.head_branch !== branch || !pullRequestOpen(f)) continue;
      if (f.opened_at && msOf(at) < msOf(f.opened_at)) continue;
      const push: PullRequestPush = { at, actor: ev.actor.handle, kind: actorKind(ev.actor) };
      const sha = attrString(ev, "head_sha");
      if (sha) push.sha = sha;
      if (!addPush(f, push)) continue;
      f.updated_at = iso(nowMs);
    }
  }
}

/** Folds events (oldest first) into facts. */
export function foldFacts(events: readonly FactEvent[], nowMs: number = Date.now()): PullRequestFacts[] {
  const book = new FactsBook();
  for (const ev of events) book.record(ev, nowMs);
  return book.list();
}
