import { afterEach, describe, expect, it, vi } from "vitest";
import { toOfError } from "@openfactory/core";
import { PLUGIN_API_VERSION, checkPluginInfo } from "@openfactory/plugin-api";
import { mockFetch } from "@openfactory/plugin-api/testing";
import { Plugin, clientFor } from "./index";

const target = {
  provider: "azure_devops",
  integrationId: "int_ado",
  teamId: "team_1",
  config: { org: "contoso", base_url: "https://ado.test" },
  token: "pat",
  userToken: false,
};

const plugin = (env = {}) => new Plugin({} as ExecutionContext, env);

afterEach(() => vi.unstubAllGlobals());

describe("Plugin", () => {
  it("reports its info and passes the version check", async () => {
    const info = await plugin({ PLUGIN_VERSION: "2.0.0" }).info();
    expect(info).toMatchObject({
      name: "azuredevops",
      version: "2.0.0",
      apiVersion: PLUGIN_API_VERSION,
      roles: ["forge", "ingress"],
      forgeProviders: ["azure_devops"],
    });
    expect(checkPluginInfo(info)).toEqual({ ok: true });
    expect(JSON.parse(info.configSchema!).required).toEqual(["org"]);
  });

  it("names the forge and takes the organization from the target", async () => {
    expect(await plugin().name(target)).toBe("AZURE_DEVOPS");
    expect(clientFor(target).organization).toBe("contoso");
    expect(() => clientFor({ ...target, config: {} })).toThrow(/organization/);
  });

  it("throws operation_not_supported for issue comments", async () => {
    const e = await plugin()
      .commentIssue(target, "p", "r", 1, "x")
      .catch((x: unknown) => x);
    expect(toOfError(e).error.code).toBe("operation_not_supported");
    expect((e as Error).message.startsWith("operation_not_supported")).toBe(true);
  });

  it("calls Azure DevOps with global fetch and gives errors a code", async () => {
    const m = mockFetch({
      "GET /contoso/proj/_apis/git/repositories/web": () => ({ defaultBranch: "refs/heads/trunk" }),
      "GET /contoso/proj/_apis/git/repositories/web/pullrequests/8": () => new Response("busy", { status: 503 }),
    });
    vi.stubGlobal("fetch", m.fetch);
    expect(await plugin().defaultBranch(target, "proj", "web")).toBe("trunk");
    const err = await plugin()
      .getPullRequest(target, "proj", "web", 8)
      .catch((e: unknown) => e);
    expect(toOfError(err).error.code).toBe("resource_unavailable");
  });
});
