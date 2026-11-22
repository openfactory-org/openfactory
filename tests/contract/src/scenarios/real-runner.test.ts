// Opt-in: the real Go runner with the exec harness takes a session.
// Enable with OF_CONTRACT_REAL_RUNNER=1 (run.mjs --real-runner). The runner
// command is OF_CONTRACT_RUNNER_CMD (default: build ./cmd/openfactory and run
// `<binary> runner`).
import { execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { beforeAll, expect, it } from "vitest";
import { waitFor } from "../harness/api.ts";
import { adminApi, createTestFactory } from "../harness/context.ts";
import { REPO_ROOT } from "../harness/paths.ts";
import { useSessions } from "../harness/sessions.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

describeFeature([TAGS.runner, TAGS.realRunner], "real Go runner (exec harness)", () => {
  const sessions = useSessions();
  let command: string[] = [];

  beforeAll(() => {
    const given = process.env.OF_CONTRACT_RUNNER_CMD;
    if (given) {
      command = given.split(/\s+/).filter(Boolean);
      return;
    }
    const bin = join(mkdtempSync(join(tmpdir(), "of-contract-runner-")), "openfactory");
    execFileSync("go", ["build", "-o", bin, "./cmd/openfactory"], { cwd: REPO_ROOT, stdio: "inherit" });
    command = [bin, "runner"];
  }, 300_000);

  it("runs one turn: the exec harness echoes the prompt as the reply", async () => {
    const api = adminApi();
    const f = await createTestFactory(api);
    const run = await api.post<{ run_id: string }>(
      "/runs",
      { prompt: "CONTRACT-ECHO-7", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    await sessions.awaitSession(run.run_id, 1);
    sessions.exec(run.run_id, 1, command, { OF_RUNNER_EXEC_COMMAND: "cat" });
    const msgs = await waitFor(
      "assistant reply from the real runner",
      async () => {
        const m = await api.get<{ messages: { role: string; content: string }[] }>(`/runs/${run.run_id}/messages`);
        return m.messages.some((x) => x.role === "assistant" && x.content.includes("CONTRACT-ECHO-7")) ? m : undefined;
      },
      120_000,
      500,
    );
    expect(msgs.messages.length).toBeGreaterThan(0);
    await api.post(`/runs/${run.run_id}/cancel`, {});
  });
});
