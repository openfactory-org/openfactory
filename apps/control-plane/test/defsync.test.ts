// The git definition sync through the TeamDO ops that the GitHub webhook and
// the cron call (internal/server/defsync_test.go: TestSyncAppliesNewCommits,
// TestSyncKeepsLastValidVersion, TestOnPushMatchesRepoAndBranch,
// TestCheckPullRequestPublishesCheckRun).
import { env } from "cloudflare:workers";
import { doName } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta, Principal } from "../src/contracts/do-messages";
import { setDefSyncRunner, setGitSourceChecker, type DefSyncRunner } from "../src/do/team/factories";
import { defSyncRunner } from "../src/wiring";
import { FakeResolver } from "./fakes/forge";

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Loose = any;

function team() {
  const id = "ds" + crypto.randomUUID().slice(0, 8);
  const admin: Principal = {
    kind: "user",
    id: "usr_admin",
    teamId: id,
    role: "admin",
    factoryRoles: {},
    userId: "usr_admin",
  };
  const meta: CallMeta = { requestId: "req_test", principal: admin };
  const stub = env.TEAM.getByName(doName.team(id));
  const op = async (name: string, params: Record<string, string> = {}, body: unknown = null): Promise<Loose> => {
    let out = (await stub.api(meta, name, params, body)) as Loose;
    if (out && typeof out === "object" && "$status" in out) out = out.body;
    if (out && typeof out === "object" && "$error" in out) throw new Error(`${name}: ${JSON.stringify(out.$error)}`);
    return out;
  };
  return { op };
}

let fr: FakeResolver;
let prevRunner: DefSyncRunner | undefined;
let prevChecker: ReturnType<typeof setGitSourceChecker>;
beforeEach(() => {
  fr = new FakeResolver();
  prevRunner = setDefSyncRunner(defSyncRunner(() => fr));
  prevChecker = setGitSourceChecker(undefined);
});
afterEach(() => {
  setDefSyncRunner(prevRunner);
  setGitSourceChecker(prevChecker);
});

/** A git-backed factory on acme/defs and the valid files of its branch. */
async function gitFactory(t: ReturnType<typeof team>) {
  const m = await t.op("createFactory", {}, { name: "Seed " + crypto.randomUUID().slice(0, 6) });
  const files = (await t.op("getDefinition", { uid: m.factory.id })).files as Record<string, string>;
  await t.op("deleteFactory", { uid: m.factory.id });
  const f = await t.op("createFactory", {}, { name: "Defs", source: "git", git_repo: "github.com/acme/defs" });
  return { id: f.factory.id as string, files };
}

describe("git definition sync", () => {
  it("a push applies a new commit once; an invalid commit keeps the last valid version", async () => {
    const t = team();
    const { id, files } = await gitFactory(t);
    const fg = fr.forgeFor("GITHUB");
    fg.files = files;
    fg.sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    // A push to another branch or repository does nothing.
    await t.op("defsyncPush", {}, { repo: "acme/defs", branch: "dev", sha: fg.sha });
    await t.op("defsyncPush", {}, { repo: "acme/other", branch: "main", sha: fg.sha });
    expect((await t.op("getFactory", { uid: id })).active_version_id ?? "").toBe("");

    await t.op("defsyncPush", {}, { repo: "acme/defs", branch: "main", sha: fg.sha });
    const f1 = await t.op("getFactory", { uid: id });
    expect(f1.active_version_id).toBeTruthy();
    const v1 = await t.op("getDefinition", { uid: id });
    expect(v1.version).toMatchObject({ commit_sha: fg.sha, message: "Sync main @ aaaaaaa" });
    // The same commit again: no new version.
    await t.op("defsyncAll");
    expect((await t.op("getFactory", { uid: id })).active_version_id).toBe(f1.active_version_id);

    fg.files = { "factory.yaml": "name: [" };
    fg.sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
    await t.op("defsyncPush", {}, { repo: "acme/defs", branch: "main", sha: fg.sha });
    expect((await t.op("getFactory", { uid: id })).active_version_id).toBe(f1.active_version_id);
    const getFiles = fg.callsOf("getFiles").length;
    // An invalid commit is not tried again.
    await t.op("defsyncAll");
    expect(fg.callsOf("getFiles").length).toBe(getFiles + 1);
    expect((await t.op("getFactory", { uid: id })).active_version_id).toBe(f1.active_version_id);
  });

  it("a definition pull request gets a check run with the resource diff", async () => {
    const t = team();
    const { id, files } = await gitFactory(t);
    const fg = fr.forgeFor("GITHUB");
    fg.files = files;
    await t.op("defsyncPush", {}, { repo: "acme/defs", branch: "main", sha: fg.sha });
    fg.files = { ...files, "agents/extra/agent.md": "---\ndescription: d\nagentType: IMPLEMENT\n---\nGo.\n" };
    await t.op(
      "defsyncPullRequest",
      {},
      {
        teamId: "x",
        factoryId: id,
        repo: "acme/defs",
        number: 7,
        headSha: "c".repeat(40),
        headRef: "feat",
        baseRef: "main",
        checkName: "openfactory/definition",
        changedPaths: ["agents/extra/agent.md"],
      },
    );
    const runs = fg.callsOf("publishCheckRun").map((c) => c.args[2] as Loose);
    expect(runs.map((r) => r.status)).toEqual(["in_progress", "completed"]);
    expect(runs[1]).toMatchObject({ conclusion: "success", headSha: "c".repeat(40) });
    expect(runs[1].title).toMatch(/Definition is valid: 1 added/);
  });

  it("factoryConfigChecks defaults to main", async () => {
    const t = team();
    const { id } = await gitFactory(t);
    expect((await t.op("defsyncConfigChecks", {}, { factoryId: id })).mode).toBe("main");
  });
});
