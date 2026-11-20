import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { clip, firstLine } from "../lib/format";
import { describeEvent, inboxKinds, isInboxFollowUp } from "../pages/Work";

const nameOf = (id: string) => (id === "usr_me" ? "You" : id === "usr_ana" ? "Ana" : undefined);
const runs: Record<string, { type: "user" | "service_account" | "factory"; id: string; name?: string }> = {
  run_me: { type: "user", id: "usr_me" },
  run_ana: { type: "user", id: "usr_ana" },
  run_bot: { type: "service_account", id: "sa_1", name: "ci-bot" },
};
const actors = { nameOf, itemCreatorID: "usr_me", runCreator: (id: string) => runs[id] };

function text(e: Parameters<typeof describeEvent>[0]): string {
  const { container } = render(<p>{describeEvent(e, actors).text}</p>);
  return container.textContent ?? "";
}

describe("work item timeline actors", () => {
  it("names the person behind an event the same way for every kind", () => {
    expect(text({ kind: "message_in", actor: "user:usr_me", data: { text: "hi" } })).toBe("You wrote");
    expect(text({ kind: "run_started", actor: "dashboard", run_id: "run_me", data: { agent: "foreman" } })).toBe("You started foreman");
    expect(text({ kind: "run_started", actor: "api", run_id: "run_ana", data: { agent: "foreman" } })).toBe("Ana started foreman");
    expect(text({ kind: "run_started", actor: "api", run_id: "run_bot", data: { agent: "foreman" } })).toBe("ci-bot started foreman");
    expect(text({ kind: "stage_changed", actor: "dashboard", data: { to: "intake" } })).toMatch(/^You moved this to/);
    expect(text({ kind: "run_started", actor: "api", run_id: "run_unknown", data: { agent: "foreman" } })).toBe("A team member started foreman");
    expect(text({ kind: "message_in", actor: "user:usr_gone", data: { text: "hi" } })).toBe("A team member wrote");
  });

  it("names automations instead of showing their IDs", () => {
    const withAutos = { ...actors, automationName: (id: string) => (id === "aut_1" ? "qa-admin-hook" : undefined) };
    const t = (actor: string) => {
      const { container } = render(<p>{describeEvent({ kind: "stage_changed", actor, data: { to: "intake" } }, withAutos).text}</p>);
      return container.textContent ?? "";
    };
    expect(t("webhook:aut_1")).toMatch(/^The automation qa-admin-hook moved this/);
    expect(t("schedule:aut_unknown")).toMatch(/^An automation moved this/);
    expect(text({ kind: "stage_changed", actor: "automation:aut_x", data: { to: "intake" } })).not.toContain("aut_x");
  });
});

describe("work item timeline text", () => {
  it("shows the title and body of a handoff note", () => {
    const e = { kind: "note", actor: "run:run_1", run_id: "run_1", data: { kind: "handoff", title: "Ready for your review", body: "PR 42 is open" } };
    expect(text(e)).toBe("The foreman handed off: Ready for your review");
    expect(describeEvent(e, actors).body).toBe("PR 42 is open");
    expect(text({ kind: "note", actor: "system", data: { text: "handoff closed" } })).toBe("The factory noted");
  });

  it("says a blocked run waits on a person", () => {
    expect(text({ kind: "run_finished", actor: "system", data: { state: "blocked", name: "foreman" } })).toBe("foreman is waiting on a person");
    expect(text({ kind: "run_finished", actor: "system", data: { state: "failed", name: "foreman" } })).toBe("foreman ended: failed");
  });

  it("hides the platform copy of an inbox answer", () => {
    expect(isInboxFollowUp({ kind: "message_in", actor: "user:usr_me", data: { text: "[human answer] You answered your question" } })).toBe(true);
    expect(isInboxFollowUp({ kind: "message_in", actor: "user:usr_me", data: { text: "[human decision] Approval request" } })).toBe(true);
    expect(isInboxFollowUp({ kind: "message_in", actor: "user:usr_me", data: { text: "please also fix [human answer]" } })).toBe(false);
    expect(isInboxFollowUp({ kind: "answer", actor: "user:usr_me", data: { resolution: "[human answer] x" } })).toBe(false);
  });
});

describe("clip", () => {
  it.each([
    ["short text", 20, "short text"],
    ["Add a README in the repository root. Then link it.", 40, "Add a README in the repository root."],
    ["one two three four five six seven", 12, "one two thr…"],
    ["Hi. one two three four five six seven", 20, "Hi. one two three f…"],
    ["Use version 1.2.3 of the library everywhere", 16, "Use version 1.2…"],
  ])("clips %j to %i", (s, max, want) => {
    expect(clip(s, max)).toBe(want);
    expect(firstLine(s, max)).toBe(want);
  });
});

describe("work item timeline inbox events", () => {
  const title = "Should I say hello or goodbye?";

  it("does not repeat the title of a question in its body", () => {
    const e = { kind: "question", actor: "run:run_1", run_id: "run_1", data: { kind: "question", title, body: `${title}\n\nThis is the QA ask test.` } };
    expect(text(e)).toBe(`The foreman asked: ${title}`);
    expect(describeEvent(e, actors).body).toBe("This is the QA ask test.");
    const only = { ...e, data: { ...e.data, body: ` ${title.toUpperCase()} ` } };
    expect(describeEvent(only, actors).body).toBe("");
  });

  it("says a person resolved a handoff, a failure or a note, and keeps real answers", () => {
    const kinds = inboxKinds(
      [{ id: "in_f", kind: "failure" }],
      [
        { kind: "note", actor: "run:run_1", data: { inbox_id: "in_h", kind: "handoff", title: "Ready" } },
        { kind: "note", actor: "run:run_1", data: { inbox_id: "in_n", kind: "answered", title: "FYI" } },
        { kind: "question", actor: "run:run_1", data: { inbox_id: "in_q", kind: "question", title } },
      ],
    );
    const withKinds = { ...actors, inboxKind: (id: string) => kinds.get(id) };
    const t = (data: Record<string, unknown>) => {
      const { container } = render(<p>{describeEvent({ kind: "answer", actor: "user:usr_me", data }, withKinds).text}</p>);
      return container.textContent ?? "";
    };
    expect(t({ inbox_id: "in_h", resolution: "resolved" })).toBe("You resolved the handoff");
    expect(t({ inbox_id: "in_f", resolution: "resolved" })).toBe("You resolved the failure");
    expect(t({ inbox_id: "in_n", resolution: "seen" })).toBe("You resolved the note");
    expect(describeEvent({ kind: "answer", actor: "user:usr_me", data: { inbox_id: "in_h", resolution: "resolved" } }, withKinds).body).toBeUndefined();
    // A real answer keeps the answer text.
    expect(t({ inbox_id: "in_q", resolution: "goodbye" })).toBe("You · answer");
    expect(describeEvent({ kind: "answer", actor: "user:usr_me", data: { inbox_id: "in_q", resolution: "goodbye" } }, withKinds).body).toBe("goodbye");
  });
});
