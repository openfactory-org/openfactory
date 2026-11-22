// The runner protocol /runner/v1 with a fake runner. Every frame is validated
// against schemas/runner/v1.
import { beforeAll, expect, it } from "vitest";
import { waitFor } from "../harness/api.ts";
import { adminApi, createTestFactory, type CreatedFactory } from "../harness/context.ts";
import { FakeRunner, RunnerHttpError } from "../harness/runner.ts";
import { useSessions } from "../harness/sessions.ts";
import { describeFeature, TAGS, target } from "../harness/target.ts";

interface Run {
  run_id: string;
  state: string;
  phase?: string;
}

describeFeature([TAGS.runner], "runner protocol v1", () => {
  const sessions = useSessions();
  let f: CreatedFactory;

  beforeAll(async () => {
    f = await createTestFactory(adminApi());
  });

  async function newRun(prompt: string): Promise<{ run: Run; runner: FakeRunner }> {
    const run = await adminApi().post<Run>("/runs", { prompt, factory_id: f.uid, agent_name: "foreman" }, 201);
    const env = await sessions.awaitSession(run.run_id, 1);
    return { run, runner: new FakeRunner(env) };
  }

  it("session start with the bootstrap token", async () => {
    const { run, runner } = await newRun("contract: start");
    expect(runner.env.OF_SESSION).toBe("1");
    expect(runner.env.OF_BOOTSTRAP_TOKEN).toMatch(/^ofb_/);
    const start = await runner.start();
    expect(start.run_id).toBe(run.run_id);
    expect(start.session).toBe(1);
    expect(start.session_token).toBeTruthy();
    expect(start.harness.mcp_servers?.factory?.url).toBeTruthy();
    const r = await adminApi().get<Run>(`/runs/${run.run_id}`);
    expect(["CLAIMED", "RUNNING", "PENDING"]).toContain(r.state);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("a wrong bootstrap token is refused", async () => {
    const { run, runner } = await newRun("contract: bad token");
    const res = await runner.call("POST", "/runner/v1/session/start", {
      body: runner.startRequest(),
      token: "ofb_notavalidtokennotavalidtokennotavalidtoke",
    });
    expect(res.status).toBe(401);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("session requests without a session token are refused", async () => {
    const { run, runner } = await newRun("contract: no token");
    await runner.start();
    const res = await runner.call("GET", "/runner/v1/session/commands", {
      token: "",
      query: { after: "0", wait: "0" },
    });
    expect(res.status).toBe(401);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("session.ready, then turn.start with the prompt; a turn writes messages", async () => {
    const { run, runner } = await newRun("contract: hello turn");
    await runner.start();
    expect(await runner.ready()).toBe(1);
    const cmd = await runner.nextCommand(["turn.start"]);
    const input = cmd.data.input as { kind?: string; content?: string }[];
    expect(input.some((i) => (i.content ?? "").includes("contract: hello turn"))).toBe(true);
    const ack = await runner.completeTurn(cmd.data.turn_id, "CONTRACT-REPLY");
    expect(ack).toBe(runner.lastSeq);
    const msgs = await waitFor("assistant message in the run", async () => {
      const m = await adminApi().get<{ messages: { role: string; content: string }[] }>(`/runs/${run.run_id}/messages`);
      return m.messages.some((x) => x.role === "assistant" && x.content.includes("CONTRACT-REPLY")) ? m : undefined;
    });
    expect(msgs.messages.length).toBeGreaterThan(0);
    await runner.idle();
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("events are acknowledged once; a repeated batch keeps the ack", async () => {
    const { run, runner } = await newRun("contract: ack");
    await runner.start();
    await runner.ready();
    const at = new Date().toISOString();
    const batch = { events: [{ seq: 2, type: "diagnostic", at, data: { code: "contract", message: "first" } }] };
    const first = await runner.sendBatch(batch);
    expect(first).toBe(2);
    const again = await runner.sendBatch(batch);
    expect(again).toBe(2);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("token renewal, secrets and conversation", async () => {
    const { run, runner } = await newRun("contract: token");
    await runner.start();
    const old = runner.token;
    const fresh = await runner.refreshToken();
    expect(fresh).toBeTruthy();
    expect(typeof old).toBe("string");
    const secrets = await runner.secrets();
    expect(secrets).toBeTypeOf("object");
    await runner.conversation();
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("a user message to a live session arrives as a command", async () => {
    const { run, runner } = await newRun("contract: live message");
    await runner.start();
    await runner.ready(["interrupt", "mid_turn_input"]);
    const first = await runner.nextCommand(["turn.start"]);
    await runner.completeTurn(first.data.turn_id, "first done");
    await runner.idle();
    const posted = await adminApi().request("POST", `/runs/${run.run_id}/messages`, {
      body: { message: "CONTRACT-FOLLOWUP" },
    });
    expect([200, 201, 202]).toContain(posted.status);
    const cmd = await runner.nextCommand(["turn.start", "message.deliver"]);
    expect(JSON.stringify(cmd.data)).toContain("CONTRACT-FOLLOWUP");
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("cancel during a session sends session.cancel or answers run_cancelled", async () => {
    const { run, runner } = await newRun("contract: cancel live");
    await runner.start();
    await runner.ready();
    await runner.nextCommand(["turn.start"]);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
    const outcome = await waitFor("session.cancel or run_cancelled", async () => {
      try {
        const cmds = await runner.commands(2);
        return cmds.find((c) => c.type === "session.cancel" || c.type === "session.stop") ? "command" : undefined;
      } catch (e) {
        if (e instanceof RunnerHttpError && (e.res.status === 410 || e.res.body?.code === "run_cancelled"))
          return "gone";
        throw e;
      }
    });
    expect(["command", "gone"]).toContain(outcome);
  });

  it("session end moves the run out of RUNNING", async () => {
    const { run, runner } = await newRun("contract: end");
    await runner.start();
    await runner.ready();
    const cmd = await runner.nextCommand(["turn.start"]);
    await runner.completeTurn(cmd.data.turn_id, "done before end");
    const res = await runner.end("stopped");
    expect(res.status).toBeLessThan(300);
    sessions.release(run.run_id, 1);
    const after = await waitFor("run leaves RUNNING after session end", async () => {
      const r = await adminApi().get<Run>(`/runs/${run.run_id}`);
      return r.state !== "RUNNING" && r.state !== "CLAIMED" ? r : undefined;
    });
    expect(["SUCCEEDED", "QUEUED", "PENDING", "FAILED", "ERROR", "CANCELLED", "BLOCKED"]).toContain(after.state);
    // The ended session's token no longer works.
    const late = await runner.call("GET", "/runner/v1/session/commands", { query: { after: "0", wait: "0" } });
    expect(late.status).toBeGreaterThanOrEqual(400);
  });
});

describeFeature([TAGS.runner, TAGS.runnerVersion], "runner version", () => {
  const sessions = useSessions();

  it("session start refuses an unsupported runner version", async () => {
    const f = await createTestFactory(adminApi());
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: version", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const env = await sessions.awaitSession(run.run_id, 1);
    const runner = new FakeRunner(env, { version: target.badRunnerVersion });
    const res = await runner.call("POST", "/runner/v1/session/start", {
      body: runner.startRequest(),
      token: env.OF_BOOTSTRAP_TOKEN,
    });
    expect(res.status).toBe(426);
    expect(res.body?.code).toBe("unsupported_runner_version");
    // The right version still starts the same session.
    const ok = await new FakeRunner(env).start();
    expect(ok.run_id).toBe(run.run_id);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });
});
