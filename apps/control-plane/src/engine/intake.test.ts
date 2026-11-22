// Ported from internal/engine engine_test.go TestParseDecision, intake_test.go
// TestActionKey, and the continuation and coverage rules of events.go.
import { describe, expect, it } from "vitest";
import type { CoreEvent } from "../contracts/do-messages";
import {
  actionKey,
  autoFollowUp,
  continuationKind,
  factoryCovers,
  formatFollowUp,
  isMention,
  isMergeEvent,
  linkRequiredFor,
  parseDecision,
  trustOfMessage,
} from "./intake";

const ev = (p: Partial<CoreEvent>): CoreEvent => ({
  id: "e1",
  team_id: "default",
  provider: "github",
  type: "issue_commented",
  actor: { provider: "github", external_id: "1", handle: "alice", is_bot: false },
  source: {},
  attributes: {},
  received_at: "2026-01-01T00:00:00Z",
  ...p,
});

describe("parseDecision", () => {
  const cases: [string, string | undefined][] = [
    ["approve", "approved"],
    ["@payments approve, looks good", "approved"],
    ["<@U123> LGTM!", "approved"],
    ["payments ship it", "approved"],
    ["Approved.", "approved"],
    ["yes", "approved"],
    ["reject", "rejected"],
    ["Request changes: make the field required", "rejected"],
    ["no", "rejected"],
    ["no idea, what do you think about X?", undefined],
    ["yes but also please make it required", undefined],
    ["approvers should be admins", undefined],
    ["", undefined],
    ["👍", "approved"],
  ];
  it.each(cases)("%j", (text, want) => {
    expect(parseDecision(text, "payments")).toBe(want);
  });
});

describe("actionKey", () => {
  it.each([
    [{ action_key: "x" }, "x"],
    [{ provider: "github", type: "issue_commented", delivery_id: "d1:issue_commented" }, "github:d1"],
    [{ provider: "linear", type: "comment_created", delivery_id: "abc" }, "linear:abc"],
    [{ provider: "slack", type: "app_mention", delivery_id: "Ev1", team_id: "T", payload: { channel: "C", ts: "1.2" } }, "slack:T:C:1.2"],
    [{ provider: "slack", type: "reaction_added", delivery_id: "Ev2", payload: '{"reaction":"eyes"}' }, "slack:Ev2"],
    [{ provider: "github", type: "x" }, ""],
  ] as [Partial<CoreEvent>, string][])("%j", (p, want) => {
    expect(actionKey(ev({ action_key: "", ...p, delivery_id: p.delivery_id ?? "" }))).toBe(want);
  });
});

describe("continuation and mentions", () => {
  it("classifies continuations", () => {
    expect(continuationKind(ev({ type: "issue_commented", text: "hi" }))).toBe("user");
    expect(continuationKind(ev({ type: "issue_commented", text: "" }))).toBe("");
    expect(continuationKind(ev({ type: "pull_request_merged" }))).toBe("event");
    expect(continuationKind(ev({ type: "pull_request_review_submitted", attributes: { review_state: "commented" } }))).toBe("");
    expect(continuationKind(ev({ type: "pull_request_review_submitted", attributes: { review_state: "approved" } }))).toBe("event");
    expect(continuationKind(ev({ type: "issue_opened", text: "body" }))).toBe("");
    expect(isMergeEvent(ev({ type: "pull_request", attributes: { merged: true } }))).toBe(true);
    expect(isMention(ev({ type: "app_mention" }))).toBe(true);
    expect(autoFollowUp(ev({ attributes: { auto_follow_up: "false" } }))).toBe(false);
    expect(linkRequiredFor("webhook")).toBe(false);
    expect(linkRequiredFor("slack")).toBe(true);
  });

  it("covers factories by repository or integration", () => {
    const e = ev({ source: { repo: "acme/api" } });
    expect(factoryCovers(e, [{ codeForge: "GITHUB", owner: "acme", name: "api" }], [])).toBe(true);
    expect(factoryCovers(e, [{ codeForge: "GITHUB", owner: "acme", name: "web" }], [])).toBe(false);
    expect(factoryCovers(e, [], [])).toBe(true);
    expect(factoryCovers(ev({ provider: "teams" }), [], ["microsoft_teams"])).toBe(true);
    expect(factoryCovers(ev({ provider: "slack" }), [], ["linear"])).toBe(false);
    expect(factoryCovers(ev({ provider: "azure_devops", source: { kind: "work_item", repo: "Proj" } }), [{ codeForge: "AZURE_DEVOPS", owner: "org/proj", name: "r" }], [])).toBe(true);
  });

  it("trust and follow-up text", () => {
    const member = ev({ actor: { provider: "github", external_id: "1", handle: "alice", is_bot: false, trust: "member", user_id: "usr_a" }, text: "please fix" });
    expect(trustOfMessage(member, "usr_b", { restrict_followups_to_starter: true }, false).trusted).toBe(false);
    const mt = trustOfMessage(member, "usr_a", { approvers: ["x@y"] }, false);
    expect(mt).toEqual({ trusted: true, canAnswer: true, canApprove: false, reason: "linked team member, not in approvers" });
    const txt = formatFollowUp(member, mt);
    expect(txt).toContain("[follow-up] New github issue commented from alice on this work item.");
    expect(txt).toContain("- Sender: linked team member");
    expect(txt).toContain("<<<\nplease fix\n>>>");
    expect(formatFollowUp(ev({ text: "x" }), { trusted: false, reason: "" })).toContain("not a trusted team member (no linked account)");
  });
});
