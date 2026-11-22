import { describe, expect, it } from "vitest";
import { OfError, type Integration } from "@openfactory/core";
import { PluginHost, pluginProviders, type HostFactory, type PluginHostBackend } from "./host";

const gitlab = (over: Partial<Integration> = {}): Integration => ({
  id: "int_1",
  team_id: "team_1",
  provider: "gitlab",
  name: "gitlab",
  config: { org: "acme", webhook_secret_ref: "GL_HOOK", base_url: "https://gl.test" },
  secret_ref: "GL_TOKEN",
  created_at: "2026-01-01T00:00:00Z",
  ...over,
});

const factories: HostFactory[] = [
  { id: "fac_1", teamId: "team_1", alias: "payments", name: "Payments" },
  { id: "fac_2", teamId: "team_2", alias: "web", name: "Web" },
];

function backend(
  ints: Integration[],
  missing: string[] = [],
): PluginHostBackend & { secretCalls: string[][]; calls: Record<string, number> } {
  const secretCalls: string[][] = [];
  const calls: Record<string, number> = { listIntegrations: 0, getFactory: 0 };
  return {
    secretCalls,
    calls,
    listTeams: async () => [
      { id: "team_1", name: "One" },
      { id: "team_2", name: "Two" },
    ],
    listIntegrations: async (teamId) => {
      calls.listIntegrations!++;
      return ints.filter((i) => teamId === "" || i.team_id === teamId);
    },
    getSecret: async (teamId, factoryId, name) => {
      secretCalls.push([teamId, factoryId, name]);
      if (missing.includes(name)) throw new OfError("resource_not_found", `secret ${name}`);
      return `value-of-${name}`;
    },
    listFactories: async (teamId) => factories.filter((f) => teamId === "" || f.teamId === teamId),
    getFactory: async (id) => {
      calls.getFactory!++;
      const f = factories.find((x) => x.id === id);
      if (!f) throw new Error("resource_not_found: factory");
      return f;
    },
    getFactoryRepos: async () => [{ codeForge: "GITLAB", owner: "acme", name: "api" }],
  };
}

const plugin = { name: "gitlab", forgeProviders: ["gitlab"], integrationProviders: ["gitlab"] };

describe("PluginHost", () => {
  it("limits integrations to the providers of the plugin", async () => {
    const h = new PluginHost(backend([gitlab(), gitlab({ id: "int_2", provider: "github" })]), { plugin });
    expect((await h.listIntegrations()).map((r) => r.id)).toEqual(["int_1"]);
    expect((await h.listIntegrations("gitlab")).map((r) => r.id)).toEqual(["int_1"]);
    await expect(h.listIntegrations("github")).rejects.toMatchObject({ code: "not_authorized" });
  });

  it("maps integration records to the plugin shape", async () => {
    const h = new PluginHost(backend([gitlab({ factory_id: "fac_1" })]), { plugin });
    expect(await h.listIntegrations("gitlab")).toEqual([
      {
        id: "int_1",
        teamId: "team_1",
        provider: "gitlab",
        name: "gitlab",
        config: { org: "acme", webhook_secret_ref: "GL_HOOK", base_url: "https://gl.test" },
        secretRef: "GL_TOKEN",
        factoryId: "fac_1",
      },
    ]);
  });

  it("scopes a call host to its team", async () => {
    const ints = [gitlab(), gitlab({ id: "int_2", team_id: "team_2" })];
    const h = new PluginHost(backend(ints), { plugin, teamId: "team_2" });
    expect((await h.listIntegrations()).map((r) => r.id)).toEqual(["int_2"]);
    expect(await h.listTeams()).toEqual([{ id: "team_2", name: "Two" }]);
    expect((await h.listFactories()).map((f) => f.id)).toEqual(["fac_2"]);
    await expect(h.getFactory("fac_1")).rejects.toMatchObject({ code: "resource_not_found" });
    await expect(h.getFactoryRepos("fac_1")).rejects.toMatchObject({ code: "resource_not_found" });
    expect(await h.getFactoryRepos("fac_2")).toHaveLength(1);
    await expect(h.getSecret("team:team_1", "GL_HOOK")).rejects.toMatchObject({ code: "not_authorized" });
  });

  it("reads a secret that an integration of the team references", async () => {
    const b = backend([gitlab()]);
    const h = new PluginHost(b, { plugin });
    expect(await h.getSecret("team:team_1", "GL_HOOK")).toBe("value-of-GL_HOOK");
    expect(await h.getSecret("team:team_1", "GL_TOKEN")).toBe("value-of-GL_TOKEN");
    expect(b.secretCalls[0]).toEqual(["team_1", "", "GL_HOOK"]);
    // A factory ID names the team and the factory.
    expect(await h.getSecret("fac_1", "GL_HOOK")).toBe("value-of-GL_HOOK");
    expect(b.secretCalls[2]).toEqual(["team_1", "fac_1", "GL_HOOK"]);
  });

  it("refuses a secret that no integration references, and base_url-style config", async () => {
    const h = new PluginHost(backend([gitlab()]), { plugin });
    await expect(h.getSecret("team:team_1", "OTHER_SECRET")).rejects.toMatchObject({ code: "not_authorized" });
    // A config value under a key that does not end in _ref does not allow a secret.
    await expect(h.getSecret("team:team_1", "https://gl.test")).rejects.toMatchObject({ code: "not_authorized" });
    await expect(h.getSecret("team:team_2", "GL_HOOK")).rejects.toMatchObject({ code: "not_authorized" });
    await expect(h.getSecret("", "GL_HOOK")).rejects.toMatchObject({ code: "not_authorized" });
    await expect(h.getSecret("team:team_1", "")).rejects.toMatchObject({ code: "not_authorized" });
  });

  it("does not let a plugin read the secrets of another provider", async () => {
    const h = new PluginHost(backend([gitlab({ provider: "github", config: { token_ref: "GH_TOKEN" } })]), { plugin });
    await expect(h.getSecret("team:team_1", "GH_TOKEN")).rejects.toMatchObject({ code: "not_authorized" });
  });

  it("uses the team of a call host when the scope is empty", async () => {
    const h = new PluginHost(backend([gitlab()]), { plugin, teamId: "team_1" });
    expect(await h.getSecret("", "GL_HOOK")).toBe("value-of-GL_HOOK");
  });

  it("expands {{NAME}} placeholders and fails on an unreadable one", async () => {
    const h = new PluginHost(backend([gitlab()]), { plugin });
    expect(await h.expandSecrets("team:team_1", "a {{GL_HOOK}} b {{ GL_TOKEN }} c")).toBe(
      "a value-of-GL_HOOK b value-of-GL_TOKEN c",
    );
    expect(await h.expandSecrets("team:team_1", "no placeholder")).toBe("no placeholder");
    await expect(h.expandSecrets("team:team_1", "{{OTHER}}")).rejects.toMatchObject({ code: "not_authorized" });
  });

  it("reads the team, the integrations and each distinct secret once per expansion", async () => {
    const b = backend([gitlab({ config: { webhook_secret_ref: "GL_HOOK", api_ref: "gl_api" } })]);
    const h = new PluginHost(b, { plugin });
    expect(await h.expandSecrets("fac_1", "{{GL_HOOK}} {{GL_TOKEN}} {{ GL_HOOK }} {{gl_api}} {{GL_HOOK}}")).toBe(
      "value-of-GL_HOOK value-of-GL_TOKEN value-of-GL_HOOK value-of-gl_api value-of-GL_HOOK",
    );
    expect(b.calls).toEqual({ listIntegrations: 1, getFactory: 1 });
    expect(b.secretCalls).toEqual([
      ["team_1", "fac_1", "GL_HOOK"],
      ["team_1", "fac_1", "GL_TOKEN"],
      ["team_1", "fac_1", "gl_api"],
    ]);
  });

  it("reads no secret when one name of the text is not allowed", async () => {
    const b = backend([gitlab()]);
    const h = new PluginHost(b, { plugin });
    await expect(h.expandSecrets("team:team_1", "{{GL_HOOK}} {{OTHER}}")).rejects.toMatchObject({
      code: "not_authorized",
    });
    expect(b.secretCalls).toEqual([]);
  });

  it("expands with the grammar of the secret store", async () => {
    // A secret_ref that is not a secret name cannot be a placeholder: the text stays.
    const b = backend([gitlab({ secret_ref: "a.b" })]);
    const h = new PluginHost(b, { plugin });
    expect(await h.expandSecrets("team:team_1", "{{a.b}} {{x/y-z}} {{\u00a0GL_HOOK}}")).toBe(
      "{{a.b}} {{x/y-z}} {{\u00a0GL_HOOK}}",
    );
    expect(b.secretCalls).toEqual([]);
  });

  it("reports every missing secret of an expansion together", async () => {
    const b = backend([gitlab({ config: { a_ref: "GL_HOOK", b_ref: "GL_B" } })], ["GL_HOOK", "GL_B"]);
    const h = new PluginHost(b, { plugin });
    await expect(h.expandSecrets("team:team_1", "{{GL_HOOK}} {{GL_TOKEN}} {{GL_B}}")).rejects.toMatchObject({
      code: "resource_not_found",
      detail: "unknown secret(s) referenced: GL_HOOK, GL_B",
    });
    // getSecret keeps the backend error.
    await expect(h.getSecret("team:team_1", "GL_B")).rejects.toMatchObject({ code: "resource_not_found" });
  });

  it("logs with the plugin name and drops nothing of the attributes", async () => {
    const lines: Array<[string, string, Record<string, unknown>]> = [];
    const h = new PluginHost(backend([]), { plugin, log: (l, m, f) => lines.push([l, m, f]) });
    await h.log("warn", "hello", { a: "b", plugin: "spoofed" });
    await h.log("bogus" as never, "x");
    expect(lines[0]).toEqual(["warn", "hello", { a: "b", plugin: "gitlab" }]);
    expect(lines[1]![0]).toBe("info");
  });

  it("collects the providers of a plugin in lower case", () => {
    expect(
      [
        ...pluginProviders({
          name: "x",
          forgeProviders: ["GitLab"],
          responderProviders: ["slack"],
          integrationProviders: ["gitlab"],
        }),
      ].sort(),
    ).toEqual(["gitlab", "slack"]);
  });
});
