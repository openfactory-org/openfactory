// The text that a run posts through its MCP tools loses the secret values of
// the run before WorkDO stores it (internal/orchestration/redact_test.go).
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";
import type { Run } from "../engine/types";
import { buildRuns, resultText, settle, withFx } from "./fixture.test-util";
import { callTool, plannerRedactor, setRedactor } from "./index";

afterEach(async () => {
  await settle();
  setRedactor(plannerRedactor);
});

const SECRET = "sekret-value-1";

async function fixture() {
  return buildRuns(
    { wi: { stage: "triage", root: "root" } },
    {
      root: { type: "FOREMAN", workItem: "wi", state: "running" },
      kid: { workItem: "wi", parent: "root", state: "running" },
    },
  );
}

/** Every text that the WorkDO stored for the work item: events, inbox items, messages. */
async function stored(f: Awaited<ReturnType<typeof fixture>>): Promise<string> {
  return withFx(f.token, (fx) =>
    JSON.stringify([fx.repo.listWorkItemEvents(f.ids.wi!), fx.repo.listInbox({}), fx.repo.listMessages(f.ids.kid!)]),
  );
}

describe("run MCP redaction", () => {
  it("removes the secret values of the run from posted text", async () => {
    const f = await fixture();
    const asked: string[] = [];
    setRedactor(async (_env, run: Run) => {
      asked.push(run.id);
      return (s) => s.replaceAll(SECRET, "***");
    });
    const calls: [string, Record<string, unknown>][] = [
      ["post_update", { text: `the key is ${SECRET}` }],
      ["ask_human", { question: `Use ${SECRET}?`, details: `${SECRET} here`, options: [`yes ${SECRET}`, "no"] }],
      ["request_approval", { title: `Spec ${SECRET}`, summary: SECRET, links: [`https://x/?k=${SECRET}`] }],
      ["send_message", { run_id: f.ids.kid!, text: `pass ${SECRET}` }],
      ["follow_up_agent", { run_id: f.ids.kid!, message: `again ${SECRET}` }],
    ];
    for (const [tool, args] of calls) {
      const res = await callTool(env, "default", f.ids.root!, tool, args);
      expect(res.isError, `${tool}: ${resultText(res)}`).toBeFalsy();
    }
    const text = await stored(f);
    expect(text).toContain("the key is ***");
    expect(text).toContain("Use ***?");
    expect(text).toContain("pass ***");
    expect(text).toContain("again ***");
    expect(text).not.toContain("sekret");
    expect(new Set(asked)).toEqual(new Set([f.ids.root]));
  });

  it("an identity redactor changes nothing", async () => {
    const f = await fixture();
    setRedactor(async () => (s) => s);
    await callTool(env, "default", f.ids.root!, "post_update", { text: "plain text" });
    expect(await stored(f)).toContain("plain text");
  });

  it("a failing redactor keeps the text and the call", async () => {
    const f = await fixture();
    setRedactor(async () => {
      throw new Error("no planner");
    });
    const res = await callTool(env, "default", f.ids.root!, "post_update", { text: "still posted" });
    expect(res.isError).toBeFalsy();
    expect(await stored(f)).toContain("still posted");
  });
});
