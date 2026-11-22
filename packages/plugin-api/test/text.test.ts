import { describe, expect, it } from "vitest";
import { difference, extractMentions, formatOutbound, hasMention, splitRepo, stringSet, truncate } from "../src/text";

describe("extractMentions", () => {
  it.each([
    ["simple", "hey @alice can you look", ["alice"]],
    ["team and bot", "cc @acme/backend and @openfactory[bot].", ["acme/backend", "openfactory[bot]"]],
    ["code ignored", "```\n@ignored\n```\nand `@inline` but @real", ["real"]],
    ["quote ignored", "> @quoted said\n@me", ["me"]],
    ["email is not a mention", "mail bob@example.com", []],
    ["dedupe ignores case", "@a @A @b", ["a", "b"]],
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
  it("splits a repository path at the last slash", () => {
    expect(splitRepo("acme/platform/api")).toEqual({ owner: "acme/platform", name: "api" });
    expect(splitRepo("api")).toEqual({ owner: "", name: "api" });
  });

  it("truncates by code points", () => {
    expect(truncate("abcdef", 4)).toBe("abc…");
    expect(truncate("abc", 4)).toBe("abc");
    expect(truncate("😀😀😀", 2)).toBe("😀…");
    expect(truncate("abc", 1)).toBe("a");
  });

  it("makes sets and differences", () => {
    expect(stringSet("a", "", "b", "a")).toEqual(["a", "b"]);
    expect(difference(["a", "b", "c"], ["b"])).toEqual(["a", "c"]);
  });

  it("formats an outbound message", () => {
    const out = formatOutbound({
      workItemId: "w",
      source: { provider: "gitlab", kind: "issue" },
      kind: "approval_request",
      text: "Ship the plan?",
      options: ["approve", "reject"],
    });
    expect(out).toBe("**Approval needed**\n\nShip the plan?\n\nReply with one of:\n- `approve`\n- `reject`");
  });
});
