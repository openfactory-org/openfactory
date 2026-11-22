import { describe, expect, it } from "vitest";
import { toPluginError } from "@openfactory/plugin-api";
import { mockFetch } from "@openfactory/plugin-api/testing";
import { AzureDevOpsClient } from "./client";
import { pickState, type TypeState } from "./tracker";

const agile: TypeState[] = [
  { name: "New", category: "Proposed" },
  { name: "Active", category: "InProgress" },
  { name: "In Review", category: "InProgress" },
  { name: "Resolved", category: "Resolved" },
  { name: "Closed", category: "Completed" },
  { name: "Removed", category: "Removed" },
];
const basic: TypeState[] = [
  { name: "To Do", category: "Proposed" },
  { name: "Doing", category: "InProgress" },
  { name: "Done", category: "Completed" },
];

describe("pickState", () => {
  it.each([
    ["start", "in_progress", agile, "New", "Active"],
    ["already active", "in_progress", agile, "Active", ""],
    ["never go back from review", "in_progress", agile, "In Review", ""],
    ["review state", "in_review", agile, "Active", "In Review"],
    ["review from new", "in_review", agile, "New", "In Review"],
    ["no review state keeps in progress", "in_review", basic, "To Do", "Doing"],
    ["no review state leaves it", "in_review", basic, "Doing", ""],
    ["review of a closed item", "in_review", agile, "Closed", ""],
    ["done", "done", agile, "In Review", "Closed"],
    ["done twice", "done", agile, "Closed", ""],
    ["done after removal", "done", agile, "Removed", ""],
    ["done in the basic process", "done", basic, "Doing", "Done"],
  ])("%s", (_name, state, states, current, want) => {
    expect(pickState(state as string, states as TypeState[], current as string)).toBe(want);
  });
});

interface Patch {
  contentType: string | null;
  ops: Array<Record<string, unknown>>;
}

function tracker(current: string, relations: unknown) {
  const patches: Patch[] = [];
  const m = mockFetch({
    "GET /contoso/proj/_apis/wit/workitems/7": () => ({
      id: 7,
      fields: { "System.WorkItemType": "User Story", "System.State": current },
      relations,
    }),
    "GET /contoso/proj/_apis/wit/workitemtypes/User%20Story/states": () => ({ value: agile }),
    "GET /contoso/proj/_apis/git/repositories/web": () => ({ id: "repo-1", project: { id: "proj-1" } }),
    "PATCH /contoso/proj/_apis/wit/workitems/7": (call) => {
      patches.push({ contentType: call.headers.get("Content-Type"), ops: call.body as Patch["ops"] });
      return {};
    },
  });
  const c = new AzureDevOpsClient({ organization: "contoso", baseUrl: "https://ado.test", token: "pat", fetch: m.fetch });
  return { c, patches };
}

describe("work item tracker", () => {
  it("sets the state with a JSON Patch document", async () => {
    const { c, patches } = tracker("In Review", null);
    await c.setWorkItemState("proj", 7, "done");
    expect(patches).toHaveLength(1);
    expect(patches[0]!.contentType).toBe("application/json-patch+json");
    expect(patches[0]!.ops[0]).toEqual({ op: "add", path: "/fields/System.State", value: "Closed" });
    // In progress does not move a work item that is in review.
    await c.setWorkItemState("proj", 7, "in_progress");
    expect(patches).toHaveLength(1);
    await expect(c.setWorkItemState("proj", 7, "later")).rejects.toThrow(/unknown work item state/);
  });

  it("attaches a pull request once", async () => {
    const pr = { forge: "AZURE_DEVOPS", repo: "proj/web", number: 11 };
    const first = tracker("Active", null);
    await first.c.attachPullRequestToWorkItem("proj", 7, pr);
    expect(first.patches).toHaveLength(1);
    expect(first.patches[0]!.ops[0]).toMatchObject({
      op: "add",
      path: "/relations/-",
      value: { rel: "ArtifactLink", url: "vstfs:///Git/PullRequestId/proj-1%2Frepo-1%2F11", attributes: { name: "Pull Request" } },
    });
    const second = tracker("Active", [{ rel: "ArtifactLink", url: "vstfs:///Git/PullRequestId/proj-1%2Frepo-1%2F11" }]);
    await second.c.attachPullRequestToWorkItem("proj", 7, pr);
    expect(second.patches).toHaveLength(0);
    const err = await first.c.attachPullRequestToWorkItem("proj", 7, { forge: "AZURE_DEVOPS", repo: "proj/web", number: 0 }).catch((e: unknown) => e);
    expect(toPluginError(err).code).toBe("invalid_request");
    expect((err as Error).message).toContain("number");
  });
});
