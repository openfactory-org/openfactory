// propose_definition_change (orchestration propose_test.go and the scoring
// ProposeDefinitionChange path): the tool passes the files, the deletions and
// the message of the run, stores a proposed version with a review inbox item,
// returns the check errors so the agent can fix the files, refuses a cancelled
// run and a definition in git, and an approval applies the version.
import { env } from "cloudflare:workers";
import { newToken } from "@openfactory/core";
import { afterEach, describe, expect, it } from "vitest";
import { createRun, createWorkItem } from "../do/work/engine";
import { bootstrap, call } from "../../test/helpers";
import { agent, resultText, settle, withFx, workStub, TEAM } from "./fixture.test-util";
import { callTool } from "./index";

afterEach(settle);

const uniq = () => crypto.randomUUID().slice(0, 6);

async function managedFactory(key: string): Promise<{ id: string; active_version_id: string }> {
  const r = await call("POST", "/api/v1/factories", { key, body: { name: "Propose " + uniq() } });
  expect(r.status, JSON.stringify(r.body)).toBe(201);
  return r.body.factory;
}

async function runOf(
  factoryId: string,
  state: "running" | "cancelled" = "running",
  metadata: Record<string, string> = {},
): Promise<{ token: string; runId: string; wiId: string }> {
  const token = newToken("w");
  const out = await withFx(token, (fx) => {
    const wi = createWorkItem(fx, { factoryId, title: "improve" }, "usr_1");
    const r = createRun(
      fx,
      { factoryId, workItemId: wi.id, config: agent("improver"), name: "improver", prompt: "go", metadata },
      "usr_1",
    );
    fx.repo.putRun(Object.assign(fx.repo.mustRun(r.id), { state }));
    return { runId: r.id, wiId: wi.id };
  });
  return { token, ...out };
}

describe("propose_definition_change", () => {
  it("refuses a proposal that changes no file", async () => {
    const { key } = await bootstrap();
    const f = await managedFactory(key);
    const { runId } = await runOf(f.id);
    const res = await callTool(env, TEAM, runId, "propose_definition_change", { message: "m" });
    // No files: the merge refuses the empty change, and the error comes back.
    expect(res.isError).toBe(true);
    expect(resultText(res)).toContain("files or delete must name at least one file");
  });

  it("stores a proposal with a review item, returns check errors, and an approval applies it", async () => {
    const { key } = await bootstrap();
    const f = await managedFactory(key);
    const defn = (await call("GET", `/api/v1/factories/${f.id}/definition`, { key })).body;
    const yaml = defn.files["factory.yaml"] as string;
    const { token, runId } = await runOf(f.id);

    const ok = await callTool(env, TEAM, runId, "propose_definition_change", {
      files: { "factory.yaml": yaml.replace(/^description:.*$/m, "") + "description: Proposed text\n" },
      message: "Describe the factory",
    });
    expect(ok.isError, resultText(ok)).toBeFalsy();
    const m = /Proposed version (defv_\S+) is stored and waits for review \(base version (\S*)\)/.exec(resultText(ok));
    expect(m, resultText(ok)).not.toBeNull();
    const [, versionId, baseId] = m!;
    expect(baseId).toBe(f.active_version_id);

    const list = await call("GET", `/api/v1/factories/${f.id}/definition/proposals`, { key });
    expect(list.status).toBe(200);
    const prop = (list.body.items as { id: string; inbox_item_id?: string; author_run_id?: string }[]).find(
      (p) => p.id === versionId,
    );
    expect(prop?.author_run_id).toBe(runId);
    expect(prop?.inbox_item_id).toMatch(/^inb_/);

    const item = (await workStub(token).api({ requestId: "t" }, "getInbox", { id: prop!.inbox_item_id! }, null)) as {
      kind: string;
      title: string;
      body: string;
      options: string[];
      run_id: string;
    };
    expect(item.kind).toBe("review");
    expect(item.title).toBe("Review definition change: Describe the factory");
    expect(item.body).toContain("description: Proposed text");
    expect(item.body).toContain(versionId);
    expect(item.options).toEqual(["approve", "reject"]);
    expect(item.run_id).toBe(runId);

    // The check errors come back so that the agent can fix the files; nothing is stored.
    const bad = await callTool(env, TEAM, runId, "propose_definition_change", {
      files: { "factory.yaml": "name: [" },
      message: "broken on purpose",
    });
    expect(bad.isError).toBe(true);
    expect(resultText(bad)).toContain("the proposed definition has");
    const after = await call("GET", `/api/v1/factories/${f.id}/definition/proposals`, { key });
    expect(after.body.items).toHaveLength(1);

    // Deleting a file that does not exist is refused.
    const del = await callTool(env, TEAM, runId, "propose_definition_change", {
      delete: ["agents/none/agent.md"],
      message: "m",
    });
    expect(del.isError).toBe(true);
    expect(resultText(del)).toContain("cannot delete agents/none/agent.md");

    // An approval applies the proposed files as the active version and resolves the item.
    const ap = await call("POST", `/api/v1/factories/${f.id}/definition/proposals/${versionId}/approve`, { key });
    expect(ap.status, JSON.stringify(ap.body)).toBe(200);
    const now = (await call("GET", `/api/v1/factories/${f.id}/definition`, { key })).body;
    expect(now.files["factory.yaml"]).toContain("description: Proposed text");
    const resolved = (await workStub(token).api(
      { requestId: "t" },
      "getInbox",
      { id: prop!.inbox_item_id! },
      null,
    )) as { resolved: boolean };
    expect(resolved.resolved).toBe(true);
  });

  it("refuses a cancelled run and a benchmark trial", async () => {
    const { key } = await bootstrap();
    const f = await managedFactory(key);
    const dead = await runOf(f.id, "cancelled");
    const r1 = await callTool(env, TEAM, dead.runId, "propose_definition_change", {
      files: { "a.md": "b" },
      message: "m",
    });
    expect(r1.isError).toBe(true);
    expect(resultText(r1)).toContain("cancelled");

    const trial = await runOf(f.id, "running", { trial_mode: "true" });
    const r2 = await callTool(env, TEAM, trial.runId, "propose_definition_change", {
      files: { "a.md": "b" },
      message: "m",
    });
    expect(r2.isError).toBe(true);
    expect(resultText(r2)).toContain("benchmark trial");
    const list = await call("GET", `/api/v1/factories/${f.id}/definition/proposals`, { key });
    expect(list.body.items).toHaveLength(0);
  });
});
