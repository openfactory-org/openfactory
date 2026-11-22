import { afterEach, describe, expect, it, vi } from "vitest";
import { OfError, toOfError } from "@openfactory/core";
import { PLUGIN_API_VERSION, checkPluginInfo } from "@openfactory/plugin-api";
import { FakeHostApi, mockFetch, webhookRequest } from "@openfactory/plugin-api/testing";
import { Plugin, clientFor } from "./index";

const target = {
  provider: "gitlab",
  integrationId: "int_gl",
  teamId: "team_1",
  config: { base_url: "https://gl.test", token_type: "oauth", user_token_type: "private" },
  token: "glpat",
  userToken: false,
};

const plugin = (env = {}) => new Plugin({} as ExecutionContext, env);

afterEach(() => vi.unstubAllGlobals());

describe("Plugin", () => {
  it("reports its info and passes the version check", async () => {
    const info = await plugin({ PLUGIN_VERSION: "1.2.3" }).info();
    expect(info).toMatchObject({ name: "gitlab", version: "1.2.3", apiVersion: PLUGIN_API_VERSION, roles: ["forge", "ingress"], forgeProviders: ["gitlab"] });
    expect(checkPluginInfo(info)).toEqual({ ok: true });
    expect(() => JSON.parse(info.configSchema!)).not.toThrow();
    expect((await plugin().info()).version).toBe("0.0.0-dev");
    await expect(plugin().ping()).resolves.toBeUndefined();
  });

  it("names the forge", async () => {
    expect(await plugin().name(target)).toBe("GITLAB");
  });

  it("picks the token type by the kind of token", async () => {
    const m = mockFetch({ "GET /api/v4/user": () => ({ username: "u" }) });
    await clientFor(target, m.fetch).login();
    await clientFor({ ...target, userToken: true }, m.fetch).login();
    expect(m.calls[0]!.headers.get("Authorization")).toBe("Bearer glpat");
    expect(m.calls[1]!.headers.get("PRIVATE-TOKEN")).toBe("glpat");
  });

  it("throws operation_not_supported for the work item methods", async () => {
    for (const call of [
      () => plugin().commentWorkItem(target, "p", 1, "x"),
      () => plugin().setWorkItemState(target, "p", 1, "done"),
      () => plugin().attachPullRequestToWorkItem(target, "p", 1, { forge: "GITLAB", repo: "a/b", number: 1 }),
    ]) {
      const e = await call().catch((x: unknown) => x);
      expect(e).toBeInstanceOf(Error);
      expect((e as Error).message.startsWith("operation_not_supported")).toBe(true);
    }
  });

  it("calls GitLab with global fetch and returns plain data", async () => {
    const m = mockFetch({
      "GET /api/v4/projects/acme%2Fapi": () => ({ default_branch: "trunk" }),
      "GET /api/v4/projects/acme%2Fapi/merge_requests/8": () => new Response("gone", { status: 404 }),
    });
    vi.stubGlobal("fetch", m.fetch);
    expect(await plugin().defaultBranch(target, "acme", "api")).toBe("trunk");
    const err = await plugin().getPullRequest(target, "acme", "api", 8).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect(toOfError(err).error.code).toBe("resource_not_found");
  });

  it("handles a webhook through the entrypoint", async () => {
    const host = new FakeHostApi({
      integrations: [{ id: "i", teamId: "t", provider: "gitlab", name: "g", config: { org: "acme", webhook_secret_ref: "S" } }],
      secrets: { t: { S: "tok" } },
    });
    const req = webhookRequest(
      "https://cp.test/plugins/gitlab/webhook",
      { object_kind: "push", ref: "refs/heads/m", after: "abc", user_username: "a", project: { path_with_namespace: "acme/api" } },
      { "X-Gitlab-Token": "tok" },
    );
    const r = await plugin().handleWebhook(req, host);
    expect(r.status).toBe(202);
    expect(r.events[0]!.type).toBe("push");
  });
});
