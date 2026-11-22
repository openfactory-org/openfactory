import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { DO_BINDINGS, REQUIRED_SECRET_NAMES, VAR_NAMES } from "../src/contracts/env";

describe("Env matches wrangler.jsonc", () => {
  it("has every var and required secret", () => {
    const e = env as unknown as Record<string, unknown>;
    for (const name of [...VAR_NAMES, ...REQUIRED_SECRET_NAMES]) expect(typeof e[name], name).toBe("string");
  });

  it("has every DO, R2, Queue, assets and service binding", () => {
    const e = env as unknown as Record<string, { getByName?: unknown }>;
    for (const name of Object.keys(DO_BINDINGS)) expect(typeof e[name]?.getByName, name).toBe("function");
    for (const name of ["DATA", "JOBS", "PROVIDER_CLOUDFLARE_SANDBOX", "PLUGIN_GITLAB", "PLUGIN_AZUREDEVOPS"]) {
      expect(e[name], name).toBeDefined();
    }
  });

  it("reaches the provider binding over RPC", async () => {
    expect(await env.PROVIDER_CLOUDFLARE_SANDBOX.id()).toBe("cloudflare-sandbox");
  });
});
