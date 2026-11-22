import { describe, expect, it } from "vitest";
import {
  EVENT_ATTR,
  EVENT_PROVIDER,
  clipBytes,
  cutBytes,
  difference,
  extractMentions,
  formatOutbound,
  hasMention,
  oneLine,
  splitRepo,
  stringSet,
  truncate,
} from "../src/index";

describe("extractMentions", () => {
  it.each([
    ["simple", "hey @alice can you look", ["alice"]],
    ["team and bot", "cc @acme/backend and @openfactory[bot].", ["acme/backend", "openfactory[bot]"]],
    ["code ignored", "```\n@ignored\n```\nand `@inline` but @real", ["real"]],
    ["quote ignored", "> @quoted said\n@me", ["me"]],
    ["email is not a mention", "mail bob@example.com", []],
    ["dedupe ignores case", "@a @A @b", ["a", "b"]],
    ["dash before is not a mention", "x-@y and @z", ["z"]],
  ])("%s", (_n, text, want) => {
    expect(extractMentions(text)).toEqual(want);
  });

  it("matches a handle with the bot suffix", () => {
    expect(hasMention("ping @OpenFactory[bot]", "openfactory")).toBe(true);
    expect(hasMention("ping @other", "@openfactory")).toBe(false);
    expect(hasMention("x", "")).toBe(false);
  });
});

describe("text helpers", () => {
  it.each([
    ["acme/platform/api", ["acme/platform", "api"]],
    ["group/sub/proj", ["group/sub", "proj"]],
    ["api", ["", "api"]],
    ["/acme/api/", ["acme", "api"]],
    ["", ["", ""]],
  ])("splits %j at the last slash", (full, want) => {
    expect(splitRepo(full)).toEqual(want);
  });

  it.each([
    ["abcdef", 4, "abc…"],
    ["abc", 4, "abc"],
    ["héllo", 3, "hé…"],
    ["😀😀😀", 2, "😀…"],
    ["abc", 1, "a"],
    ["abc", 0, ""],
    ["abc", -1, ""],
  ])("truncates %j to %d code points", (s, n, want) => {
    expect(truncate(s, n)).toBe(want);
  });

  it("cuts bytes at a character start", () => {
    const b = new TextEncoder().encode("aé€");
    expect(cutBytes(b, 10)).toBe(b);
    expect(Array.from(cutBytes(b, 2))).toEqual([0x61]);
    expect(Array.from(cutBytes(b, 3))).toEqual([0x61, 0xc3, 0xa9]);
    expect(cutBytes(b, 0).length).toBe(0);
    expect(clipBytes("aé€", 5)).toBe("aé");
    expect(clipBytes("aé€", 6)).toBe("aé€");
  });

  it("makes sets, differences and one-line text", () => {
    expect(stringSet("a", "", "b", "a")).toEqual(["a", "b"]);
    expect(difference(["a", "b", "c"], ["b"])).toEqual(["a", "c"]);
    expect(oneLine(" a\n\tb  c ")).toBe("a b c");
  });

  it("formats an outbound message", () => {
    expect(formatOutbound({ kind: "approval_request", text: "Ship the plan?", options: ["approve", "reject"] })).toBe(
      "**Approval needed**\n\nShip the plan?\n\nReply with one of:\n- `approve`\n- `reject`",
    );
    expect(formatOutbound({ kind: "question", text: " Pick? ", options: ["a", "b"] })).toBe(
      "**Question from the factory**\n\nPick?\n\nReply with one of:\n- `a`\n- `b`",
    );
    expect(formatOutbound({ kind: "update", text: "done\n\n" })).toBe("done");
  });
});

describe("event attributes", () => {
  it("has one name per key", () => {
    const values = Object.values(EVENT_ATTR);
    expect(new Set(values).size).toBe(values.length);
  });

  it("keeps the trigger match contract names", () => {
    expect(EVENT_ATTR.Repo).toBe("repo");
    expect(EVENT_ATTR.PRNumber).toBe("pr_number");
    expect(EVENT_ATTR.ActorIsBot).toBe("actor_is_bot");
    expect(EVENT_ATTR.WebhookID).toBe("webhook_id");
    expect(EVENT_PROVIDER.AzureDevOps).toBe("azure_devops");
    expect(EVENT_PROVIDER.Teams).toBe("teams");
  });
});
