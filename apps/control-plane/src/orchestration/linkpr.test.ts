// link_pull_request: the pull request of a URL or of a repo and number
// (TestParsePullRequestURL and the link_pull_request part of
// TestHumanAndWorkItemTools in internal/orchestration/server_test.go; merge
// rules of engine.LinkPullRequest), and the normalized reference it adds.
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";
import { buildRuns, resultText, settle, withFx } from "./fixture.test-util";
import { callTool } from "./index";
import { parsePullRequestUrl, prRef } from "./linkpr";

afterEach(settle);

describe("parsePullRequestUrl", () => {
  const cases: [string, { forge: string; repo: string; number: number } | undefined][] = [
    ["https://github.com/acme/api/pull/42", { forge: "GITHUB", repo: "acme/api", number: 42 }],
    ["https://github.com/acme/api/pull/42/files", { forge: "GITHUB", repo: "acme/api", number: 42 }],
    ["https://gitlab.com/g/sub/proj/-/merge_requests/9", { forge: "GITLAB", repo: "g/sub/proj", number: 9 }],
    [
      "https://dev.azure.com/org/proj/_git/repo/pullrequest/7",
      { forge: "AZURE_DEVOPS", repo: "org/proj/repo", number: 7 },
    ],
    ["https://example.com/nothing", undefined],
    ["not a url", undefined],
  ];
  it.each(cases)("%s", (url, want) => {
    expect(parsePullRequestUrl(url)).toEqual(want);
  });
});

describe("prRef", () => {
  it("fills forge, repo and number from the URL; explicit fields win", () => {
    expect(prRef({ url: "https://gitlab.com/g/sub/proj/-/merge_requests/9" })).toEqual({
      forge: "GITLAB",
      repo: "g/sub/proj",
      number: 9,
      url: "https://gitlab.com/g/sub/proj/-/merge_requests/9",
      branch: "",
      state: "",
    });
    expect(prRef({ url: "https://github.com/acme/api/pull/4", repo: "acme/other", forge: "github" })).toMatchObject({
      forge: "GITHUB",
      repo: "acme/other",
      number: 4,
    });
    expect(prRef({ repo: "acme/api", number: 3, state: "Draft" }).state).toBe("draft");
  });

  it("refuses an unknown state and a call without url or repo and number", () => {
    expect(() => prRef({ url: "https://github.com/a/b/pull/1", state: "shipped" })).toThrow(/unknown state/);
    expect(() => prRef({ repo: "acme/api" })).toThrow(/url \(or repo and number\) is required/);
  });
});

describe("link_pull_request tool", () => {
  it("stores the pull request and its normalized reference; a second link updates it", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { type: "FOREMAN", workItem: "wi", state: "running" } },
    );
    const url = "https://gitlab.com/g/sub/proj/-/merge_requests/9";
    const res = await callTool(env, "default", f.ids.root!, "link_pull_request", { url });
    expect(res.isError, resultText(res)).toBeFalsy();
    expect(JSON.parse(resultText(res))).toEqual({ linked: "gitlab:g/sub/proj#9" });
    let wi = await withFx(f.token, (fx) => fx.repo.getWorkItem(f.ids.wi!)!);
    expect(wi.pull_requests).toEqual([
      { forge: "GITLAB", repo: "g/sub/proj", number: 9, url, branch: "", state: "open" },
    ]);
    expect(wi.references).toHaveLength(1);
    expect(wi.references![0]).toMatchObject({
      kind: "pull_request",
      key: "gitlab:g/sub/proj#9",
      source: { provider: "gitlab", kind: "pull_request", repo: "g/sub/proj", number: 9, url },
    });

    // The same pull request by repo and number (any case): merged, not added.
    const again = await callTool(env, "default", f.ids.root!, "link_pull_request", {
      repo: "G/Sub/Proj",
      number: 9,
      forge: "GITLAB",
      branch: "feat",
      state: "merged",
    });
    expect(again.isError, resultText(again)).toBeFalsy();
    wi = await withFx(f.token, (fx) => fx.repo.getWorkItem(f.ids.wi!)!);
    expect(wi.pull_requests).toHaveLength(1);
    expect(wi.pull_requests![0]).toMatchObject({ repo: "G/Sub/Proj", branch: "feat", state: "merged", url });
    expect(wi.references).toHaveLength(1);
  });

  it("a URL of an unknown forge is stored without a reference", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { type: "FOREMAN", workItem: "wi", state: "running" } },
    );
    const url = "https://git.example.com/review/1";
    const res = await callTool(env, "default", f.ids.root!, "link_pull_request", { url });
    expect(res.isError, resultText(res)).toBeFalsy();
    const wi = await withFx(f.token, (fx) => fx.repo.getWorkItem(f.ids.wi!)!);
    expect(wi.pull_requests).toEqual([{ forge: "", repo: "", number: 0, url, branch: "", state: "open" }]);
    expect(wi.references ?? []).toHaveLength(0);
  });

  it("refuses bad input and a run without a work item", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { type: "FOREMAN", workItem: "wi", state: "running" }, solo: { state: "running" } },
    );
    const bad = await callTool(env, "default", f.ids.root!, "link_pull_request", { repo: "acme/api" });
    expect(bad.isError).toBe(true);
    expect(resultText(bad)).toContain("url (or repo and number) is required");
    const solo = await callTool(env, "default", f.ids.solo!, "link_pull_request", {
      url: "https://github.com/acme/api/pull/1",
    });
    expect(solo.isError).toBe(true);
    expect(resultText(solo)).toContain("no work item");
  });
});
