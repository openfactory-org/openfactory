// Run-loop scenarios with the fake runner: foreman → child → lifecycle message
// → parent turn; the factory concurrency limit; ask_human through the inbox.
import { expect, it } from "vitest";
import { waitFor } from "../harness/api.ts";
import { adminApi, createTestFactory } from "../harness/context.ts";
import { FakeRunner } from "../harness/runner.ts";
import { useSessions } from "../harness/sessions.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

interface Run {
  run_id: string;
  state: string;
  phase?: string;
  parent_run_id?: string;
  work_item_id?: string;
}

describeFeature([TAGS.runner, TAGS.scenario], "run loop: foreman, child, lifecycle, parent", () => {
  const sessions = useSessions();

  it("the child's finished turn comes back to the foreman as a new input", async () => {
    const api = adminApi();
    const f = await createTestFactory(api, {
      coder: { type: "CUSTOM", description: "Writes code for the contract suite" },
    });

    // 1. The foreman run starts; the fake runner takes the session.
    const parent = await api.post<Run>(
      "/runs",
      { prompt: "contract: delegate to coder", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const prunner = new FakeRunner(await sessions.awaitSession(parent.run_id, 1));
    await prunner.start();
    await prunner.ready();
    const pturn = await prunner.nextCommand(["turn.start"]);

    // 2. The foreman starts a child through the factory MCP server.
    const agents = await prunner.mcp<{ agents: { agent_identity_uid: string; name: string }[] }>("list_agents", {});
    const coder = agents.agents.find((a) => a.name === "coder");
    expect(coder, JSON.stringify(agents)).toBeDefined();
    await prunner.mcp("run_agents", {
      summary: "contract: one child",
      agent_run_configs: [{ name: "child-1", agent_identity_uid: coder!.agent_identity_uid, prompt: "CHILD-TASK" }],
    });
    await prunner.completeTurn(pturn.data.turn_id, "started the coder");
    await prunner.idle();

    // 3. The child run exists under the parent and gets its own session.
    const child = await waitFor("child run", async () => {
      const list = await api.get<{ items: Run[] }>("/runs", { query: { parent_run_id: parent.run_id } });
      return list.items[0];
    });
    expect(child.parent_run_id).toBe(parent.run_id);
    expect(child.work_item_id).toBe(parent.work_item_id);
    const crunner = new FakeRunner(await sessions.awaitSession(child.run_id, 1));
    await crunner.start();
    await crunner.ready();
    const cturn = await crunner.nextCommand(["turn.start"]);
    expect(JSON.stringify(cturn.data.input)).toContain("CHILD-TASK");
    await crunner.completeTurn(cturn.data.turn_id, "CHILD-RESULT-42");
    await crunner.idle();

    // 4. The lifecycle message reaches the parent: a new turn (or a delivered message) with the child's result.
    const lifecycle = await prunner.nextCommand(["turn.start", "message.deliver"], 45_000);
    expect(JSON.stringify(lifecycle.data.input)).toContain("CHILD-RESULT-42");
    if (lifecycle.type === "turn.start") {
      await prunner.completeTurn(lifecycle.data.turn_id, "parent saw the child result");
    }
    const msgs = await waitFor("parent reply stored", async () => {
      const m = await api.get<{ messages: { role: string; content: string }[] }>(`/runs/${parent.run_id}/messages`);
      return m.messages.some(
        (x) => x.content.includes("parent saw the child result") || x.content.includes("CHILD-RESULT-42"),
      )
        ? m
        : undefined;
    });
    expect(msgs.messages.length).toBeGreaterThan(0);

    await api.post(`/work-items/${parent.work_item_id}/cancel`, {});
  });
});

describeFeature([TAGS.runner, TAGS.scenario], "run loop: concurrency limit", () => {
  const sessions = useSessions();

  it("maxConcurrentRuns: 1 holds the second and third run; they start in FIFO order", async () => {
    const api = adminApi();
    const f = await createTestFactory(api, undefined, "maxConcurrentRuns: 1\n");
    const runs: Run[] = [];
    for (let i = 1; i <= 3; i++) {
      runs.push(
        await api.post<Run>("/runs", { prompt: `contract: limit ${i}`, factory_id: f.uid, agent_name: "foreman" }, 201),
      );
    }
    const [a, b, c] = runs as [Run, Run, Run];
    const ra = new FakeRunner(await sessions.awaitSession(a.run_id, 1));
    await ra.start();
    // While the first holds the only slot, the others get no session.
    await new Promise((r) => setTimeout(r, 2500));
    expect(sessions.hasSession(b.run_id, 1)).toBe(false);
    expect(sessions.hasSession(c.run_id, 1)).toBe(false);
    for (const r of [b, c]) {
      const got = await api.get<Run>(`/runs/${r.run_id}`);
      expect(got.state).toBe("QUEUED");
    }

    // The first ends: the second (not the third) gets the slot.
    await api.post(`/runs/${a.run_id}/cancel`, {});
    sessions.release(a.run_id, 1);
    const rb = new FakeRunner(await sessions.awaitSession(b.run_id, 1, 45_000));
    await rb.start();
    await new Promise((r) => setTimeout(r, 1500));
    expect(sessions.hasSession(c.run_id, 1)).toBe(false);

    await api.post(`/runs/${b.run_id}/cancel`, {});
    sessions.release(b.run_id, 1);
    await sessions.awaitSession(c.run_id, 1, 45_000);
    await api.post(`/runs/${c.run_id}/cancel`, {});
  });
});

describeFeature([TAGS.runner, TAGS.scenario], "run loop: ask_human through the inbox", () => {
  const sessions = useSessions();

  it("the question is an inbox item; the answer comes back to the run", async () => {
    const api = adminApi();
    const f = await createTestFactory(api);
    const run = await api.post<Run>(
      "/runs",
      { prompt: "contract: ask", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const runner = new FakeRunner(await sessions.awaitSession(run.run_id, 1));
    await runner.start();
    await runner.ready();
    const turn = await runner.nextCommand(["turn.start"]);
    await runner.mcp("ask_human", { question: "CONTRACT-QUESTION: blue or green?", options: ["blue", "green"] });
    await runner.completeTurn(turn.data.turn_id, "asked", "needs_input");
    await runner.idle();

    const item = await waitFor("inbox question", async () => {
      const inbox = await api.get<{
        items: { id: string; kind: string; run_id?: string; title?: string; body?: string }[];
      }>("/inbox", { query: { scope: "team", factory_id: f.uid } });
      return inbox.items.find((i) => i.kind === "question" && i.run_id === run.run_id);
    });
    await api.get(`/inbox/${item.id}`);
    await api.post(`/inbox/${item.id}/resolve`, { resolution: "green" });

    const answer = await runner.nextCommand(["turn.start", "message.deliver"], 45_000);
    expect(JSON.stringify(answer.data.input)).toContain("green");
    await api.post(`/runs/${run.run_id}/cancel`, {});
  });
});
