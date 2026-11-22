// Event → automation matching. Port of internal/triggers/match.go.
import type { Automation, Event, Trigger } from "@openfactory/core";
import * as T from "./attributes";
import { CATALOG, GITHUB_CI_EVENTS, SELF_IGNORED_PROVIDERS, SKIP_BOTS_ALL, SKIP_BOTS_SELF, canonicalProvider } from "./catalog";
import { validateCron } from "./cron";
import { InvalidFilterError, attrBool, collect, evalCondition, parseFilter } from "./filter";
import { matchPayload } from "./payload";

export { canonicalProvider };

/** The event fields the matcher reads. payload may be raw JSON text for exact number matching. */
export type MatchEvent = Pick<Event, "provider" | "type" | "actor" | "attributes" | "text" | "factory_id" | "payload">;

/**
 * Whether ev starts automation a: enabled, same factory (when the event has
 * one), and at least one trigger matches. Evaluation errors fail closed.
 */
export function match(ev: MatchEvent | null | undefined, a: Automation): boolean {
  if (!ev || !a.enabled) return false;
  if (ev.factory_id && a.factory_id && ev.factory_id !== a.factory_id) return false;
  for (const [i, t] of (a.triggers ?? []).entries()) {
    if (canonicalProvider(t.provider) === T.ProviderSchedule) {
      if (ev.attributes?.[T.AttrAutomationID] !== a.id) continue;
    }
    try {
      if (matchTrigger(ev, t)) return true;
    } catch (e) {
      console.warn("trigger evaluation failed; not matching", a.id, a.name, i, t.provider, t.event, (e as Error).message);
    }
  }
  return false;
}

/** The automations of list that ev starts. */
export function matchingAutomations(ev: MatchEvent, list: readonly Automation[]): Automation[] {
  return list.filter((a) => match(ev, a));
}

function eventTypeMatches(provider: string, trigger: string, got: string): boolean {
  if (trigger === got) return true;
  return provider === T.ProviderSlack && trigger === "message_dm" && (got === "message_im" || got === "message_mpim");
}

/**
 * Evaluates one trigger: provider, event type, every filter key, payload
 * pattern and the provider defaults (GitHub skip_bots, commit_status states,
 * self-authored activity). Throws for a malformed filter (treat as no match).
 */
export function matchTrigger(ev: MatchEvent, t: Trigger): boolean {
  const provider = canonicalProvider(t.provider);
  if (canonicalProvider(ev.provider) !== provider) return false;
  if (!eventTypeMatches(provider, t.event, ev.type)) return false;
  const pf = parseFilter(provider, t.event, t.filter, "match");
  const attrs = (ev.attributes ?? {}) as Record<string, unknown>;
  const isSelf = attrBool(attrs, T.AttrActorIsSelf);
  const isBot = ev.actor?.is_bot || attrBool(attrs, T.AttrActorIsBot) || isSelf;
  if (SELF_IGNORED_PROVIDERS.has(provider) && isSelf) return false;
  if (provider === T.ProviderGitHub) {
    if (pf.skipBots === SKIP_BOTS_ALL && isBot) return false;
    if (pf.skipBots === SKIP_BOTS_SELF && isSelf) return false;
    if (t.event === "commit_status") {
      const st = typeof attrs[T.AttrCommitState] === "string" ? (attrs[T.AttrCommitState] as string).toLowerCase() : "";
      if (st !== "success" && st !== "failure" && st !== "error") return false;
    }
    if (GITHUB_CI_EVENTS.has(t.event) && pf.conds.some((c) => c.key === "labels" || c.key === "authors")) {
      const n = collect(attrs, [T.AttrLinkedPRCount], "int").values;
      if (n.length !== 1 || n[0] !== "1") return false;
    }
  } else if (provider === T.ProviderSchedule) {
    const want = t.schedule?.name ?? "";
    const got = typeof attrs[T.AttrScheduleName] === "string" ? attrs[T.AttrScheduleName] : "";
    if (got !== want) return false;
  }
  let text = ev.text ?? "";
  if (text === "" && typeof attrs[T.AttrText] === "string") text = attrs[T.AttrText] as string;
  for (const c of pf.conds) if (!evalCondition(attrs, text, c)) return false;
  if (pf.payload !== undefined && !matchPayload(pf.payload, ev.payload)) return false;
  return true;
}

/**
 * Checks an authored trigger against the catalog, including the cron
 * expression of a schedule. Throws InvalidFilterError (or InvalidPatternError).
 */
export function validateTrigger(t: Trigger): void {
  if (!t.provider) throw new InvalidFilterError("provider is required");
  if (!CATALOG[t.provider]) {
    const c = canonicalProvider(t.provider);
    if (c !== t.provider && CATALOG[c]) throw new InvalidFilterError(`unknown provider "${t.provider}"; use "${c}"`);
    throw new InvalidFilterError(`unknown provider "${t.provider}"`);
  }
  if (!t.event) throw new InvalidFilterError("event is required");
  if (t.event.includes(".")) throw new InvalidFilterError(`event "${t.event}" must not be dotted; set provider and event separately`);
  if (t.provider === T.ProviderSchedule) {
    if (!t.schedule) throw new InvalidFilterError(`schedule is required for schedule.${t.event}`);
    const err = validateCron(t.schedule.cron);
    if (err) throw new InvalidFilterError(`schedule.cron: ${err}`);
  } else if (t.schedule) {
    throw new InvalidFilterError("schedule is only valid for schedule triggers");
  }
  parseFilter(t.provider, t.event, t.filter, "author");
}

/** Validates the triggers of one automation and the unique schedule names. */
export function validateTriggers(ts: readonly Trigger[] | null | undefined): void {
  if (!ts || ts.length === 0) throw new InvalidFilterError("at least one trigger is required");
  const names = new Set<string>();
  for (const [i, t] of ts.entries()) {
    try {
      validateTrigger(t);
    } catch (e) {
      const err = e as Error;
      err.message = `triggers[${i}]: ${err.message}`;
      throw err;
    }
    if (!t.schedule) continue;
    const n = t.schedule.name ?? "";
    if (names.has(n)) {
      if (n === "") throw new InvalidFilterError(`triggers[${i}]: at most one schedule can omit its name`);
      throw new InvalidFilterError(`triggers[${i}]: duplicate schedule name "${n}"`);
    }
    names.add(n);
  }
}
