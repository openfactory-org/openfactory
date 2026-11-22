import { describe, expect, it } from "vitest";
import type { Env } from "../../contracts/env";
import { providerFor } from "./providers";

describe("providerFor cloudflare-sandbox", () => {
  it("forwards ack to the service binding", async () => {
    const acked: string[] = [];
    const env = {
      PROVIDER_CLOUDFLARE_SANDBOX: { ack: async (id: string) => void acked.push(id) },
    } as unknown as Env;
    const p = providerFor(env, "default", "cloudflare-sandbox") as unknown as { ack(id: string): Promise<void> };
    expect(typeof p.ack).toBe("function");
    await p.ack("task_1");
    expect(acked).toEqual(["task_1"]);
  });
});
