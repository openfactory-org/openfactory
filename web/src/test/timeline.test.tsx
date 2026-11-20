import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { describeEvent } from "../pages/Work";

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
});
