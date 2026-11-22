// Ports internal/triggers match_test.go, payload_test.go and cron/glob checks.
import { describe, expect, it } from "vitest";
import type { Event, Trigger } from "@openfactory/core";
import * as T from "./attributes";
import { matchTrigger, match, validateTrigger, validateTriggers } from "./match";
import { matchPayload, validatePayloadPattern } from "./payload";
import { parseCron, validateCron } from "./cron";
import { matchGlob, validateGlob, pathMatch } from "./glob";
import { events } from "./catalog";
import { scheduleEventId, pendingSlot } from "./schedules";

type Attrs = Record<string, unknown>;
type Ev = Pick<Event, "provider" | "type" | "actor" | "attributes" | "text" | "payload">;
const mkEvent = (provider: string, type: string, a?: Attrs): Ev => ({
  provider,
  type,
  actor: { provider, external_id: "", handle: "", is_bot: false },
  attributes: a ?? null,
});
const withText = (ev: Ev, s: string): Ev => ({ ...ev, text: s });
const prAttrs = (extra?: Attrs): Attrs => ({
  [T.AttrRepo]: "acme/api",
  [T.AttrBaseBranch]: "main",
  [T.AttrPRNumber]: 42,
  [T.AttrLabels]: ["bug", "Factory-Ready"],
  [T.AttrAuthor]: "alex",
  [T.AttrAssignees]: ["sam"],
  [T.AttrPaths]: ["src/app/main.go", "docs/readme.md"],
  ...extra,
});
const pr = (extra?: Attrs) => mkEvent("github", "pull_request_opened", prAttrs(extra));
const bot = { ...pr(), actor: { provider: "github", external_id: "", handle: "", is_bot: true } };
const self = pr({ [T.AttrActorIsSelf]: true });
const labeled = (added: string[], current: string[]) =>
  mkEvent("github", "issue_labeled", { [T.AttrRepo]: "acme/api", [T.AttrAddedLabels]: added, [T.AttrLabels]: current });
const base = { [T.AttrChannelID]: "C0123456789", [T.AttrChannelName]: "incidents", [T.AttrUserID]: "U1", [T.AttrUserName]: "alex", [T.AttrUserEmail]: "alex@example.com" };
const reaction = { [T.AttrChannelID]: "C1", [T.AttrChannelName]: "engineering", [T.AttrUserID]: "U2", [T.AttrEmoji]: ":eyes::skin-tone-3:", [T.AttrItemUserID]: "U9", [T.AttrItemUserName]: "pat" };
const msg = (typ: string, a: Attrs) => mkEvent("slack", typ, a);
const ev = mkEvent("microsoft_teams", "app_mention", { [T.AttrTeamID]: "11111111-1111-4111-8111-111111111111", [T.AttrChannelID]: "19:abc@thread.tacv2", [T.AttrUserID]: "u-1" });

const CASES: [string, string, string, string | undefined, () => Ev, boolean, boolean][] = [
  ["no filter", "github", "pull_request_opened", undefined, () => pr(undefined), true, false],
  ["wrong provider", "gitlab", "push", undefined, () => pr(undefined), false, false],
  ["wrong event", "github", "pull_request_closed", undefined, () => pr(undefined), false, false],
  ["repo match case-insensitive", "github", "pull_request_opened", '{"repos":["ACME/api"]}', () => pr(undefined), true, false],
  ["repo mismatch", "github", "pull_request_opened", '{"repos":["acme/web"]}', () => pr(undefined), false, false],
  ["AND of keys", "github", "pull_request_opened", '{"repos":["acme/api"],"base_branches":["develop"]}', () => pr(undefined), false, false],
  ["any-of list", "github", "pull_request_opened", '{"base_branches":["develop","main"]}', () => pr(undefined), true, false],
  ["base branch refs/heads normalized", "github", "pull_request_opened", '{"base_branches":["refs/heads/main"]}', () => pr(undefined), true, false],
  ["labels in matcher", "github", "pull_request_opened", '{"labels":{"in":["factory-ready"]}}', () => pr(undefined), true, false],
  ["labels not_in excludes", "github", "pull_request_opened", '{"labels":{"not_in":["bug"]}}', () => pr(undefined), false, false],
  ["labels not_in passes", "github", "pull_request_opened", '{"labels":{"not_in":["wip"]}}', () => pr(undefined), true, false],
  ["labels in + not_in", "github", "pull_request_opened", '{"labels":{"in":["bug"],"not_in":["wip"]}}', () => pr(undefined), true, false],
  ["empty list no restriction", "github", "pull_request_opened", '{"labels":[],"repos":null,"authors":{}}', () => pr(undefined), true, false],
  ["pr_numbers int", "github", "pull_request_opened", '{"pr_numbers":[42]}', () => pr(undefined), true, false],
  ["pr_numbers float attr", "github", "pull_request_opened", '{"pr_numbers":[42]}', () => pr({[T.AttrPRNumber]: 42}), true, false],
  ["pr_numbers mismatch", "github", "pull_request_opened", '{"pr_numbers":[7]}', () => pr(undefined), false, false],
  ["alias prNumbers", "github", "pull_request_opened", '{"prNumbers":{"not_in":[42]}}', () => pr(undefined), false, false],
  ["alias baseBranches", "github", "pull_request_opened", '{"baseBranches":["main"]}', () => pr(undefined), true, false],
  ["alias and native together", "github", "pull_request_opened", '{"baseBranches":["main"],"base_branches":["main"]}', () => pr(undefined), false, true],
  ["paths glob **", "github", "pull_request_opened", '{"paths":["src/**"]}', () => pr(undefined), true, false],
  ["paths glob no match", "github", "pull_request_opened", '{"paths":["lib/**"]}', () => pr(undefined), false, false],
  ["paths glob star segment", "github", "pull_request_opened", '{"paths":["docs/*.md"]}', () => pr(undefined), true, false],
  ["paths not_in excludes any", "github", "pull_request_opened", '{"paths":{"not_in":["docs/**"]}}', () => pr(undefined), false, false],
  ["authors login fold", "github", "pull_request_opened", '{"authors":["Alex"]}', () => pr(undefined), true, false],
  ["assignees []any", "github", "pull_request_opened", '{"assignees":["sam"]}', () => pr(undefined), true, false],
  ["keywords substring CI", "github", "pull_request_opened", '{"keywords":["RETEST"]}', () => withText(pr(undefined), "please retest this"), true, false],
  ["keywords no text", "github", "pull_request_opened", '{"keywords":["retest"]}', () => pr(undefined), false, false],
  ["keywords text attr fallback", "github", "pull_request_opened", '{"keywords":["retest"]}', () => pr({[T.AttrText]: "Retest pls"}), true, false],
  ["keywords not_in", "github", "pull_request_opened", '{"keywords":{"not_in":["skip"]}}', () => withText(pr(undefined), "[SKIP] ci"), false, false],
  ["missing attr fails in", "github", "pull_request_opened", '{"base_branches":["main"]}', () => mkEvent("github", "pull_request_opened", {}), false, false],
  ["missing attr passes not_in", "github", "pull_request_opened", '{"base_branches":{"not_in":["main"]}}', () => mkEvent("github", "pull_request_opened", undefined), true, false],
  ["skip_bots default all skips bot", "github", "pull_request_opened", undefined, () => bot, false, false],
  ["skip_bots default all skips attr bot", "github", "pull_request_opened", undefined, () => pr({[T.AttrActorIsBot]: true}), false, false],
  ["skip_bots default skips self", "github", "pull_request_opened", undefined, () => self, false, false],
  ["skip_bots self admits third-party bot", "github", "pull_request_opened", '{"skip_bots":"self"}', () => bot, true, false],
  ["skip_bots self skips self", "github", "pull_request_opened", '{"skip_bots":"self"}', () => self, false, false],
  ["skip_bots none admits self", "github", "pull_request_opened", '{"skip_bots":"none"}', () => self, true, false],
  ["skip_bots invalid", "github", "pull_request_opened", '{"skip_bots":"some"}', () => pr(undefined), false, true],
  ["skip_bots list invalid", "github", "pull_request_opened", '{"skip_bots":["all"]}', () => pr(undefined), false, true],
  ["unknown key", "github", "pull_request_opened", '{"color":["red"]}', () => pr(undefined), false, true],
  ["key not for event", "github", "push", '{"labels":["x"]}', () => mkEvent("github", "push", undefined), false, true],
  ["unknown operator", "github", "pull_request_opened", '{"labels":{"any":["x"]}}', () => pr(undefined), false, true],
  ["scalar value rejected", "github", "pull_request_opened", '{"labels":"bug"}', () => pr(undefined), false, true],
  ["in and not_in conflict", "github", "pull_request_opened", '{"labels":{"in":["bug"],"not_in":["BUG"]}}', () => pr(undefined), false, true],
  ["pr_numbers string rejected", "github", "pull_request_opened", '{"pr_numbers":["42"]}', () => pr(undefined), false, true],
  ["filter not object", "github", "pull_request_opened", '[1]', () => pr(undefined), false, true],
  ["unsupported attr type fails closed", "github", "pull_request_opened", '{"repos":["acme/api"]}', () => pr({[T.AttrRepo]: {x: 1}}), false, true],
  ["labeled: in checks added labels", "github", "issue_labeled", '{"labels":["factory-ready"]}', () => labeled(["factory-ready"], ["bug", "factory-ready"]), true, false],
  ["labeled: pre-existing label not added", "github", "issue_labeled", '{"labels":["factory-ready"]}', () => labeled(["bug"], ["bug", "factory-ready"]), false, false],
  ["labeled: not_in checks current", "github", "issue_labeled", '{"labels":{"in":["factory-ready"],"not_in":["blocked"]}}', () => labeled(["factory-ready"], ["blocked", "factory-ready"]), false, false],
  ["commit_status success", "github", "commit_status", '{"states":["FAILURE","success"]}', () => mkEvent("github", "commit_status", {[T.AttrCommitState]: "success", [T.AttrContext]: "ci/build"}), true, false],
  ["commit_status pending never", "github", "commit_status", undefined, () => mkEvent("github", "commit_status", {[T.AttrCommitState]: "pending"}), false, false],
  ["commit_status contexts fold", "github", "commit_status", '{"contexts":["CI/Build"]}', () => mkEvent("github", "commit_status", {[T.AttrCommitState]: "failure", [T.AttrContext]: "ci/build"}), true, false],
  ["commit_status invalid state value", "github", "commit_status", '{"states":["pending"]}', () => mkEvent("github", "commit_status", {[T.AttrCommitState]: "failure"}), false, true],
  ["channel id", "slack", "message_posted", '{"channel_ids":["C0123456789"]}', () => msg("message_posted", base), true, false],
  ["channel id not_in", "slack", "message_posted", '{"channel_ids":{"not_in":["C0123456789"]}}', () => msg("message_posted", base), false, false],
  ["channels alias with #", "slack", "message_posted", '{"channels":[" #incidents "]}', () => msg("message_posted", base), true, false],
  ["channels alias matcher object rejected", "slack", "message_posted", '{"channels":{"not_in":["incidents"]}}', () => msg("message_posted", base), false, true],
  ["channels with channel_ids rejected", "slack", "message_posted", '{"channels":["a"],"channel_ids":["C1"]}', () => msg("message_posted", base), false, true],
  ["users alias by email", "slack", "message_posted", '{"users":["Alex@Example.com"]}', () => msg("message_posted", base), true, false],
  ["users alias by name", "slack", "message_posted", '{"users":["alex"]}', () => msg("message_posted", base), true, false],
  ["keywords", "slack", "message_posted", '{"keywords":["outage"]}', () => withText(msg("message_posted", base), "Major OUTAGE in eu"), true, false],
  ["message_dm covers im", "slack", "message_dm", undefined, () => msg("message_im", base), true, false],
  ["message_dm covers mpim", "slack", "message_dm", undefined, () => msg("message_mpim", base), true, false],
  ["message_im not mpim", "slack", "message_im", undefined, () => msg("message_mpim", base), false, false],
  ["self post ignored", "slack", "app_mention", undefined, () => msg("app_mention", {[T.AttrActorIsSelf]: true}), false, false],
  ["emoji normalized", "slack", "reaction_added", '{"emojis":[":eyes:"]}', () => msg("reaction_added", reaction), true, false],
  ["emoji mismatch", "slack", "reaction_added", '{"emojis":["ticket"]}', () => msg("reaction_added", reaction), false, false],
  ["item users alias", "slack", "reaction_added", '{"itemUsers":["pat"]}', () => msg("reaction_added", reaction), true, false],
  ["item_user_ids", "slack", "reaction_added", '{"item_user_ids":["U9"],"user_ids":["U2"]}', () => msg("reaction_added", reaction), true, false],
  ["itemUsers only on reactions", "slack", "message_posted", '{"itemUsers":["pat"]}', () => msg("message_posted", base), false, true],
  ["emojis not on member_joined", "slack", "member_joined_channel", '{"emojis":["x"]}', () => msg("member_joined_channel", base), false, true],];

describe("matchTrigger (match_test.go)", () => {
  for (const [name, provider, event, filter, mk, want, wantErr] of CASES) {
    it(name, () => {
      const t: Trigger = { provider, event, ...(filter !== undefined ? { filter } : {}) };
      let got = false;
      let err: unknown;
      try {
        got = matchTrigger(mk(), t);
      } catch (e) {
        err = e;
      }
      expect(err !== undefined, String(err)).toBe(wantErr);
      expect(got).toBe(want);
    });
  }

  it("filter as a decoded object works too", () => {
    expect(matchTrigger(pr(), { provider: "github", event: "pull_request_opened", filter: { repos: ["ACME/api"] } })).toBe(true);
  });

  it("schedule triggers bind to their automation", () => {
    const a = {
      id: "aut_1",
      factory_id: "f1",
      name: "nightly",
      enabled: true,
      agent: "x",
      prompt: "",
      triggers: [{ provider: "schedule", event: "cron_fired", schedule: { name: "", cron: "@daily" } }],
    };
    const e = mkEvent("schedule", "cron_fired", { [T.AttrAutomationID]: "aut_1", [T.AttrScheduleName]: "" });
    expect(match(e, a)).toBe(true);
    expect(match(mkEvent("schedule", "cron_fired", { [T.AttrAutomationID]: "aut_2" }), a)).toBe(false);
    expect(match(e, { ...a, enabled: false })).toBe(false);
  });

  it("webhook payload filter compares big numbers exactly with a raw body", () => {
    const t: Trigger = { provider: "webhook", event: "received", filter: '{"webhook_ids":["whk_1"],"payload":{"id":[12345678901234567890]}}' };
    const e = { ...mkEvent("webhook", "received", { [T.AttrWebhookID]: "whk_1" }), payload: '{"id":12345678901234567890}' };
    expect(matchTrigger(e, t)).toBe(true);
    expect(matchTrigger({ ...e, payload: '{"id":12345678901234567891}' }, t)).toBe(false);
  });
});

describe("validateTrigger", () => {
  it("rejects authoring errors", () => {
    expect(() => validateTrigger({ provider: "microsoft_teams", event: "app_mention" })).toThrow(/use "teams"/);
    expect(() => validateTrigger({ provider: "github", event: "github.push" })).toThrow(/dotted/);
    expect(() => validateTrigger({ provider: "schedule", event: "cron_fired" })).toThrow(/schedule is required/);
    expect(() => validateTrigger({ provider: "schedule", event: "cron_fired", schedule: { cron: "* * *" } })).toThrow(/expected 5 fields/);
    expect(() => validateTrigger({ provider: "schedule", event: "cron_fired", schedule: { cron: "@every 30s" } })).toThrow(/at least 1m0s/);
    expect(() => validateTrigger({ provider: "gitlab", event: "bot_mentioned", filter: { mentioned: ["x"] } })).toThrow(/server-managed/);
    expect(() => validateTrigger({ provider: "azure_devops", event: "push" })).toThrow(/not available for authoring/);
    expect(() => validateTrigger({ provider: "schedule", event: "cron_fired", schedule: { cron: "@daily" }, filter: { schedule_ids: ["x"] } })).toThrow(/no filter/);
    expect(() => validateTrigger({ provider: "github", event: "push", schedule: { cron: "@daily" } })).toThrow(/only valid for schedule/);
    validateTrigger({ provider: "github", event: "push", filter: { paths: ["src/**"] } });
  });
  it("checks schedule names", () => {
    const s = (name: string) => ({ provider: "schedule", event: "cron_fired", schedule: { name, cron: "@daily" } });
    expect(() => validateTriggers([s(""), s("")])).toThrow(/at most one schedule can omit/);
    expect(() => validateTriggers([s("a"), s("a")])).toThrow(/duplicate schedule name "a"/);
    expect(() => validateTriggers([])).toThrow(/at least one trigger/);
    validateTriggers([s(""), s("a")]);
  });
  it("lists events without server-managed keys", () => {
    const gl = events("gitlab")!;
    expect(gl.find((e) => e.event === "bot_mentioned")!.filter_fields).toEqual(["repos"]);
    expect(events("msteams")!.length).toBe(2);
    expect(events("nope")).toBeNull();
  });
});

const BODY = `{
  "event": "deploy.finished", "env": "production", "service": "payments",
  "count": 1, "ratio": 0.5, "big": 12345678901234567890, "ok": true, "nothing": null,
  "tags": ["a", "b"], "nums": [1, 2, 3],
  "pull_request": {"draft": false, "user": {"login": "alex", "type": "User"}},
  "commits": [{"id": "c1", "author": {"name": "x"}}, {"id": "c2", "author": {"name": "y"}}],
  "labels": [{"name": "bug"}, {"name": "ui"}],
  "Case": "Value"
}`;

const PAYLOAD: [string, string, boolean][] = [
  ["null pattern", `null`, true],
  ["empty pattern", `{}`, true],
  ["string in", `{"event": ["deploy.finished"]}`, true],
  ["string in any-of", `{"env": ["staging", "production"]}`, true],
  ["string case-sensitive", `{"Case": ["value"]}`, false],
  ["not_in fails", `{"env": {"not_in": ["production"]}}`, false],
  ["missing key fails in", `{"region": ["eu"]}`, false],
  ["missing key passes not_in", `{"region": {"not_in": ["dev"]}}`, true],
  ["exists true missing", `{"region": {"exists": true}}`, false],
  ["exists false missing", `{"region": {"exists": false}}`, true],
  ["exists false present", `{"service": {"exists": false}}`, false],
  ["null value present for exists", `{"nothing": {"exists": true}}`, true],
  ["null matchable", `{"nothing": [null]}`, true],
  ["null not equal false", `{"nothing": [false]}`, false],
  ["missing not equal null", `{"absent": [null]}`, false],
  ["numbers numerically 1 == 1.0", `{"count": [1.0]}`, true],
  ["numbers 1e0", `{"count": [1e0]}`, true],
  ["number 0.5 == 5e-1", `{"ratio": [5e-1]}`, true],
  ["number vs string", `{"count": ["1"]}`, false],
  ["big integer exact", `{"big": [12345678901234567890]}`, true],
  ["big integer differs", `{"big": [12345678901234567891]}`, false],
  ["bool vs string", `{"ok": ["true"]}`, false],
  ["combined fails one", `{"env": {"in": ["production"], "not_in": []}, "event": {"not_in": ["deploy.finished"]}}`, false],
  ["nested object", `{"pull_request": {"draft": [false], "user": {"login": ["alex"]}}}`, true],
  ["nested requires presence", `{"repository": {"name": {"not_in": ["x"]}}}`, false],
  ["nested empty object requires presence", `{"pull_request": {}}`, true],
  ["nested empty object missing", `{"repository": {}}`, false],
  ["nested on scalar fails", `{"env": {"name": ["x"]}}`, false],
  ["array of objects deep", `{"commits": {"author": {"name": ["y"]}}}`, true],
  ["array of objects same element", `{"commits": {"id": ["c1"], "author": {"name": ["y"]}}}`, false],
  ["array of scalars overlap", `{"tags": ["b", "z"]}`, true],
  ["array of scalars not_in overlap", `{"tags": {"not_in": ["a"]}}`, false],
  ["array of numbers", `{"nums": [3.0]}`, true],
  ["scalar leaf on object value", `{"pull_request": ["x"]}`, false],
  ["empty in no restriction", `{"env": []}`, true],
  ["empty in on missing", `{"absent": {"in": []}}`, true],
  ["exists false with empty in", `{"absent": {"exists": false, "in": []}}`, true],
];

describe("matchPayload (payload_test.go)", () => {
  for (const [name, pattern, want] of PAYLOAD) it(name, () => expect(matchPayload(pattern, BODY)).toBe(want));
  it("non-object bodies", () => {
    expect(matchPayload(`{"a": [1]}`, `[{"a": 2}, {"a": 1}]`)).toBe(true);
    expect(matchPayload(`{"a": [1]}`, `"scalar"`)).toBe(false);
    expect(matchPayload(`{}`, `42`)).toBe(true);
    expect(matchPayload(`{"a": {"not_in": [1]}}`, `null`)).toBe(false);
  });
  it("fails closed", () => {
    expect(() => matchPayload(`{"a": ["x"]}`, `{not json`)).toThrow();
    expect(() => matchPayload(`{"a": "x"}`, `{"a": "x"}`)).toThrow(/invalid payload pattern/);
  });
  it("validates limits", () => {
    let deep = `{"leaf": [1]}`;
    for (let i = 1; i < 11; i++) deep = `{"n${i}": ${deep}}`;
    expect(() => validatePayloadPattern(deep)).toThrow(/deeper than 10/);
    const many = "{" + Array.from({ length: 65 }, (_, i) => `"k${i}": [1]`).join(",") + "}";
    expect(() => validatePayloadPattern(many)).toThrow(/more than 64 leaf/);
    expect(() => validatePayloadPattern(`{"a": {"in": [1], "not_in": [1.0]}}`)).toThrow(/both in and not_in/);
    expect(() => validatePayloadPattern(`{"a": {"exists": false, "in": [1]}}`)).toThrow(/exists: false/);
    expect(() => validatePayloadPattern(`{"a": {"in": [1], "b": [2]}}`)).toThrow(/mixes operators/);
    expect(() => validatePayloadPattern(`{"a": [[1]]}`)).toThrow(/got a list/);
  });
});

describe("cron (robfig semantics, UTC)", () => {
  const at = (s: string) => Date.parse(s);
  const iso = (ms: number) => new Date(ms).toISOString();
  it("next slots", () => {
    expect(iso(parseCron("*/15 * * * *").next(at("2026-01-01T10:07:30Z")))).toBe("2026-01-01T10:15:00.000Z");
    expect(iso(parseCron("0 9 * * 1-5").next(at("2026-01-02T09:00:00Z")))).toBe("2026-01-05T09:00:00.000Z");
    expect(iso(parseCron("@daily").next(at("2026-02-28T12:00:00Z")))).toBe("2026-03-01T00:00:00.000Z");
    expect(iso(parseCron("@hourly").next(at("2026-12-31T23:59:59Z")))).toBe("2027-01-01T00:00:00.000Z");
    expect(iso(parseCron("@yearly").next(at("2026-06-01T00:00:00Z")))).toBe("2027-01-01T00:00:00.000Z");
    expect(iso(parseCron("@weekly").next(at("2026-01-01T00:00:00Z")))).toBe("2026-01-04T00:00:00.000Z");
    expect(iso(parseCron("@every 1h30m").next(at("2026-01-01T00:00:00.500Z")))).toBe("2026-01-01T01:30:00.000Z");
    // DOM/DOW OR rule when both are restricted.
    expect(iso(parseCron("0 0 13 * 5").next(at("2026-03-01T00:00:00Z")))).toBe("2026-03-06T00:00:00.000Z");
    expect(iso(parseCron("0 0 31 2 *").next(at("2026-01-01T00:00:00Z")))).toBe("1970-01-01T00:00:00.000Z");
    expect(iso(parseCron("0 0 1 jan,jul *").next(at("2026-02-01T00:00:00Z")))).toBe("2026-07-01T00:00:00.000Z");
  });
  it("errors", () => {
    expect(validateCron("")).toBe("cron expression is required");
    expect(validateCron("TZ=UTC 0 * * * *")).toMatch(/time zones/);
    expect(validateCron("61 * * * *")).toMatch(/end of range \(61\) above maximum \(59\)/);
    expect(validateCron("@weird")).toMatch(/unrecognized descriptor/);
    expect(validateCron("@every 1m")).toBeNull();
    expect(validateCron("0 0 * * MON")).toBeNull();
  });
  it("pending slot follows the clock", () => {
    const s = parseCron("* * * * *");
    const now = at("2026-01-01T10:10:30Z");
    expect(iso(pendingSlot(s, at("2026-01-01T10:00:00Z"), now, 5 * 60_000))).toBe("2026-01-01T10:10:00.000Z");
  });
  it("schedule event IDs are deterministic", async () => {
    const a = await scheduleEventId("aut_1:", at("2026-01-01T00:00:00Z"));
    expect(a).toMatch(/^evt_sched_[0-9a-f]{24}$/);
    expect(await scheduleEventId("aut_1:", at("2026-01-01T00:00:00.999Z"))).toBe(a);
  });
});

describe("glob", () => {
  it("matches like path.Match with **", () => {
    expect(matchGlob("src/**", "src/a/b.go")).toBe(true);
    expect(matchGlob("**/*.go", "main.go")).toBe(true);
    expect(matchGlob("docs/*.md", "docs/x/y.md")).toBe(false);
    expect(matchGlob("a/**/b", "a/b")).toBe(true);
    expect(matchGlob("[a-c]?.txt", "bz.txt")).toBe(true);
    expect(pathMatch("\\*", "*")).toBe(true);
    expect(validateGlob("a**")).toMatch(/whole path segment/);
    expect(validateGlob("[")).toMatch(/malformed/);
    expect(validateGlob("")).toBe("empty glob");
  });
});
