// Live: GitHub App issue → foreman → pull request. Acceptance scenario 5
// (forge part).
//
//   OF_E2E_GITHUB_OWNER, OF_E2E_GITHUB_REPO  the test repository; the GitHub App
//                                            of the control plane is installed on it
//   OF_E2E_GITHUB_TOKEN                      a token that can create issues and labels
//                                            in that repository (stored as the team
//                                            secret GITHUB_TOKEN for the agent)
//   OF_E2E_ANTHROPIC_API_KEY                 model credential for the claude harness
import { beforeAll, expect, it } from "vitest";
import { api, describeLive, ensureModelSecret, env, fixture, renameFactory, uniq, waitFor } from "./live.ts";
import type { Loose } from "./loose.ts";

async function github<T = Loose>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`https://api.github.com${path}`, {
    method,
    headers: {
      authorization: `Bearer ${env("OF_E2E_GITHUB_TOKEN")}`,
      accept: "application/vnd.github+json",
      "x-github-api-version": "2022-11-28",
      "user-agent": "openfactory-e2e",
      ...(body === undefined ? {} : { "content-type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (res.status >= 300) throw new Error(`GitHub ${method} ${path}: ${res.status}: ${text.slice(0, 300)}`);
  return (text ? JSON.parse(text) : undefined) as T;
}

describeLive(
  ["OF_E2E_GITHUB_OWNER", "OF_E2E_GITHUB_REPO", "OF_E2E_GITHUB_TOKEN", "OF_E2E_ANTHROPIC_API_KEY"],
  "GitHub issue to pull request",
  () => {
    let factoryUid = "";
    let factoryName = "";

    beforeAll(async () => {
      await ensureModelSecret();
      await api(
        "POST",
        "/secrets",
        { name: "GITHUB_TOKEN", type: "generic", value: env("OF_E2E_GITHUB_TOKEN") },
        [201, 409],
      );
      factoryName = uniq("e2e-gh");
      const files = renameFactory(
        fixture("gh", { GITHUB_OWNER: env("OF_E2E_GITHUB_OWNER"), GITHUB_NAME: env("OF_E2E_GITHUB_REPO") }),
        factoryName,
      );
      const created = await api<{ factory: { uid: string } }>("POST", "/factories", { files });
      factoryUid = created.factory.uid;
    });

    it(
      "a labeled issue starts a work item that links a pull request",
      async () => {
        const repo = `/repos/${env("OF_E2E_GITHUB_OWNER")}/${env("OF_E2E_GITHUB_REPO")}`;
        const marker = uniq("E2E-GH");
        const issue = await github<{ number: number }>("POST", `${repo}/issues`, {
          title: `${marker}: add a file`,
          body: `Create the file e2e/${marker}.txt with the content ${marker}, open a pull request and link it to the work item.`,
        });
        // The factory label routes the issue (the trigger is issue_labeled).
        await github("POST", `${repo}/issues/${issue.number}/labels`, { labels: [factoryName] });

        const wi = await waitFor(
          "work item with a pull request",
          async () => {
            const items = await api<{ items: { id: string; title?: string; pull_requests?: { url?: string }[] }[] }>(
              "GET",
              `/factories/${factoryUid}/work-items`,
            );
            return items.items.find((w) => (w.title ?? "").includes(marker) && (w.pull_requests?.length ?? 0) > 0);
          },
          30 * 60_000,
          10_000,
        );
        expect(wi.pull_requests?.[0]?.url).toMatch(/\/pull\/\d+/);
      },
      35 * 60_000,
    );
  },
);
