// Port of internal/api/environments_test.go: environment validation on
// create and update, and the in-use check of DELETE for environments and
// MCP servers that an active definition references.
import { describe, expect, it } from "vitest";
import { bootstrap, call } from "./helpers";

const uniq = () => crypto.randomUUID().slice(0, 6);

describe("environments (internal/api environments_test ports)", () => {
  it("validates repos, image and env_vars, and refuses a taken name", async () => {
    const { key } = await bootstrap();
    const web = "web-" + uniq();
    expect((await call("POST", "/api/v1/environments", { key, body: { name: web } })).status).toBe(201);
    const n = () => "env-" + uniq();
    const cases: { name: string; body: Record<string, unknown>; status: number; detail: string }[] = [
      { name: "https repo", body: { name: n(), repos: ["https://github.com/acme/web"] }, status: 201, detail: "" },
      {
        name: "https repo with .git",
        body: { name: n(), repos: ["https://gitlab.example.com/acme/platform/billing.git"] },
        status: 201,
        detail: "",
      },
      {
        name: "forge repo",
        body: { name: n(), repos: ["GITLAB:acme/platform/billing", "acme/api"] },
        status: 201,
        detail: "",
      },
      { name: "repo without owner", body: { name: "x", repos: ["nope"] }, status: 400, detail: "repo" },
      {
        name: "url without path",
        body: { name: "x", repos: ["https://github.com/acme"] },
        status: 400,
        detail: "repo",
      },
      { name: "ftp url", body: { name: "x", repos: ["ftp://example.com/acme/web"] }, status: 400, detail: "repo" },
      {
        name: "image",
        body: { name: n(), image: "registry.example.com:5000/team/tools:1.2" },
        status: 201,
        detail: "",
      },
      {
        name: "image with digest",
        body: { name: n(), image: "node@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" },
        status: 201,
        detail: "",
      },
      { name: "image with space", body: { name: "x", image: "node 22" }, status: 400, detail: "image" },
      { name: "image uppercase path", body: { name: "x", image: "Node:22" }, status: 400, detail: "image" },
      { name: "image bad tag", body: { name: "x", image: "node:" }, status: 400, detail: "image" },
      { name: "env var", body: { name: n(), env_vars: { NODE_ENV: "test", _x1: "y" } }, status: 201, detail: "" },
      {
        name: "env var with dash",
        body: { name: "x", env_vars: { "NODE-ENV": "test" } },
        status: 400,
        detail: "NODE-ENV",
      },
      { name: "env var with digit first", body: { name: "x", env_vars: { "1A": "b" } }, status: 400, detail: "1A" },
      {
        name: "reserved env var",
        body: { name: "x", env_vars: { OF_METADATA_TEAM: "b" } },
        status: 400,
        detail: "reserved",
      },
      {
        name: "reserved env var lowercase",
        body: { name: "x", env_vars: { of_run_id: "b" } },
        status: 400,
        detail: "reserved",
      },
      {
        name: "duplicate name",
        body: { name: web },
        status: 409,
        detail: `an environment named "${web}" already exists`,
      },
      {
        name: "duplicate name other case",
        body: { name: ` ${web.toUpperCase()} ` },
        status: 409,
        detail: "already exists",
      },
    ];
    for (const c of cases) {
      const r = await call("POST", "/api/v1/environments", { key, body: c.body });
      expect(r.status, `${c.name}: ${JSON.stringify(r.body)}`).toBe(c.status);
      if (c.detail) expect(r.body.detail, c.name).toContain(c.detail);
    }

    // PATCH validates the same fields and refuses a taken name.
    const otherName = "other-" + uniq();
    const other = await call("POST", "/api/v1/environments", { key, body: { name: otherName } });
    const path = `/api/v1/environments/${other.body.id}`;
    expect((await call("PATCH", path, { key, body: { image: "not an image" } })).status).toBe(400);
    expect((await call("PATCH", path, { key, body: { env_vars: { OF_X: "1" } } })).status).toBe(400);
    expect((await call("PATCH", path, { key, body: { name: web } })).status).toBe(409);
    const ok = await call("PATCH", path, { key, body: { name: otherName, repos: ["https://github.com/acme/web"] } });
    expect(ok.status, JSON.stringify(ok.body)).toBe(200);
    expect(ok.body.repos).toEqual(["https://github.com/acme/web"]);
  });

  it("refuses to delete an environment or MCP server that an agent uses", async () => {
    const { key } = await bootstrap();
    const envName = "qa-env-" + uniq();
    const envId = (await call("POST", "/api/v1/environments", { key, body: { name: envName } })).body.id as string;
    const free = (await call("POST", "/api/v1/environments", { key, body: { name: "free-" + uniq() } })).body.id;
    const mcpName = "docs-" + uniq();
    const mcp = await call("POST", "/api/v1/mcp-servers", { key, body: { name: mcpName, command: "docs-mcp" } });
    expect(mcp.status, JSON.stringify(mcp.body)).toBe(201);

    const fname = "qa-" + uniq();
    const f = (await call("POST", "/api/v1/factories", { key, body: { name: fname } })).body.factory;
    const defPath = `/api/v1/factories/${f.id}/definition`;
    const files = (await call("GET", defPath, { key })).body.files as Record<string, string>;
    const foreman = files["agents/foreman/agent.md"]!;
    const withRefs = foreman.replace(
      /^---\n/,
      `---\nenvironmentId: ${envId}\nmcpServers:\n  docs:\n    managedId: ${mcpName}\n`,
    );
    const put = await call("PUT", defPath, { key, body: { files: { ...files, "agents/foreman/agent.md": withRefs } } });
    expect(put.status, JSON.stringify(put.body)).toBe(200);

    const r = await call("DELETE", `/api/v1/environments/${envId}`, { key });
    expect(r.status).toBe(409);
    expect(r.body.detail).toBe(
      `Environment ${envName} is used by agent foreman of factory ${fname}. Remove the reference first.`,
    );
    expect((await call("GET", `/api/v1/environments/${envId}`, { key })).status).toBe(200);
    const m = await call("DELETE", `/api/v1/mcp-servers/${mcp.body.id}`, { key });
    expect(m.status).toBe(409);
    expect(m.body.detail).toBe(
      `MCP server ${mcpName} is used by agent foreman of factory ${fname}. Remove the reference first.`,
    );
    expect((await call("DELETE", `/api/v1/environments/${free}`, { key })).status).toBe(204);

    // Without the reference the delete succeeds.
    const back = await call("PUT", defPath, { key, body: { files } });
    expect(back.status, JSON.stringify(back.body)).toBe(200);
    expect((await call("DELETE", `/api/v1/environments/${envId}`, { key })).status).toBe(204);
    expect((await call("DELETE", `/api/v1/mcp-servers/${mcpName}`, { key })).status).toBe(204);
  });

  it("names at most five places in the in-use error", async () => {
    const { inUseError } = await import("../src/do/team/settings");
    expect(inUseError("Environment", "x", [])).toBeUndefined();
    expect(inUseError("Environment", "x", ["a", "b", "c", "d", "e", "f", "g"])?.detail).toBe(
      "Environment x is used by a, b, c, d, e and 2 more. Remove the references first.",
    );
  });

  it("finds the uses of environments and MCP servers in a definition (TestResourceUses)", async () => {
    const { environmentUses } = await import("../src/do/team/settings");
    const { mcpServerUsesOf } = await import("../src/do/team/mcp");
    const def = {
      mcpServers: { wiki: { managedId: "mcp_wiki" } },
      agents: [
        { name: "foreman", environment_id: "env_1", mcp_servers: { wiki: { managedId: "mcp_wiki" } } },
        { name: "coder", mcp_servers: { wiki: { managedId: "mcp_wiki" }, docs: { managedId: "docs" } } },
      ],
      automations: [
        { name: "nightly", overrides: { environment_id: "env_1" } },
        { name: "weekly", overrides: { environment_id: "env_2", mcp_servers: { d: { managedId: "mcp_docs" } } } },
      ],
      scorers: [{ slug: "quality", mcp_servers: { d: { managedId: "docs" } } }],
      scorerDefaults: { environment_id: "env_1" },
    } as unknown as Parameters<typeof environmentUses>[0];
    const cases: [string, string[], string][] = [
      ["environment", environmentUses(def, "env_1"), "agent foreman, automation nightly, scorerDefaults"],
      ["unused environment", environmentUses(def, "env_9"), ""],
      ["factory-wide server once", mcpServerUsesOf(def, "mcp_wiki", "wiki"), "the factory-wide mcpServers"],
      [
        "server by name or id",
        mcpServerUsesOf(def, "mcp_docs", "docs"),
        "agent coder, automation weekly, scorer quality",
      ],
      ["unused server", mcpServerUsesOf(def, "mcp_x", "x"), ""],
    ];
    for (const [name, got, want] of cases) expect(got.join(", "), name).toBe(want);
  });
});
