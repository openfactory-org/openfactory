import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { MAX_BENCHMARK_REPETITIONS, validateRepetitions } from "../src/commands/benchmark.ts";
import { mcpMode } from "../src/commands/mcpregistry.ts";
import { printRewrap } from "../src/commands/secret.ts";
import { json, problem, router, runCli, type Call } from "./helpers.ts";

const KEY = { OF_API_KEY: "k", OF_SERVER: "http://srv.test" };
const factory = {
  uid: "fac_1",
  id: "fac_1",
  name: "Payments",
  alias: "pay",
  source: "managed",
  updated_at: "2026-11-22T00:00:00Z",
};
const page = (items: unknown[]): Response => json({ items, page_info: {} });
const body = (c: Call | undefined): unknown => JSON.parse(c?.body ?? "null");

describe("bootstrap", () => {
  it("posts without an API key and prints the key once", async () => {
    const r = await runCli(["bootstrap", "--email", "a@example.test", "--name", "Ada"], {
      vars: { OF_SERVER: "http://srv.test" },
      handler: router({
        "POST /bootstrap": json({
          team: { id: "t1", name: "Default" },
          user: { email: "a@example.test" },
          api_key: {},
          key: "of_k_s",
        }),
      }),
    });
    expect(r.code).toBe(0);
    expect(body(r.calls[0])).toEqual({ team_name: "Default", admin_email: "a@example.test", admin_name: "Ada" });
    expect(r.calls[0]?.headers.get("Authorization")).toBeNull();
    expect(r.out).toBe(
      "Team:    Default (t1)\nAdmin:   a@example.test\nAPI key: of_k_s\n\nThe key is shown only once:\n  export OF_SERVER=http://srv.test OF_API_KEY=of_k_s\n",
    );
  });

  it("needs --email", async () => {
    const r = await runCli(["bootstrap"]);
    expect(r.err).toBe("error: --email is required\n");
  });
});

describe("factory", () => {
  it("resolves a factory by alias and by name when the UID lookup fails", async () => {
    const handler = router({
      "GET /factories/@pay": problem(404, "resource_not_found", "not found"),
      "GET /factories/Payments": problem(404, "resource_not_found", "not found"),
      "GET /factories": page([factory]),
      "GET /factories/fac_1/agents": page([
        { name: "foreman", type: "FOREMAN", harness: { type: "claude", model: "m" } },
      ]),
    });
    const byAlias = await runCli(["factory", "get", "@pay"], { vars: KEY, handler });
    expect(byAlias.code).toBe(0);
    expect(byAlias.out).toBe(
      "UID:      fac_1\nName:     Payments\nAlias:    @pay\nSource:   managed\n\nAgents:\nNAME     TYPE     HARNESS  MODEL\nforeman  FOREMAN  claude   m\n",
    );
    const byName = await runCli(["factory", "get", "payments"], { vars: KEY, handler });
    expect(byName.code).toBe(0);
    const none = await runCli(["factory", "get", "zzz"], { vars: KEY, handler });
    expect(none.err).toBe('error: factory "zzz" not found\n');
  });

  it("passes other errors through", async () => {
    const r = await runCli(["factory", "get", "x"], {
      vars: KEY,
      handler: router({ "GET /factories/x": problem(403, "not_authorized", "Forbidden") }),
    });
    expect(r.code).toBe(1);
    expect(r.err).toContain("403 not_authorized");
  });

  it("creates a factory with repositories and agents", async () => {
    const r = await runCli(
      [
        "factory",
        "create",
        "--name",
        "P",
        "--repo",
        "gitlab:acme/web",
        "--repo",
        "acme/app",
        "--agents",
        "triage,spec",
      ],
      {
        vars: KEY,
        handler: router({ "POST /factories": json({ factory: factory }, 201) }),
      },
    );
    expect(r.code).toBe(0);
    expect(body(r.calls[0])).toEqual({
      name: "P",
      repositories: [
        { codeForge: "GITLAB", owner: "acme", name: "web" },
        { codeForge: "GITHUB", owner: "acme", name: "app" },
      ],
      agents: ["triage", "spec"],
    });
    expect(r.out).toBe("Created factory Payments (@pay): fac_1\n");
  });

  it("expands validation errors of the API", async () => {
    const r = await runCli(["factory", "create"], {
      vars: KEY,
      handler: () =>
        problem(400, "invalid_request", "Invalid", {
          errors: [
            { file: "factory.yaml", line: 3, message: "bad" },
            { file: "x.md", message: "worse" },
          ],
        }),
    });
    expect(r.code).toBe(1);
    expect(r.err).toContain("\n  factory.yaml:3: bad\n  x.md: worse\n");
  });

  it("expands validation errors of the API for every command", async () => {
    const r = await runCli(["secret", "set", "TOKEN", "v1", "--type", "nope"], {
      vars: KEY,
      handler: () =>
        problem(400, "invalid_request", "Invalid", { errors: [{ file: "type", message: "unknown secret type" }] }),
    });
    expect(r.code).toBe(1);
    expect(r.err).toContain("\n  type: unknown secret type\n");
  });

  it("dispatches a prompt of several words with metadata", async () => {
    const r = await runCli(["factory", "run", "fac_1", "fix", "the", "bug", "--title", "T", "--metadata", "a=b"], {
      vars: KEY,
      handler: router({
        "GET /factories/fac_1": json(factory),
        "POST /factories/fac_1/runs": json(
          { run_id: "run_1", work_item_id: "wi_1", run_url: "http://srv.test/runs/run_1" },
          201,
        ),
      }),
    });
    expect(r.code).toBe(0);
    expect(body(r.calls[1])).toEqual({ prompt: "fix the bug", title: "T", metadata: { a: "b" } });
    expect(r.out).toBe("Started run run_1 (work item wi_1)\nhttp://srv.test/runs/run_1\n");
  });

  it("reads the prompt from stdin for -", async () => {
    const r = await runCli(["work-item", "send", "fac_1", "-"], {
      vars: KEY,
      stdin: "from stdin\n",
      handler: router({
        "GET /factories/fac_1": json(factory),
        "POST /factories/fac_1/runs": json({ run_id: "r" }, 201),
      }),
    });
    expect(body(r.calls[1])).toEqual({ prompt: "from stdin\n" });
  });

  it("rejects a bad --metadata before the API is called", async () => {
    const r = await runCli(["factory", "run", "fac_1", "x", "--metadata", "nokv"], {
      vars: KEY,
      handler: router({ "GET /factories/fac_1": json(factory) }),
    });
    expect(r.code).toBe(1);
    expect(r.err).toBe('error: invalid --metadata "nokv" (want key=value)\n');
  });

  it("applies a definition to the factory with the same alias", async () => {
    const dir = mkdtempSync(join(tmpdir(), "of-cli-"));
    expect((await runCli(["init", dir, "--name", "Payments", "--alias", "pay"])).code).toBe(0);
    const r = await runCli(["factory", "apply", dir, "-m", "msg"], {
      vars: KEY,
      handler: router({
        "GET /factories/pay": json(factory),
        "PUT /factories/fac_1/definition": json({ version: { id: "ver_9" } }),
      }),
    });
    expect(r.code).toBe(0);
    const put = body(r.calls[1]) as { files: Record<string, string>; message: string };
    expect(put.message).toBe("msg");
    expect(Object.keys(put.files)).toContain("factory.yaml");
    expect(r.out).toBe("Applied version ver_9 to Payments (@pay)\n");
  });

  it("creates the factory when none matches and --factory is not set", async () => {
    const dir = mkdtempSync(join(tmpdir(), "of-cli-"));
    await runCli(["init", dir, "--name", "New", "--alias", "new"]);
    const r = await runCli(["factory", "apply", dir], {
      vars: KEY,
      handler: router({
        "GET /factories": page([]),
        "POST /factories": json({ factory: { ...factory, name: "New", alias: "new" } }, 201),
      }),
    });
    expect(r.code).toBe(0);
    expect(r.out).toBe("Created factory New (@new): fac_1\n");
  });

  it("refuses an invalid definition and prints the errors to stderr", async () => {
    const dir = mkdtempSync(join(tmpdir(), "of-cli-"));
    writeFileSync(join(dir, "factory.yaml"), "name: x\nbogus: 1\n");
    const r = await runCli(["factory", "apply", dir], { vars: KEY, handler: () => json({}) });
    expect(r.code).toBe(1);
    expect(r.err).toContain("factory.yaml");
    expect(r.calls).toHaveLength(0);
  });
});

describe("run", () => {
  it("lists runs with filters and a limit", async () => {
    const runs = [1, 2, 3].map((n) => ({
      run_id: `run_${n}`,
      state: "RUNNING",
      name: "n",
      agent_name: "foreman",
      created_at: "2026-11-22T11:59:00Z",
      title: `title ${n}`,
    }));
    const r = await runCli(["run", "list", "--state", "running,failed", "--limit", "2", "--work-item", "wi_1"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({ "GET /runs": page(runs) }),
    });
    expect(r.code).toBe(0);
    const u = new URL(r.calls[0]?.url ?? "");
    expect(u.searchParams.get("state")).toBe("RUNNING,FAILED");
    expect(u.searchParams.get("work_item_id")).toBe("wi_1");
    expect(r.out).toBe(
      "RUN    STATE    NAME  AGENT    CREATED  TITLE\nrun_1  RUNNING  n     foreman  1m ago   title 1\nrun_2  RUNNING  n     foreman  1m ago   title 2\n",
    );
  });

  it("shows a run", async () => {
    const r = await runCli(["run", "get", "run_1"], {
      vars: KEY,
      handler: router({
        "GET /runs/run_1": json({
          run_id: "run_1",
          state: "FAILED",
          agent_name: "foreman",
          name: "n",
          created_at: "2026-11-22T11:59:00Z",
          run_url: "u",
          usage: { input_tokens: 5, output_tokens: 7, cost_usd: 0.12345 },
          error: "boom",
          result: "done",
        }),
      }),
    });
    expect(r.out).toMatch(
      /^Run: {6}run_1\nState: {4}FAILED\nAgent: {4}foreman \(n\)\nCreated: {2}2026-11-22T\S+\nURL: {6}u\nUsage: {4}5 in \/ 7 out tokens, \$0\.1235\nError: {4}boom\n\ndone\n$/,
    );
  });

  it("prints a transcript and follows until the run ends", async () => {
    let polls = 0;
    const msgs = [
      { run_id: "r", seq: 1, role: "assistant", content: "hello\n", created_at: "2026-11-22T10:00:01Z" },
      { run_id: "r", seq: 2, role: "tool_call", content: "x ".repeat(300), created_at: "2026-11-22T10:00:02Z" },
    ];
    const r = await runCli(["run", "logs", "r", "-f"], {
      vars: KEY,
      handler: router({
        "GET /runs/r/messages": (c) => {
          const after = Number(new URL(c.url).searchParams.get("after"));
          return json({ messages: msgs.filter((m) => m.seq > after), has_more: false, next_after: 0 });
        },
        "GET /runs/r": () => json({ run_id: "r", state: ++polls > 0 ? "BLOCKED" : "RUNNING" }),
      }),
    });
    expect(r.code).toBe(0);
    const lines = r.out.trimEnd().split("\n");
    expect(lines[0]).toMatch(/^\d\d:\d\d:\d\d assistant {3}hello$/);
    expect(lines[1]).toMatch(/^\d\d:\d\d:\d\d tool_call {3}x x x/);
    expect(lines[1]?.endsWith("…")).toBe(true);
    expect(lines.at(-1)).toBe("-- run r is BLOCKED");
  });

  it("queues a follow-up and cancels with cascade", async () => {
    const r = await runCli(["run", "followup", "r", "and", "more"], {
      vars: KEY,
      handler: router({ "POST /runs/r/followups": json({ run_id: "r", state: "RUNNING" }) }),
    });
    expect(body(r.calls[0])).toEqual({ message: "and more" });
    expect(r.out).toBe("Follow-up queued for r (RUNNING)\n");
    const c = await runCli(["run", "cancel", "r"], { vars: KEY, handler: router({ "POST /runs/r/cancel": json({}) }) });
    expect(body(c.calls[0])).toEqual({ cascade: true });
    expect(c.out).toBe("Cancelled r\n");
  });
});

describe("work-item and inbox", () => {
  it("lists work items by stage", async () => {
    const r = await runCli(["work-item", "list", "fac_1", "--stage", "triage,spec"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /factories/fac_1": json(factory),
        "GET /factories/fac_1/work-items": page([
          { id: "wi_1", stage: "spec", source: { provider: "linear" }, updated_at: "2026-11-22T11:59:50Z", title: "T" },
        ]),
      }),
    });
    expect(new URL(r.calls[1]?.url ?? "").searchParams.get("stage")).toBe("triage,spec");
    expect(r.out).toBe("ID    STAGE  SOURCE  UPDATED  TITLE\nwi_1  spec   linear  10s ago  T\n");
  });

  it("completes a work item", async () => {
    const r = await runCli(["work-item", "complete", "wi_1", "--note", "ok"], {
      vars: KEY,
      handler: router({ "POST /work-items/wi_1/complete": json({ id: "wi_1", stage: "done" }) }),
    });
    expect(body(r.calls[0])).toEqual({ note: "ok" });
    expect(r.out).toBe("Work item wi_1 is done\n");
  });

  it("lists inbox items for the team including resolved ones", async () => {
    const r = await runCli(["inbox", "list", "--all", "--team"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /inbox": page([
          { id: "i1", kind: "approval", title: "Merge?", resolved: false, created_at: "2026-11-22T11:00:00Z" },
          {
            id: "i2",
            kind: "question",
            title: "Which?",
            resolved: true,
            resolution: "A",
            created_at: "2026-11-22T11:00:00Z",
          },
        ]),
      }),
    });
    const u = new URL(r.calls[0]?.url ?? "");
    expect([u.searchParams.get("include_resolved"), u.searchParams.get("scope")]).toEqual(["true", "team"]);
    expect(r.out).toBe(
      "ID  KIND      STATUS  CREATED  TITLE\ni1  approval  open    1h ago   Merge?\ni2  question  A       1h ago   Which?\n",
    );
  });

  it("resolves and unresolves", async () => {
    const r = await runCli(["inbox", "resolve", "i1", "looks", "good"], {
      vars: KEY,
      handler: router({ "POST /inbox/i1/resolve": json({ id: "i1", resolution: "looks good" }) }),
    });
    expect(body(r.calls[0])).toEqual({ resolution: "looks good" });
    expect(r.out).toBe("Resolved i1: looks good\n");
    const u = await runCli(["inbox", "unresolve", "i1"], {
      vars: KEY,
      handler: router({ "POST /inbox/i1/unresolve": json({ id: "i1" }) }),
    });
    expect(u.out).toBe("Reopened i1\n");
  });
});

describe("--output json", () => {
  // Every command that gets a resource back prints it with -o json.
  const cases: { args: string[]; route: string; answer: Record<string, unknown> }[] = [
    {
      args: ["run", "followup", "r", "more"],
      route: "POST /runs/r/followups",
      answer: { run_id: "r", state: "RUNNING" },
    },
    { args: ["run", "cancel", "r"], route: "POST /runs/r/cancel", answer: { run_id: "r" } },
    {
      args: ["work-item", "complete", "wi_1"],
      route: "POST /work-items/wi_1/complete",
      answer: { id: "wi_1", stage: "complete" },
    },
    { args: ["inbox", "resolve", "i1", "ok"], route: "POST /inbox/i1/resolve", answer: { id: "i1", resolution: "ok" } },
    { args: ["secret", "set", "T", "v"], route: "POST /secrets", answer: { name: "T", type: "generic" } },
  ];
  for (const c of cases) {
    it(`prints the answer of ${c.args.slice(0, 2).join(" ")}`, async () => {
      const r = await runCli(["-o", "json", ...c.args], { vars: KEY, handler: router({ [c.route]: json(c.answer) }) });
      expect(r.code).toBe(0);
      expect(JSON.parse(r.out)).toEqual(c.answer);
    });
  }

  it("rejects an unknown output format", async () => {
    const r = await runCli(["-o", "yaml", "factory", "list"], { vars: KEY });
    expect(r.code).toBe(1);
    expect(r.err).toBe('error: invalid --output "yaml" (want text or json)\n');
    expect(r.calls).toEqual([]);
  });
});

describe("secret", () => {
  it("creates a secret from the argument", async () => {
    const r = await runCli(["secret", "set", "TOKEN", "v1", "--type", "anthropic_api_key"], {
      vars: KEY,
      handler: router({ "POST /secrets": json({ name: "TOKEN", type: "anthropic_api_key" }, 201) }),
    });
    expect(body(r.calls[0])).toEqual({ name: "TOKEN", type: "anthropic_api_key", value: "v1", factory_id: "" });
    expect(r.out).toBe("Saved secret TOKEN (anthropic_api_key)\n");
  });

  it("updates on conflict and reads the value from stdin without the trailing newline", async () => {
    const r = await runCli(["secret", "set", "TOKEN", "-", "--factory", "fac_1"], {
      vars: KEY,
      stdin: "from-stdin\r\n",
      handler: router({
        "GET /factories/fac_1": json(factory),
        "POST /secrets": problem(409, "conflict", "exists"),
        "PUT /secrets/TOKEN": json({ name: "TOKEN", type: "generic" }),
      }),
    });
    expect(r.code).toBe(0);
    expect(body(r.calls[1])).toMatchObject({ value: "from-stdin", factory_id: "fac_1" });
    expect(r.calls[2]?.method).toBe("PUT");
    expect(new URL(r.calls[2]?.url ?? "").searchParams.get("factory_id")).toBe("fac_1");
    expect(body(r.calls[2])).toEqual({ value: "from-stdin" });
    expect(r.out).toBe("Saved secret TOKEN (generic)\n");
  });

  it("lists names only and deletes", async () => {
    const r = await runCli(["secret", "list"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /secrets": page([
          { name: "A", type: "generic", updated_at: "2026-11-22T11:59:00Z" },
          { name: "B", type: "generic", factory_id: "fac_1", updated_at: "2026-11-22T11:59:00Z" },
        ]),
      }),
    });
    expect(r.out).toBe("NAME  TYPE     SCOPE  UPDATED\nA     generic  team   1m ago\nB     generic  fac_1  1m ago\n");
    const d = await runCli(["secret", "delete", "A"], {
      vars: KEY,
      handler: router({ "DELETE /secrets/A": new Response(null, { status: 204 }) }),
    });
    expect(d.out).toBe("Deleted secret A\n");
  });

  it("rewraps through the admin route and prints the counts", async () => {
    const r = await runCli(["secret", "rewrap"], {
      vars: KEY,
      handler: router({ "POST /secrets/rewrap": json({ rewrapped: 3, skipped: 2, key_id: "k2" }) }),
    });
    expect(r.code).toBe(0);
    expect(r.calls[0]?.method).toBe("POST");
    expect(r.out).toBe("rewrapped: 3\nalready current: 2\nkey: k2\n");
  });

  it("exits with 1 when the rewrap fails", async () => {
    const r = await runCli(["secret", "rewrap"], {
      vars: KEY,
      handler: router({ "POST /secrets/rewrap": problem(403, "not_authorized", "Admin only") }),
    });
    expect(r.code).toBe(1);
    expect(r.err).toContain("not_authorized");
  });

  it("prints counts", () => {
    let out = "";
    printRewrap({ write: (c) => void (out += c) }, { rewrapped: 1, skipped: 0 });
    expect(out).toBe("rewrapped: 1\nalready current: 0\n");
  });
});

describe("webhook", () => {
  it("creates a webhook and prints the secret once", async () => {
    const r = await runCli(
      ["webhook", "create", "fac_1", "--name", "pd", "--auth-mode", "signature", "--signature-scheme", "pagerduty"],
      {
        vars: KEY,
        handler: router({
          "GET /factories/fac_1": json(factory),
          "POST /factories/fac_1/webhooks": json(
            {
              webhook: { id: "wh_1", name: "pd" },
              ingress_url: "http://srv.test/webhooks/wh_1",
              secret: "s3",
              awaiting_secret: true,
            },
            201,
          ),
        }),
      },
    );
    expect(body(r.calls[1])).toEqual({ name: "pd", auth_mode: "signature", signature_scheme: "pagerduty" });
    expect(r.out).toBe(
      "Created webhook pd (wh_1)\nIngress URL: http://srv.test/webhooks/wh_1\nSecret (shown once): s3\nSet the provider's signing secret with the dashboard or `webhook` API rotate-secret.\n",
    );
  });

  it("lists webhooks and deliveries", async () => {
    const l = await runCli(["webhook", "list", "fac_1"], {
      vars: KEY,
      handler: router({
        "GET /factories/fac_1": json(factory),
        "GET /factories/fac_1/webhooks": page([{ id: "wh_1", name: "pd", auth_mode: "token", enabled: true }]),
      }),
    });
    expect(l.out).toBe("ID    NAME  AUTH   SCHEME  ENABLED\nwh_1  pd    token          true\n");
    const d = await runCli(["webhook", "deliveries", "wh_1", "--limit", "5"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /webhooks/wh_1/deliveries": page([
          { id: "d1", status: "matched", reason: "", run_ids: ["r1", "r2"], received_at: "2026-11-22T11:59:00Z" },
        ]),
      }),
    });
    expect(new URL(d.calls[0]?.url ?? "").searchParams.get("limit")).toBe("5");
    expect(d.out).toBe("ID  STATUS   REASON  RUNS   RECEIVED\nd1  matched          r1,r2  1m ago\n");
  });
});

describe("benchmark", () => {
  it("validates repetitions", () => {
    expect(MAX_BENCHMARK_REPETITIONS).toBe(10);
    for (const n of [1, 10]) expect(() => validateRepetitions(n)).not.toThrow();
    for (const n of [0, 11, -1]) expect(() => validateRepetitions(n)).toThrow("between 1 and 10");
  });

  it("rejects repetitions outside 1..10 before contacting the server", async () => {
    for (const reps of ["0", "11", "-1"]) {
      const r = await runCli([
        "--server",
        "http://srv.test",
        "--api-key",
        "of_test",
        "benchmark",
        "run",
        "payments",
        "suite",
        "--repetitions",
        reps,
      ]);
      expect(r.code).toBe(1);
      expect(r.err).toContain("between 1 and 10");
      expect(r.calls).toHaveLength(0);
    }
  });

  it("starts a suite with configurations and scorers", async () => {
    const r = await runCli(
      ["benchmark", "run", "fac_1", "s1", "--config", "a,b", "--scorer", "x", "--repetitions", "3"],
      {
        vars: KEY,
        handler: router({
          "GET /factories/fac_1": json(factory),
          "POST /factories/fac_1/benchmarks/s1/runs": json({ id: "br_1", state: "running" }, 201),
        }),
      },
    );
    expect(body(r.calls[1])).toEqual({
      configurations: [{ name: "a" }, { name: "b" }],
      repetitions: 3,
      scorers: ["x"],
    });
    expect(r.out).toBe("Started benchmark run br_1 (running)\n");
  });

  it("lists suites and runs", async () => {
    const r = await runCli(["benchmark", "list", "fac_1"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /factories/fac_1": json(factory),
        "GET /factories/fac_1/benchmarks": page([
          { slug: "s1", name: "Suite", agent: "implement", configurations: [{}, {}], tasks: [{}] },
        ]),
        "GET /factories/fac_1/benchmark-runs": page([
          { id: "br_1", suite: "s1", state: "complete", created_at: "2026-11-22T11:59:00Z" },
        ]),
      }),
    });
    expect(r.out).toBe(
      "SUITE  NAME   AGENT      CONFIGS  TASKS\ns1     Suite  implement  2        1\n\nRUN   SUITE  STATE     CREATED\nbr_1  s1     complete  1m ago\n",
    );
  });
});

describe("worker list", () => {
  it("lists workers", async () => {
    const r = await runCli(["worker", "list"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /workers": page([
          {
            id: "w1",
            status: "online",
            kind: "microvm",
            slots: 4,
            slots_used: 1,
            version: "1.2.3",
            last_heartbeat: "2026-11-22T11:59:55Z",
          },
        ]),
      }),
    });
    expect(r.out).toBe(
      "ID  STATUS  KIND     ACTIVE  VERSION  HEARTBEAT\nw1  online  microvm  1/4     1.2.3    5s ago\n",
    );
  });
});

describe("mcp-registry", () => {
  it("lists registered and built-in servers", async () => {
    const r = await runCli(["mcp-registry", "list"], {
      vars: KEY,
      now: new Date("2026-11-22T12:00:00Z"),
      handler: router({
        "GET /mcp-servers": json({
          items: [
            {
              name: "sentry",
              url: "https://mcp.sentry.dev/mcp",
              connection: "proxy",
              updated_at: "2026-11-22T11:59:00Z",
            },
            { name: "docs", command: "npx", args: ["-y", "@acme/docs"], updated_at: "2026-11-22T11:59:00Z" },
          ],
          page_info: {},
          builtin: [{ managed_id: "linear", provider: "linear", configured: false }],
        }),
      }),
    });
    expect(r.out).toBe(
      "NAME    TRANSPORT                   MODE                        UPDATED\nsentry  https://mcp.sentry.dev/mcp  proxied                     1m ago\ndocs    npx -y @acme/docs           command                     1m ago\nlinear  built-in                    integration not configured  \n",
    );
  });

  it("adds a server with headers, env and OAuth settings", async () => {
    const r = await runCli(
      [
        "mcp-registry",
        "add",
        "notion",
        "--url",
        "https://mcp.notion.com/mcp",
        "--header",
        "Authorization=Bearer {{T}}",
        "--oauth",
        "--scope",
        "read",
        "--direct",
      ],
      {
        vars: KEY,
        handler: router({
          "POST /mcp-servers": json(
            {
              id: "m1",
              name: "notion",
              connection: "direct",
              oauth: { status: "not_connected" },
              warnings: ["private host"],
            },
            201,
          ),
        }),
      },
    );
    expect(body(r.calls[0])).toEqual({
      name: "notion",
      url: "https://mcp.notion.com/mcp",
      headers: { Authorization: "Bearer {{T}}" },
      connection: "direct",
      oauth: { scopes: ["read"], client_id: "", client_secret_name: "" },
    });
    expect(r.err).toBe("Warning: private host\n");
    expect(r.out).toBe(
      "Registered MCP server notion (oauth, not connected). Reference it with managedId: notion\nConnect it: openfactory mcp-registry connect notion\n",
    );
  });

  it("rejects a header without a name", async () => {
    const r = await runCli(["mcp-registry", "add", "x", "--url", "u", "--header", "=v"], { vars: KEY });
    expect(r.err).toBe('error: invalid --header "=v" (want NAME=VALUE)\n');
  });

  it("reads the registry and the built-in servers with one request", async () => {
    const r = await runCli(["-o", "json", "mcp-registry", "list"], {
      vars: KEY,
      handler: router({
        "GET /mcp-servers": json({
          items: [{ id: "m1", name: "sentry", url: "https://mcp.sentry.dev/mcp" }],
          page_info: {},
          builtin: [{ managed_id: "linear", provider: "linear", configured: true }],
        }),
      }),
    });
    expect(r.code).toBe(0);
    expect(r.calls.map((c) => `${c.method} ${new URL(c.url).pathname}`)).toEqual(["GET /api/v1/mcp-servers"]);
    expect(JSON.parse(r.out)).toEqual({
      mcp_servers: [{ id: "m1", name: "sentry", url: "https://mcp.sentry.dev/mcp" }],
      builtin: [{ managed_id: "linear", provider: "linear", configured: true }],
    });
  });

  it("connects, disconnects and deletes", async () => {
    const handler = router({
      "POST /mcp-servers/notion/oauth/connect": json({ authorization_url: "https://auth.test/x" }),
      "POST /mcp-servers/notion/oauth/disconnect": json({}),
      "DELETE /mcp-servers/notion": new Response(null, { status: 204 }),
    });
    expect((await runCli(["mcp-registry", "connect", "notion"], { vars: KEY, handler })).out).toBe(
      "Open this URL in a browser signed in to the dashboard:\nhttps://auth.test/x\n",
    );
    expect((await runCli(["mcp-registry", "disconnect", "notion"], { vars: KEY, handler })).out).toBe(
      "Disconnected MCP server notion\n",
    );
    expect((await runCli(["mcp-registry", "delete", "notion"], { vars: KEY, handler })).out).toBe(
      "Deleted MCP server notion\n",
    );
  });

  it("prints the OAuth URL as JSON with -o json", async () => {
    const r = await runCli(["-o", "json", "mcp-registry", "connect", "notion"], {
      vars: KEY,
      handler: router({
        "POST /mcp-servers/notion/oauth/connect": json({ authorization_url: "https://auth.test/x" }),
      }),
    });
    expect(JSON.parse(r.out)).toEqual({ authorization_url: "https://auth.test/x" });
  });

  it("describes the mode", () => {
    expect(mcpMode({ id: "1", name: "a", connection: "direct" })).toBe("direct");
    expect(mcpMode({ id: "1", name: "a", command: "x" })).toBe("command");
    expect(mcpMode({ id: "1", name: "a", oauth: { status: "not_connected" } })).toBe("oauth, not connected");
  });
});
