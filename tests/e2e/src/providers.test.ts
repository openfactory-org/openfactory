// Live: one run on each hosted sandbox provider, then a restricted network
// policy. Acceptance scenarios 5 and 6.
//
// Per provider, set its switch and the shared variables (see live.ts):
//   OF_E2E_CLOUDFLARE_SANDBOX=1   provider cloudflare-sandbox (deployed with the control plane)
//   OF_TEST_DAYTONA_KEY           provider daytona (the key is configured on the control plane)
//   OF_TEST_BOAT_KEY              provider boatdev (the key is configured on the control plane)
//   OF_E2E_ANTHROPIC_API_KEY      model credential for the claude harness
// The restricted policy check also needs OF_E2E_ALLOWED_HOST (a host that the
// agent may reach) and OF_E2E_BLOCKED_URL (a URL that it must not reach).
import { beforeAll, expect, it } from "vitest";
import {
  api,
  describeLive,
  ensureModelSecret,
  env,
  fixture,
  renameFactory,
  uniq,
  waitFor,
  waitRunDone,
} from "./live.ts";

const PROVIDERS: { id: string; switchVar: string }[] = [
  { id: "cloudflare-sandbox", switchVar: "OF_E2E_CLOUDFLARE_SANDBOX" },
  { id: "daytona", switchVar: "OF_TEST_DAYTONA_KEY" },
  { id: "boatdev", switchVar: "OF_TEST_BOAT_KEY" },
];

for (const p of PROVIDERS) {
  describeLive([p.switchVar, "OF_E2E_ANTHROPIC_API_KEY"], `provider ${p.id}`, () => {
    beforeAll(async () => {
      await ensureModelSecret();
    });

    it(
      "runs a foreman turn and answers",
      async () => {
        const name = uniq(`e2e-${p.id}`);
        const created = await api<{ factory: { uid: string } }>("POST", "/factories", {
          files: renameFactory(fixture("def"), name),
        });
        const run = await api<{ run_id: string }>("POST", "/runs", {
          prompt: "Reply with exactly the text E2E-PROVIDER-OK and nothing else.",
          factory_id: created.factory.uid,
          agent_name: "foreman",
          config: { worker_host: p.id },
        });
        await waitRunDone(run.run_id);
        const msgs = await api<{ messages: { role: string; content: string }[] }>(
          "GET",
          `/runs/${run.run_id}/messages`,
        );
        expect(msgs.messages.some((m) => m.role === "assistant" && m.content.includes("E2E-PROVIDER-OK"))).toBe(true);
      },
      30 * 60_000,
    );
  });

  describeLive(
    [p.switchVar, "OF_E2E_ANTHROPIC_API_KEY", "OF_E2E_ALLOWED_HOST", "OF_E2E_BLOCKED_URL"],
    `provider ${p.id}: restricted network policy`,
    () => {
      beforeAll(async () => {
        await ensureModelSecret();
      });

      it(
        "reaches the allowed host; a blocked request shows the egress diagnostic",
        async () => {
          const name = uniq(`e2e-net-${p.id}`);
          const files = renameFactory(fixture("net", { ALLOWED_HOST: env("OF_E2E_ALLOWED_HOST") }), name);
          const created = await api<{ factory: { uid: string } }>("POST", "/factories", { files });
          const run = await api<{ run_id: string }>("POST", "/runs", {
            prompt:
              `Run: curl -sS -m 10 -o /dev/null -w '%{http_code}' https://${env("OF_E2E_ALLOWED_HOST")}/ ; ` +
              `then curl -sS -m 10 ${env("OF_E2E_BLOCKED_URL")} . Report both results.`,
            factory_id: created.factory.uid,
            agent_name: "foreman",
            config: { worker_host: p.id },
          });
          await waitRunDone(run.run_id);
          const diag = await waitFor(
            "network/egress_blocked diagnostic",
            async () => {
              const d = await api<unknown>("GET", `/runs/${run.run_id}/diagnostics`);
              return JSON.stringify(d).includes("egress_blocked") ? d : undefined;
            },
            5 * 60_000,
          );
          expect(JSON.stringify(diag)).toContain("egress_blocked");
        },
        30 * 60_000,
      );
    },
  );
}
