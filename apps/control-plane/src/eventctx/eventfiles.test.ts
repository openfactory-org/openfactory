// Port of internal/engine/eventfiles_test.go, bundle part: a webhook run gets
// the delivery body and the envelope; a chat run gets the thread from its
// context builder on the first message only. Intake stores the bundle and the
// WorkDO keeps its reference (src/do/ingress/intake.test.ts covers the path
// from HandleEvent; src/do/work/eventfiles.test.ts covers release and
// delivery).
import { describe, expect, it } from "vitest";
import type { Event } from "@openfactory/core";
import { utf8 } from "../lib/bytes";
import { build, cleanRel, decode, encode, promptSection, type Builder } from "./index";

const dec = new TextDecoder();
const files = (b: { files: { path: string; data: Uint8Array }[] } | null) =>
  Object.fromEntries((b?.files ?? []).map((f) => [f.path, dec.decode(f.data)]));

const threadBuilder: Builder = {
  provider: () => "slack",
  eventContext: async () => ({
    files: [
      { path: "thread.json", data: utf8('[{"ts":"1.0","user":"U1","text":"The staging deploy broke at 10:02."}]') },
    ],
    omitted: [{ name: "screenshot.png", reason: "larger than 1024 KiB" }],
  }),
};

describe("event files of webhook and chat runs", () => {
  it("a webhook run stores the payload and the envelope", async () => {
    const ev = {
      id: "evt_w",
      team_id: "team1",
      factory_id: "fac_1",
      provider: "webhook",
      type: "received",
      delivery_id: "dlv-9",
      actor: { provider: "webhook", external_id: "wh_1", handle: "ci-alerts" },
      source: { provider: "webhook", kind: "delivery", external_id: "dlv-9", title: "ci-alerts" },
      attributes: { webhook_id: "wh_1", webhook_name: "ci-alerts" },
      received_at: "2026-01-01T00:00:00Z",
      payload: { build: { status: "failed", id: 42 } },
    } as unknown as Event;
    const b = decode(encode((await build("msg_1", ev, true))!));
    const f = files(b);
    expect(f[".openfactory/events/msg_1/payload.json"]).toContain('"status":"failed"');
    expect(f[".openfactory/events/msg_1/envelope.json"]).toContain('"delivery": "dlv-9"');
    expect(Object.keys(f)[0]).toBe(".openfactory/events/msg_1/envelope.json");
    expect(promptSection(b)).toContain("Webhook: ci-alerts, delivery dlv-9");
  });

  it("a chat run gets the thread; a later message has the envelope and payload only", async () => {
    const ev = {
      id: "evt_s",
      team_id: "team1",
      provider: "slack",
      type: "app_mention",
      actor: { provider: "slack", external_id: "U2", handle: "dana" },
      source: { provider: "slack", kind: "thread", channel: "C1", thread_ref: "1.0" },
      text: "@factory fix the deploy",
      received_at: "2026-01-01T00:00:00Z",
    } as unknown as Event;
    const first = await build("msg_a", ev, true, [threadBuilder]);
    expect(files(first)[".openfactory/events/msg_a/context/thread.json"]).toContain(
      "The staging deploy broke at 10:02.",
    );
    const prompt = promptSection(first);
    expect(prompt).toContain("context/thread.json");
    expect(prompt).toContain("Left out: screenshot.png");

    const second = await build("msg_b", ev, false, [threadBuilder]);
    expect(second!.envelope.context_files ?? []).toEqual([]);
    expect(files(second)[".openfactory/events/msg_b/envelope.json"]).toBeDefined();
  });

  it("a builder failure is listed as left out; bad and extra files are refused", async () => {
    const failing: Builder = {
      provider: () => "slack",
      eventContext: async () => {
        throw new Error("rate limited");
      },
    };
    const many: Builder = {
      provider: () => "slack",
      eventContext: async () => ({
        files: [
          { path: "../../etc", data: utf8("x") },
          ...Array.from({ length: 41 }, (_, i) => ({ path: `f${i}.txt`, data: utf8("x") })),
        ],
      }),
    };
    const ev = {
      id: "e",
      team_id: "t",
      provider: "slack",
      type: "message",
      actor: { provider: "slack" },
      source: {},
      received_at: "",
    } as unknown as Event;
    const b = await build("m", ev, true, [failing, many]);
    const reasons = (b!.envelope.omitted ?? []).map((o) => `${o.name}: ${o.reason}`);
    expect(reasons).toContain("context: rate limited");
    // "../../etc" goes above context/: it is refused, not cleaned to "etc".
    expect(reasons).toContain("../../etc: bad file name");
    expect(reasons.filter((r) => r.endsWith("the context size limit was reached"))).toHaveLength(1);
    expect(b!.envelope.context_files).toHaveLength(40);
    expect(b!.envelope.context_files).not.toContain("context/etc");
    expect(await build("", ev, true)).toBeNull();
    expect(await build("m", null, true)).toBeNull();
  });

  it("cleanRel refuses absolute paths and paths above the root", () => {
    const cases: [string, string][] = [
      ["thread.json", "thread.json"],
      ["a/./b//c.txt", "a/b/c.txt"],
      ["a/../b.txt", "b.txt"],
      ["a\\b.txt", "a/b.txt"],
      ["", ""],
      [".", ""],
      ["a/..", ""],
      ["..", ""],
      ["../etc/passwd", ""],
      ["a/../../etc", ""],
      ["/abs/file", ""],
      ["\\abs\\file", ""],
      ["C:/x.txt", ""],
      ["c:x.txt", ""],
    ];
    for (const [in_, want] of cases) expect(cleanRel(in_), in_).toBe(want);
  });

  it("a builder that answers in time leaves no timer behind", async () => {
    const set = globalThis.setTimeout;
    const clear = globalThis.clearTimeout;
    const live = new Set<unknown>();
    globalThis.setTimeout = ((fn: () => void, ms?: number) => {
      const id = set(fn, ms);
      live.add(id);
      return id;
    }) as typeof setTimeout;
    globalThis.clearTimeout = ((id: Parameters<typeof clearTimeout>[0]) => {
      live.delete(id);
      clear(id);
    }) as typeof clearTimeout;
    try {
      const ev = {
        id: "e",
        team_id: "t",
        provider: "slack",
        type: "message",
        actor: { provider: "slack" },
        source: {},
        received_at: "",
      } as unknown as Event;
      const failing: Builder = {
        provider: () => "slack",
        eventContext: async () => {
          throw new Error("rate limited");
        },
      };
      await build("m", ev, true, [threadBuilder, failing]);
      expect(live.size).toBe(0);
    } finally {
      globalThis.setTimeout = set;
      globalThis.clearTimeout = clear;
    }
  });
});
