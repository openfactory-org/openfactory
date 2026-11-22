// link_issue: the issue of a URL or of a provider and key
// (internal/orchestration/linkissue_test.go), and the reference it adds.
import { env } from "cloudflare:workers";
import { sourceKey } from "@openfactory/core";
import { afterEach, describe, expect, it } from "vitest";
import { buildRuns, resultText, settle, withFx } from "./fixture.test-util";
import { callTool } from "./index";
import { issueRef, parseIssueUrl } from "./linkissue";

afterEach(settle);

describe("parseIssueUrl", () => {
  const cases: [string, string, string][] = [
    ["https://linear.app/acme/issue/ENG-123/fix-invoices", "linear:ENG-123", "issue"],
    ["https://acme.atlassian.net/browse/OPS-7", "jira:OPS-7", "issue"],
    ["https://acme.atlassian.net/jira/software/projects/OPS/boards/1?selectedIssue=OPS-9", "jira:OPS-9", "issue"],
    ["https://github.com/acme/api/issues/12", "github:acme/api#12", "issue"],
    ["https://gitlab.com/acme/group/api/-/issues/5", "gitlab:acme/group/api#5", "issue"],
    ["https://dev.azure.com/acme/Payments/_workitems/edit/42", "azure_devops:Payments#42", "work_item"],
  ];
  it.each(cases)("%s", (url, key, kind) => {
    const src = parseIssueUrl(url);
    expect(src).toBeDefined();
    expect(sourceKey(src!)).toBe(key);
    expect(src!.kind).toBe(kind);
  });

  it.each(["https://example.com/nothing/here", "not a url"])("refuses %s", (url) => {
    expect(parseIssueUrl(url)).toBeUndefined();
  });
});

describe("issueRef", () => {
  it("normalizes the provider and requires one", () => {
    expect(sourceKey(issueRef({ provider: "Jira", key: "OPS-1" }))).toBe("jira:OPS-1");
    expect(sourceKey(issueRef({ provider: "azure-devops", repo: "Payments", number: 3 }))).toBe(
      "azure_devops:Payments#3",
    );
    expect(issueRef({ provider: "azure-devops", repo: "Payments", number: 3 }).kind).toBe("work_item");
    expect(() => issueRef({ key: "OPS-1" })).toThrow(/provider/);
    expect(() => issueRef({ url: "https://example.com/x" })).toThrow(/cannot read an issue/);
  });
});

describe("link_issue tool", () => {
  it("adds the issue with its source key as a reference of the work item", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      {
        root: { type: "FOREMAN", workItem: "wi", state: "running" },
      },
    );
    const res = await callTool(env, "default", f.ids.root!, "link_issue", {
      url: "https://linear.app/acme/issue/eng-123/fix",
    });
    expect(res.isError, resultText(res)).toBeFalsy();
    expect(JSON.parse(resultText(res))).toEqual({ linked: "linear:ENG-123" });
    const refs = await withFx(f.token, (fx) => fx.repo.getWorkItem(f.ids.wi!)!.references ?? []);
    expect(refs).toHaveLength(1);
    expect(refs[0]).toMatchObject({
      kind: "issue",
      key: "linear:ENG-123",
      source: { provider: "linear", kind: "issue", external_id: "ENG-123" },
    });
    // A key without a provider is refused before anything is stored.
    const bad = await callTool(env, "default", f.ids.root!, "link_issue", { key: "ENG-9" });
    expect(bad.isError).toBe(true);
    expect(resultText(bad)).toContain("provider");
  });

  it("refuses a run without a work item", async () => {
    const f = await buildRuns({}, { solo: { state: "running" } });
    const res = await callTool(env, "default", f.ids.solo!, "link_issue", { provider: "jira", key: "OPS-1" });
    expect(res.isError).toBe(true);
    expect(resultText(res)).toContain("no work item");
  });
});
