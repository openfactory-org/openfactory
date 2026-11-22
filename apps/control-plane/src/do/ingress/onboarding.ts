// GitHub onboarding notices: port of internal/engine/onboarding.go. A GitHub
// mention from an account that is not linked to a team member starts no
// work. When the factory has automations.githubOnboardingNotices on (the
// default), the account gets one public comment that says how to link it.
import type { CoreEvent } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { normalizeProvider } from "../../engine/intake";
import type { ForgeResolver } from "../../forges/forge";
import { splitRepo } from "@openfactory/core";

export type OnboardingReason = "unlinked" | "non_member";

/** The candidate factory as intake sees it. */
export interface OnboardingFactory {
  factory: { id: string };
  runtime: { github_onboarding_notices?: boolean };
}

export interface OnboardingDeps {
  user(id: string): Promise<{ id: string; team_id?: string } | undefined>;
  /** Posts the comment once per (team, account, reason). */
  onboardingNotice?(ev: CoreEvent, reason: OnboardingReason, factoryId: string): Promise<void>;
}

/**
 * githubActorEligible: an event from a human GitHub account starts work only
 * when the account is linked to a user of the team. Bot events and events
 * without an external actor are not gated.
 */
export async function githubActorEligible(
  d: OnboardingDeps,
  teamId: string,
  ev: CoreEvent,
  factories: OnboardingFactory[],
): Promise<boolean> {
  if (normalizeProvider(ev.provider) !== "github" || ev.actor.is_bot || !ev.actor.external_id || factories.length === 0)
    return true;
  let reason: OnboardingReason | "" = "";
  if (!ev.actor.user_id) reason = "unlinked";
  else {
    let u: { id: string; team_id?: string } | undefined;
    try {
      u = await d.user(ev.actor.user_id);
    } catch (e) {
      // Fail open on store errors rather than drop work.
      console.warn("intake: look up linked user", ev.actor.user_id, e instanceof Error ? e.message : String(e));
      return true;
    }
    if (!u) reason = "unlinked";
    else if ((u.team_id ?? teamId) !== (ev.team_id || teamId)) reason = "non_member";
  }
  if (!reason) return true;
  console.info("intake: github event from an ineligible account ignored", reason, ev.actor.handle, ev.type);
  const f = factories.find((x) => x.runtime.github_onboarding_notices !== false);
  if (f && d.onboardingNotice) {
    try {
      await d.onboardingNotice(ev, reason, f.factory.id);
    } catch (e) {
      console.warn("intake: onboarding notice", e instanceof Error ? e.message : String(e));
    }
  }
  return false;
}

/** onboardingHow: how an unlinked person links the GitHub account. */
export function onboardingHow(publicURL: string): string {
  const where = publicURL ? publicURL.replace(/\/+$/, "") + "/integrations" : "the Open Factory dashboard";
  return `Sign in to ${where} and link your GitHub account under "Link your account", then try again.`;
}

/** The comment text. It is public, so it holds no link URL. */
export function onboardingComment(handle: string, reason: OnboardingReason, publicURL: string): string {
  if (reason === "unlinked")
    return `Hi @${handle}, this repository is connected to an Open Factory factory, but your GitHub account is not linked to an Open Factory account, so this request did not start any work.\n\n${onboardingHow(publicURL)}\n\nAfter linking, mention the factory or apply the label again.`;
  return `Hi @${handle}, this repository is connected to an Open Factory factory, but your linked Open Factory account is not an active member of the factory's team, so this request did not start any work.\n\nAsk a team admin to add you to the team, then mention the factory or apply the label again.`;
}

/** The R2 key that records a posted notice. */
export const onboardingKey = (teamId: string, externalId: string, reason: OnboardingReason): string =>
  `intake/github-onboarding/${teamId}/${encodeURIComponent(externalId.toLowerCase())}/${reason}`;

/**
 * postOnboardingNotice: comments once per (team, GitHub account, reason). The
 * record lives in R2; intake runs one event at a time per partition.
 */
export async function postOnboardingNotice(
  env: Pick<Env, "DATA" | "OF_PUBLIC_URL">,
  forges: ForgeResolver,
  teamId: string,
  ev: CoreEvent,
  reason: OnboardingReason,
  factoryId: string,
): Promise<boolean> {
  const src = ev.source;
  if (!src.repo || !src.number) return false;
  const key = onboardingKey(teamId, ev.actor.external_id, reason);
  if (await env.DATA.head(key)) return false;
  const body = onboardingComment(ev.actor.handle || ev.actor.external_id, reason, env.OF_PUBLIC_URL ?? "");
  const f = await forges.forge({ teamId, factoryId, codeForge: "GITHUB", repo: src.repo });
  const [owner, name] = splitRepo(src.repo);
  await f.comment(owner, name, src.number, body);
  await env.DATA.put(key, new Date().toISOString());
  return true;
}
