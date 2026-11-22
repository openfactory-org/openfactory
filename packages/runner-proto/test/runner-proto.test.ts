import { describe, expect, it } from "vitest";
import { buildConversation } from "@openfactory/core";
import {
  clampWait,
  COMMAND_DATA_DEFS,
  EVENT_DATA_DEFS,
  isCommandType,
  isEventType,
  isRunner,
  MAX_COMMAND_WAIT_SECONDS,
  newCommand,
  newEvent,
  parseRunner,
  RUNNER_PATHS,
  RUNNER_SCHEMAS,
  validateCommand,
  validateEvent,
  validateEventBatch,
  validateRunner,
} from "../src/index";

const at = "2026-11-22T10:00:00Z";

describe("validators", () => {
  it("accepts valid events and rejects bad envelopes", () => {
    const ev = newEvent(1, "message", { role: "assistant", content: "hi" }, { at, turnId: "t1" });
    expect(ev).toEqual({
      seq: 1,
      type: "message",
      at: "2026-11-22T10:00:00.000Z",
      turn_id: "t1",
      data: { role: "assistant", content: "hi" },
    });
    expect(validateEvent(ev)).toEqual([]);
    expect(isRunner("events#Event", ev)).toBe(true);
    const bad = validateRunner("events#Event", { seq: 0, type: "nope", at });
    expect(bad.valid).toBe(false);
    if (!bad.valid)
      expect(bad.errors.map((e) => e.instanceLocation)).toEqual(expect.arrayContaining(["#/seq", "#/type"]));
    expect(isRunner("events#Event", { seq: 1, type: "idle", at, extra: true })).toBe(false);
  });

  it("checks the data of each event and command type", () => {
    expect(validateEvent({ seq: 2, type: "turn.ended", at, data: { outcome: "done" } })).toEqual([]);
    const errs = validateEvent({ seq: 2, type: "turn.ended", at, data: { outcome: "maybe" } });
    expect(errs.length).toBeGreaterThan(0);
    expect(errs.every((e) => e.instanceLocation.startsWith("#/data"))).toBe(true);
    expect(validateEvent(newEvent(3, "turn.started", undefined, { at }))).toEqual([]);
    expect(validateCommand(newCommand(1, "turn.start", { turn_id: "t1", input: [] }))).toEqual([]);
    expect(validateCommand(newCommand(2, "session.cancel", undefined))).toEqual([]);
    expect(validateCommand({ seq: 1, type: "session.stop", data: { reason: "idle" } }).length).toBeGreaterThan(0);
  });

  it("validates batches and names the bad event", () => {
    const good = { events: [newEvent(1, "usage", { cumulative: true, input_tokens: 1, output_tokens: 2 }, { at })] };
    expect(validateEventBatch(good)).toEqual({ batch: good, errors: [] });
    const bad = validateEventBatch({ events: [good.events[0], { seq: 2, type: "usage", at, data: {} }] });
    expect(bad.batch).toBeUndefined();
    expect(bad.errors.every((e) => e.instanceLocation.startsWith("#/events/1/data"))).toBe(true);
  });

  it("parses or throws", () => {
    expect(
      parseRunner("transfer#GitCredentialRequest", { protocol: "https", host: "github.com", path: "a/b" }).host,
    ).toBe("github.com");
    expect(() => parseRunner("transfer#GitCredentialRequest", { host: 1 })).toThrow(
      /invalid transfer#GitCredentialRequest/,
    );
  });

  it("accepts the conversation that core builds", () => {
    const conv = buildConversation({ id: "run_1" }, [
      { id: "m1", role: "user", content: "go" },
      { id: "m2", role: "assistant", content: "ok" },
      { id: "m3", role: "tool_call", content: "bash", data: { call_id: "c1", arguments: { command: "ls" } } },
      { id: "m4", role: "tool_result", content: "", data: { call_id: "c1", is_error: true } },
    ]);
    const r = validateRunner("conversation#Conversation", conv);
    expect(r.errors).toEqual([]);
  });

  it("maps every enum value to a data definition", () => {
    const evEnum = (RUNNER_SCHEMAS.events.$defs.Event.properties.type as { enum: string[] }).enum;
    expect(Object.keys(EVENT_DATA_DEFS).sort()).toEqual([...evEnum].sort());
    const cmdEnum = (RUNNER_SCHEMAS.commands.$defs.Command.properties.type as { enum: string[] }).enum;
    expect(Object.keys(COMMAND_DATA_DEFS).sort()).toEqual([...cmdEnum].sort());
    expect(isEventType("idle")).toBe(true);
    expect(isEventType("nope")).toBe(false);
    expect(isCommandType("turn.start")).toBe(true);
  });
});

describe("constants", () => {
  it("clamps the commands wait", () => {
    expect(clampWait(-1)).toBe(0);
    expect(clampWait(10)).toBe(10);
    expect(clampWait(99)).toBe(MAX_COMMAND_WAIT_SECONDS);
    expect(clampWait(Number.NaN)).toBe(0);
  });
  it("has the paths", () => {
    expect(RUNNER_PATHS.sessionCommands).toBe("/runner/v1/session/commands");
  });
});
