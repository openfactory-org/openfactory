import { describe, expect, it } from "vitest";
import { isNotSupported } from "../src/errors";
import { ApiError } from "../src/http";
import type { ForgeTarget, PluginInfo } from "../src/types";
import { ForgePluginWorker, pluginInfo, type ForgeClient, type ForgeSpec } from "../src/worker";

const target: ForgeTarget = {
  provider: "fake",
  integrationId: "int_1",
  teamId: "team_1",
  config: {},
  token: "tok",
  userToken: false,
};

const calls: string[] = [];
let built = 0;

const client = (): ForgeClient => {
  built++;
  const rec =
    (name: string) =>
    async (...args: unknown[]): Promise<void> => {
      calls.push(`${name}(${args.join(",")})`);
    };
  return {
    cloneUrl: (o, r) => `https://git.test/${o}/${r}.git`,
    defaultBranch: async () => "main",
    createPullRequest: async (_o, r, pr) => ({ forge: "FAKE", repo: r, number: 7, title: pr.title }),
    getPullRequest: async (_o, r, n) => ({ forge: "FAKE", repo: r, number: n }),
    comment: rec("comment"),
    commentWorkItem: rec("commentWorkItem"),
    setCommitStatus: rec("setCommitStatus"),
    getFiles: async () => ({ files: { "a.yaml": "x" }, commitSha: "abc" }),
    replyInThread: rec("replyInThread"),
    resolveThread: rec("resolveThread"),
    submitReview: rec("submitReview"),
    markReady: rec("markReady"),
    requestReviewers: rec("requestReviewers"),
    login: async () => {
      throw new ApiError("GET", "https://git.test/user", 401, "bad token");
    },
    reviewThread: async () => ({ comments: [] }) as never,
    fileDiff: async () => ({ path: "a", patch: "" }) as never,
  };
};

class Fake extends ForgePluginWorker {
  protected readonly forge: ForgeSpec = {
    name: "FAKE",
    label: "fake",
    client,
    unsupported: ["setWorkItemState"],
  };

  async info(): Promise<PluginInfo> {
    return pluginInfo({ name: "fake", roles: ["forge"] });
  }
}

const plugin = () => new Fake({} as ExecutionContext, {});

describe("ForgePluginWorker", () => {
  it("delegates each forge method to the client", async () => {
    calls.length = 0;
    const p = plugin();
    expect(await p.name(target)).toBe("FAKE");
    expect(await p.cloneUrl(target, "o", "r")).toBe("https://git.test/o/r.git");
    expect(await p.defaultBranch(target, "o", "r")).toBe("main");
    expect(await p.getPullRequest(target, "o", "r", 3)).toMatchObject({ number: 3 });
    await p.comment(target, "o", "r", 3, "hi");
    await p.commentWorkItem(target, "proj", 9, "note");
    await p.markReady(target, "o", "r", 3);
    expect(calls).toEqual(["comment(o,r,3,hi)", "commentWorkItem(proj,9,note)", "markReady(o,r,3)"]);
  });

  it("maps client errors to OfError codes", async () => {
    const e = await plugin()
      .login(target)
      .catch((x: unknown) => x);
    expect((e as Error).message).toMatch(/^external_authentication_required/);
  });

  it.each([
    ["listed in unsupported", (p: Fake) => p.setWorkItemState(target, "proj", 1, "done"), false],
    ["missing on the client", (p: Fake) => p.commentIssue(target, "o", "r", 1, "x"), true],
    [
      "missing on the client (attach)",
      (p: Fake) => p.attachPullRequestToWorkItem(target, "proj", 1, { forge: "FAKE", repo: "r", number: 1 }),
      true,
    ],
  ])("throws operation_not_supported for a method %s", async (_name, call, buildsClient) => {
    built = 0;
    const e = await call(plugin()).catch((x: unknown) => x);
    expect(isNotSupported(e)).toBe(true);
    expect((e as Error).message).toMatch(/^operation_not_supported: fake: /);
    expect(built > 0).toBe(buildsClient);
  });
});
