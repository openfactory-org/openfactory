// Pure parts of the MCP port: merge guard (internal/mergeguard tests),
// registry validation (TestValidate, TestConnection), reference parsing
// (TestParsePullRequestURL), the JSON-RPC handler and the envelope format.
import { describe, expect, it } from "vitest";
import { env } from "cloudflare:workers";
import {
  MCP_CONNECTION_DIRECT,
  MCP_CONNECTION_PROXY,
  MCP_OAUTH_NOT_CONNECTED,
  isOfError,
  type RegisteredMCPServer,
} from "@openfactory/core";
import { checkRedirectURI, redirectAllowed } from "./auth/core";
import { localEncrypter } from "../secrets/envelope";
import { handleMessage, serveMcp, textResult, type McpServer } from "./jsonrpc";
import { MERGE_REFUSED_HEADER, REFUSAL_TEXT, guardMerge, isMergeTool } from "./mergeguard";
import { authParam, canonicalResource } from "./registry/oauth";
import { defaultConnection, parseConnection, privateHostWarning, proxied, validate } from "./registry/validate";
import { matchesWorkItem, parsePullRequestURL, planReference, truncate } from "./server/refs";

describe("mergeguard", () => {
  it("isMergeTool", () => {
    const merge = [
      "merge_pull_request",
      "mergePullRequest",
      "forge.merge-pr",
      "merge_pr",
      "merge",
      "merge_merge_request",
      "accept_merge_request",
      "acceptMergeRequest",
      "complete_pull_request",
      "pull_request_merge",
      "pr_merge",
      "squash_merge",
      "enable_auto_merge",
      "enable_pull_request_automerge",
      "set_auto_merge",
      "merge_branch",
      "mcp__forge__merge_pull_request",
      "MergePR",
      "merge_and_close",
      "add_to_merge_queue",
      "enqueue_pull_request",
      "merge_into",
      "rebase_merge",
    ];
    for (const n of merge) expect(isMergeTool(n), n).toBe(true);
    const other = [
      "get_merge_request",
      "list_merge_requests",
      "create_merge_request",
      "create_pull_request",
      "get_pull_request",
      "merge_request_diff",
      "get_merge_status",
      "list_merges_in_queue_status",
      "get_merge_base",
      "linear_get_issue",
      "comment_on_merge_request",
      "update_merge_request",
      "emerge",
      "submerged_items",
      "",
      "merge_request",
    ];
    for (const n of other) expect(isMergeTool(n), n).toBe(false);
  });

  it("refuses merges, passes other calls, refuses a batch with a merge", async () => {
    const records: string[] = [];
    let allow = false;
    const policy = {
      allow: () => allow,
      record: (tool: string, ok: boolean) => void records.push(`${tool}:${ok ? "allowed" : "refused"}`),
    };
    const merge = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"merge_pull_request","arguments":{"pull_number":1}}}`;
    const res = await guardMerge(merge, policy);
    expect(res?.status).toBe(200);
    expect(res?.headers.get(MERGE_REFUSED_HEADER)).toBe("true");
    const body = (await res!.json()) as { id: number; result: { isError: boolean; content: { text: string }[] } };
    expect(body.id).toBe(7);
    expect(body.result.isError).toBe(true);
    expect(body.result.content[0]!.text).toBe(REFUSAL_TEXT);

    expect(
      await guardMerge(
        `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"create_pull_request"}}`,
        policy,
      ),
    ).toBeNull();
    expect(await guardMerge(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`, policy)).toBeNull();

    const batch =
      `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_pull_request"}},` +
      `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"enable_auto_merge"}}]`;
    const br = await guardMerge(batch, policy);
    const list = (await br!.json()) as { id: number; error?: { code: number }; result?: { isError: boolean } }[];
    expect(list).toHaveLength(2);
    expect(list[0]!.error?.code).toBe(-32600);
    expect(list[1]!.result?.isError).toBe(true);

    allow = true;
    expect(await guardMerge(merge, policy)).toBeNull();
    expect(records).toEqual(["merge_pull_request:refused", "enable_auto_merge:refused", "merge_pull_request:allowed"]);
  });

  it("a refused notification gets 202", async () => {
    const res = await guardMerge(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"merge"}}`, {});
    expect(res?.status).toBe(202);
  });
});

const srv = (m: Partial<RegisteredMCPServer>): RegisteredMCPServer => ({
  id: "",
  team_id: "",
  name: "",
  created_at: "",
  updated_at: "",
  ...m,
});

function expectInvalid(m: RegisteredMCPServer, label: string): void {
  try {
    validate(m);
  } catch (e) {
    expect(isOfError(e, "invalid_request"), label).toBe(true);
    return;
  }
  throw new Error(`${label}: want invalid`);
}

describe("registry validate", () => {
  it("rejects bad entries (TestValidate)", () => {
    const bad: Partial<RegisteredMCPServer>[] = [
      { name: "", url: "https://a.test" },
      { name: "builtin-linear", url: "https://a.test" },
      { name: "a", url: "https://a.test", command: "x" },
      { name: "a" },
      { name: "a", url: "ftp://a.test" },
      { name: "a", url: "https://u:p@a.test" },
      { name: "a", url: "https://a.test", env: { A: "1" } },
      { name: "a", command: "x", headers: { A: "1" } },
      { name: "a", command: "x", connection: "proxy" },
      { name: "a", url: "https://a.test/sse", connection: "proxy" },
      { name: "a", url: "https://a.test", headers: { "X-Api-Key": "abc" } },
      { name: "a", command: "x", env: { GITHUB_TOKEN: "ghp_x" } },
      { name: "a", url: "https://a.test", oauth: { status: "not_connected", token_endpoint: "http://idp.test/token" } },
      { name: "a", url: "https://a.test", oauth: { status: "not_connected", client_secret_name: "S" } },
      { name: "a", url: "https://a.test", oauth: { status: "not_connected", scopes: ["a b"] } },
    ];
    bad.forEach((b, i) => expectInvalid(srv(b), `case ${i}`));
    const ok = srv({
      name: "a",
      url: "https://a.test/mcp",
      headers: { Authorization: "Bearer {{TOKEN}}", "X-Team": "eng" },
      oauth: { status: "not_connected", token_endpoint: "http://127.0.0.1:9/token" },
    });
    validate(ok);
    expect(proxied(ok)).toBe(true);
    expect(ok.connection).toBe(MCP_CONNECTION_PROXY);
    expect(ok.oauth?.status).toBe(MCP_OAUTH_NOT_CONNECTED);
  });

  it("connection modes (TestConnection)", () => {
    const cases: [string, Partial<RegisteredMCPServer>, string | undefined, boolean][] = [
      ["default proxy", { name: "a", url: "https://a.test/mcp" }, MCP_CONNECTION_PROXY, false],
      ["explicit direct", { name: "a", url: "https://a.test/mcp", connection: "direct" }, MCP_CONNECTION_DIRECT, false],
      ["legacy sse", { name: "a", url: "https://a.test/sse" }, MCP_CONNECTION_DIRECT, true],
      ["command", { name: "a", command: "npx" }, undefined, false],
    ];
    for (const [name, inp, want, warning] of cases) {
      const m = srv(inp);
      const w = defaultConnection(m);
      validate(m);
      expect(m.connection, name).toBe(want);
      expect(proxied(m), name).toBe(want === MCP_CONNECTION_PROXY);
      expect(w !== "", name).toBe(warning);
    }
    expect(() => parseConnection("tunnel")).toThrow(/connection "tunnel": want proxy or direct/);
    expect(parseConnection(" Proxy ")).toBe(MCP_CONNECTION_PROXY);
    expect(parseConnection("")).toBeUndefined();
    const invalid: Partial<RegisteredMCPServer>[] = [
      { name: "a", url: "https://a.test/sse", connection: "proxy" },
      { name: "a", url: "https://a.test/mcp", connection: "direct", oauth: { status: "not_connected" } },
      { name: "a", command: "npx", connection: "proxy" },
    ];
    for (const b of invalid) expectInvalid(srv(b), JSON.stringify(b));
  });

  it("warns about a proxied server on a private network", () => {
    for (const host of ["10.0.0.5", "192.168.1.2", "mcp.local", "localhost", "172.20.0.1"]) {
      const m = srv({ name: "a", url: `http://${host}/mcp`, connection: "proxy" });
      validate(m);
      expect(privateHostWarning(m), host).not.toBe("");
    }
    const pub = srv({ name: "a", url: "https://mcp.example.test/mcp", connection: "proxy" });
    validate(pub);
    expect(privateHostWarning(pub)).toBe("");
    const direct = srv({ name: "a", url: "http://10.0.0.5/mcp", connection: "direct" });
    validate(direct);
    expect(privateHostWarning(direct)).toBe("");
  });
});

describe("oauth helpers", () => {
  it("authParam and canonicalResource", () => {
    expect(
      authParam(
        [`Bearer resource_metadata="https://x.test/.well-known/oauth-protected-resource/mcp"`],
        "resource_metadata",
      ),
    ).toBe("https://x.test/.well-known/oauth-protected-resource/mcp");
    expect(authParam([`Bearer realm="a", error="invalid_token"`], "error")).toBe("invalid_token");
    expect(canonicalResource("HTTPS://MCP.Example.test/mcp?x=1#f")).toBe("https://mcp.example.test/mcp");
    expect(canonicalResource("https://mcp.example.test")).toBe("https://mcp.example.test");
  });

  it("redirect URI rules (TestRegistrationRules)", () => {
    expect(checkRedirectURI("http://127.0.0.1:8080/cb")).toBe("");
    expect(checkRedirectURI("https://app.example.test/cb")).toBe("");
    expect(checkRedirectURI("com.example.agent://oauth/callback")).toBe("");
    expect(checkRedirectURI("http://evil.example.test/cb")).not.toBe("");
    expect(checkRedirectURI("javascript:alert(1)")).not.toBe("");
    expect(checkRedirectURI("https://a.test/cb#frag")).not.toBe("");
    const c = { client_id: "c", redirect_uris: ["http://localhost/cb"], created_at: "" };
    expect(redirectAllowed(c, "http://localhost:5555/cb")).toBe(true);
    expect(redirectAllowed(c, "http://localhost:5555/other")).toBe(false);
    expect(redirectAllowed(c, "http://127.0.0.1:5555/cb")).toBe(false);
  });
});

describe("references (TestParsePullRequestURL, TestResolveExternalReferences)", () => {
  it("parses pull and merge request URLs", () => {
    expect(parsePullRequestURL("https://github.com/acme/web/pull/42")).toMatchObject({
      forge: "GITHUB",
      repo: "acme/web",
      number: 42,
    });
    expect(parsePullRequestURL("https://gitlab.com/group/sub/proj/-/merge_requests/7/")).toMatchObject({
      forge: "GITLAB",
      repo: "group/sub/proj",
      number: 7,
      url: "https://gitlab.com/group/sub/proj/-/merge_requests/7",
    });
    expect(parsePullRequestURL("https://dev.azure.com/org/proj/_git/repo/pullrequest/9")).toMatchObject({
      forge: "AZURE_DEVOPS",
      repo: "org/proj/repo",
      number: 9,
    });
    expect(parsePullRequestURL("https://github.com/acme/web/issues/42")).toBeNull();
    expect(parsePullRequestURL("not a url")).toBeNull();
  });

  it("plans references", () => {
    expect(planReference("acme/web#12", "")).toEqual({
      kind: "search",
      keys: ["github:acme/web#12", "gitlab:acme/web#12"],
      prURL: "",
      branch: "",
    });
    expect(planReference("ENG-42", "")).toEqual({
      kind: "search",
      keys: ["linear:ENG-42", "jira:ENG-42"],
      prURL: "",
      branch: "",
    });
    expect(planReference("feature/x", "")).toEqual({ kind: "search", keys: [], prURL: "", branch: "feature/x" });
    expect(planReference("https://linear.app/acme/issue/ENG-1/title", "")).toMatchObject({ keys: ["linear:ENG-1"] });
    expect(planReference("https://acme.atlassian.net/browse/OPS-3", "")).toMatchObject({ keys: ["jira:OPS-3"] });
    expect(planReference("https://acme.slack.com/archives/C0123/p1700000000123456", "")).toMatchObject({
      keys: ["slack:C0123:1700000000.123456"],
    });
    expect(planReference("https://github.com/acme/web/issues/5", "")).toMatchObject({ keys: ["github:acme/web#5"] });
    expect(planReference("https://factory.test/factories/f/work/wi_1", "https://factory.test")).toEqual({
      kind: "dashboard",
      workItems: ["wi_1"],
      runs: [],
    });
    expect(planReference("https://example.test/nothing", "")).toEqual({ kind: "notFound" });
    const wi = {
      id: "w",
      source_key: "",
      pull_requests: [
        { forge: "GITHUB", repo: "acme/web", number: 3, url: "https://github.com/acme/web/pull/3", branch: "b1" },
      ],
    };
    expect(matchesWorkItem(wi, [], "https://github.com/acme/web/pull/3/", "")).toBe(true);
    expect(matchesWorkItem(wi, ["github:acme/web#3"], "", "")).toBe(true);
    expect(matchesWorkItem(wi, [], "", "b1")).toBe(true);
    expect(matchesWorkItem(wi, [], "", "b2")).toBe(false);
  });
});

describe("jsonrpc", () => {
  const server: McpServer<{ who: string }> = {
    info: { name: "t", version: "1" },
    instructions: "hi",
    tools: [
      {
        name: "echo",
        description: "Echo",
        inputSchema: { type: "object" },
        handler: (a, c) => textResult(`${c.who}:${String(a.x)}`),
      },
      {
        name: "boom",
        description: "Fails",
        inputSchema: { type: "object" },
        handler: () => {
          throw new Error("bad");
        },
      },
    ],
  };
  it("initialize, list, call, errors, notifications, batches", async () => {
    const init = await handleMessage(
      server,
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18" } },
      { who: "a" },
    );
    expect(init?.result).toMatchObject({
      protocolVersion: "2025-06-18",
      serverInfo: { name: "t" },
      instructions: "hi",
    });
    const old = await handleMessage(
      server,
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "1999-01-01" } },
      { who: "a" },
    );
    expect((old?.result as { protocolVersion: string }).protocolVersion).toBe("2025-11-25");
    expect(
      await handleMessage(server, { jsonrpc: "2.0", method: "notifications/initialized" }, { who: "a" }),
    ).toBeNull();
    const call = await handleMessage(
      server,
      { jsonrpc: "2.0", id: "x", method: "tools/call", params: { name: "echo", arguments: { x: 5 } } },
      { who: "a" },
    );
    expect(call?.result).toEqual({ content: [{ type: "text", text: "a:5" }] });
    const boom = await handleMessage(
      server,
      { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "boom" } },
      { who: "a" },
    );
    expect(boom?.result).toEqual({ content: [{ type: "text", text: "bad" }], isError: true });
    const unknown = await handleMessage(
      server,
      { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "nope" } },
      { who: "a" },
    );
    expect(unknown?.error?.code).toBe(-32602);
    const missing = await handleMessage(server, { jsonrpc: "2.0", id: 4, method: "nope" }, { who: "a" });
    expect(missing?.error?.code).toBe(-32601);

    const post = (body: string) =>
      new Request("http://x/mcp", { method: "POST", body, headers: { "content-type": "application/json" } });
    const r1 = await serveMcp(
      post(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`),
      server,
      { who: "a" },
    );
    expect(await r1.json()).toEqual([{ jsonrpc: "2.0", id: 1, result: {} }]);
    expect(
      (await serveMcp(post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), server, { who: "a" })).status,
    ).toBe(202);
    expect((await serveMcp(post("{nope"), server, { who: "a" })).status).toBe(400);
    expect((await serveMcp(new Request("http://x/mcp"), server, { who: "a" })).status).toBe(405);
  });
});

describe("envelope", () => {
  it("round-trips and binds the name", async () => {
    const enc = await localEncrypter(env);
    const sealed = await enc.encrypt(
      "team1",
      "",
      "mcp-oauth:mcp_1",
      "mcp_oauth_token",
      new TextEncoder().encode("secret-value") as Uint8Array<ArrayBuffer>,
    );
    expect(sealed[0]).toBe(2);
    expect(new TextDecoder().decode(await enc.decrypt("team1", "", "mcp-oauth:mcp_1", "mcp_oauth_token", sealed))).toBe(
      "secret-value",
    );
    await expect(enc.decrypt("team2", "", "mcp-oauth:mcp_1", "mcp_oauth_token", sealed)).rejects.toThrow();
    await expect(enc.decrypt("team1", "", "mcp-oauth:mcp_2", "mcp_oauth_token", sealed)).rejects.toThrow();
  });
});

describe("truncate", () => {
  it.each([
    ["short", "abc", 5, "abc"],
    ["exact", "abcde", 5, "abcde"],
    ["ascii cut", "abcdef", 3, "abc… (truncated)"],
    // "é" is 2 bytes: a cut inside it drops the partial character.
    ["split character", "aé", 2, "a… (truncated)"],
    ["whole character", "aéb", 3, "aé… (truncated)"],
    // A real U+FFFD before the cut stays.
    ["replacement character kept", "a\ufffdbc", 4, "a\ufffd… (truncated)"],
  ])("%s", (_name, s, n, want) => {
    expect(truncate(s, n)).toBe(want);
  });
});
