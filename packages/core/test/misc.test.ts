import { describe, expect, it } from "vitest";
import {
  buildConversation,
  equalFold,
  canonicalJSON,
  checkNoMetadataEnv,
  compareJSONNumbers,
  defaultModel,
  harnessForModel,
  hashCanonical,
  inboxLink,
  integrationsLink,
  isOfError,
  mergeMetadata,
  metadataEnv,
  models,
  normalizeType,
  parseLossless,
  runLink,
  sha256Hex,
  stringifyLossless,
  toolIdempotencyKey,
  validateMetadata,
  validateReasoningLevel,
  workItemLink,
  factoryWebhookLink,
  isLosslessNumber,
} from "../src/index";

describe("conversation", () => {
  it("keeps call ids and arguments of the runner path", () => {
    const conv = buildConversation({ id: "run_1" }, [
      { id: "m1", role: "user", content: "go" },
      { id: "m2", role: "tool_call", content: "bash", data: { call_id: "c1", arguments: { command: "ls" } } },
      { id: "m3", role: "tool_result", content: "a.txt", data: '{"call_id":"c1"}' },
    ]);
    const blocks = conv.steps!.flatMap((s) => s.messages!.flatMap((m) => m.blocks ?? []));
    expect(blocks).toHaveLength(3);
    expect(blocks[1]).toMatchObject({
      type: "action",
      name: "bash",
      call_id: "c1",
      input: { command: "ls" },
      state: "completed",
    });
    expect(blocks[2]).toMatchObject({ type: "action_result", call_id: "c1", text: "a.txt" });
    expect(conv.conversation_id).toBe("run_1");
    expect(conv.steps![0]!.step_id).toBe("step_1");
  });

  it("survives tool messages without data", () => {
    const conv = buildConversation({ id: "run_1" }, [
      { id: "m1", role: "tool_call", content: "bash" },
      { id: "m2", role: "tool_result", content: "ok", data: "not json" },
      { id: "m3", role: "system", content: "note" },
      { id: "m4", role: "log", content: "ignored" },
    ]);
    const msgs = conv.steps![0]!.messages!;
    expect(msgs).toHaveLength(2);
    expect(msgs[0]!.blocks).toHaveLength(2);
    expect(msgs[0]!.blocks![0]).toEqual({ type: "action", name: "bash", state: "completed" });
    expect(msgs[1]).toMatchObject({ role: "system", kind: "system" });
  });
});

describe("metadata", () => {
  it("maps keys to OF_METADATA_ variables", () => {
    expect(metadataEnv({ setup_profile: "backend", "team.name": "x", "ticket-id": "ENG-1" })).toEqual({
      OF_METADATA_SETUP_PROFILE: "backend",
      OF_METADATA_TEAM_NAME: "x",
      OF_METADATA_TICKET_ID: "ENG-1",
    });
  });

  const many: Record<string, string> = {};
  for (let i = 0; i < 21; i++) many["k".repeat(i + 1)] = "v";
  it.each([
    ["empty", null, true],
    ["valid", { "a.b-c_D9": "v".repeat(256) }, true],
    ["empty value", { k: "" }, true],
    ["too many", many, false],
    ["empty key", { "": "v" }, false],
    ["long key", { ["k".repeat(65)]: "v" }, false],
    ["bad char", { "a b": "v" }, false],
    ["long value", { k: "v".repeat(257) }, false],
    ["multi-byte value", { k: "ä".repeat(129) }, false],
    ["nul", { k: "a\u0000b" }, false],
    ["utf8", { k: "\ud800" }, false],
    ["collision", { "a.b": "1", "a-b": "2" }, false],
    ["case collision", { Key: "1", KEY: "2" }, false],
  ] as const)("%s", (_name, meta, ok) => {
    if (ok) expect(() => validateMetadata(meta)).not.toThrow();
    else {
      try {
        validateMetadata(meta);
        expect.unreachable();
      } catch (e) {
        expect(isOfError(e, "invalid_request")).toBe(true);
      }
    }
  });

  it("merges child over batch over parent and checks the limit", () => {
    expect(
      mergeMetadata({ a: "parent", b: "parent", c: "parent" }, { b: "batch", c: "batch" }, { c: "child" }),
    ).toEqual({
      a: "parent",
      b: "batch",
      c: "child",
    });
    const parent: Record<string, string> = {};
    for (let i = 0; i < 20; i++) parent["p".repeat(i + 1)] = "v";
    expect(() => mergeMetadata(parent, null, { extra: "v" })).toThrow();
    expect(() => checkNoMetadataEnv({ of_metadata_x: "1" })).toThrow(/reserved/);
    expect(() => checkNoMetadataEnv({ OTHER: "1" })).not.toThrow();
  });

  it("reports problems in Go wording", () => {
    expect(() => validateMetadata({ "a.b": "1", "a-b": "2" })).toThrow(
      'metadata keys "a-b" and "a.b" both map to OF_METADATA_A_B',
    );
  });
});

describe("links", () => {
  it("builds dashboard paths", () => {
    expect(runLink("https://of.example.com/", "run_1")).toBe("https://of.example.com/runs/run_1");
    expect(runLink("", "run 1")).toBe("/runs/run%201");
    expect(workItemLink("https://x", "fac/1", "wi_1")).toBe("https://x/f/fac%2F1/work/wi_1");
    expect(factoryWebhookLink("https://x", "f", "w")).toBe("https://x/f/f/webhooks/w");
    expect(inboxLink("https://x")).toBe("https://x/inbox");
    expect(integrationsLink("https://x")).toBe("https://x/integrations");
    expect(integrationsLink("https://x", "slack", "a b")).toBe("https://x/integrations?connection=a+b&provider=slack");
  });
});

describe("harness models", () => {
  it("has the catalog", () => {
    expect(defaultModel("claude")).toBe("claude-opus-5-5");
    expect(defaultModel("codex-cli")).toBe("gpt-5.3-codex");
    expect(defaultModel("nope")).toBe("");
    expect(models("claude-code").length).toBeGreaterThan(5);
    expect(normalizeType("")).toBe("claude");
    expect(harnessForModel("anthropic.claude-sonnet-5")).toBe("claude");
    expect(harnessForModel("o3-mini")).toBe("codex");
    expect(harnessForModel("gemini")).toBe("");
    expect(validateReasoningLevel("claude", "HIGH")).toBeNull();
    expect(validateReasoningLevel("codex", "max")).toContain("invalid reasoningLevel");
    expect(validateReasoningLevel("other", "low")).toBe('harness "other" does not accept reasoningLevel');
  });
});

describe("canonical JSON and hashing", () => {
  it("sorts keys at every level and uses Go escaping", () => {
    expect(canonicalJSON({ b: 1, a: { d: [1, "x<y"], c: null }, u: undefined })).toBe(
      '{"a":{"c":null,"d":[1,"x\\u003cy"]},"b":1}',
    );
    expect(canonicalJSON({ b: 1, a: 2 })).toBe(canonicalJSON({ a: 2, b: 1 }));
  });

  it("hashes with SHA-256", async () => {
    expect(await sha256Hex("abc")).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    expect(await hashCanonical({ b: 1, a: 2 })).toBe(await sha256Hex('{"a":2,"b":1}'));
    const k = await toolIdempotencyKey("run_1", "t1", "run_agents", { x: 1 });
    expect(k).toMatch(/^orch:idem:run_1:t1:run_agents:[0-9a-f]{64}$/);
  });

  it("keeps big numbers exact", () => {
    const v = parseLossless('{"n":9007199254740993,"f":1.10}');
    const n = (v as Record<string, unknown>)["n"];
    expect(isLosslessNumber(n)).toBe(true);
    expect(String(n)).toBe("9007199254740993");
    expect(canonicalJSON(v)).toBe('{"f":1.10,"n":9007199254740993}');
    expect(stringifyLossless(v)).toBe('{"n":9007199254740993,"f":1.10}');
    expect(compareJSONNumbers("9007199254740993", "9007199254740992")).toBe(1);
    expect(compareJSONNumbers("1.10", "1.1")).toBe(0);
    expect(compareJSONNumbers("-2", "1e-3")).toBe(-1);
    expect(compareJSONNumbers("1e3", "999.9")).toBe(1);
    expect(compareJSONNumbers("0.0", "-0")).toBe(0);
    expect(compareJSONNumbers("-10", "-9")).toBe(-1);
    expect(compareJSONNumbers("x", "1")).toBeNull();
  });
});

describe("equalFold", () => {
  // Expected values are Go strings.EqualFold results.
  it.each([
    ["Go", "GO", true],
    ["go", "gopher", false],
    ["", "", true],
    ["ß", "SS", false],
    ["ß", "ss", false],
    ["ß", "\u1e9e", true],
    ["\u212a", "k", true],
    ["\u212a", "K", true],
    ["σ", "ς", true],
    ["ς", "Σ", true],
    ["ΑΣ", "ας", true],
    ["ΑΣ", "ασ", true],
    ["ſ", "S", true],
    ["\ufb01", "FI", false],
    ["İ", "i", false],
  ])("%s ~ %s", (a, b, want) => {
    expect(equalFold(a, b)).toBe(want);
  });
});
