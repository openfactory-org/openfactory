// Port of internal/eventctx/review_test.go: the review context builder reads
// the review thread and the file diff of a review comment from the forge.
import { describe, expect, it } from "vitest";
import { OfError, type Event, type FileDiff, type ReviewThread, type ThreadComment } from "@openfactory/core";
import { GitHubClient } from "../forges/github/client";
import type { Forge, ForgeLookup, ForgeResolver } from "../forges/forge";
import {
  build,
  decode,
  encode,
  MAX_REVIEW_FILE_BYTES,
  promptSection,
  redact,
  reviewBuilders,
  threadFile,
} from "./index";
import type { Bundle } from "./index";

const DIR = ".openfactory/events/m/context/";
const dec = new TextDecoder();

/** Serves one forge client and records the lookup. */
class Source implements ForgeResolver {
  lookups: ForgeLookup[] = [];
  constructor(
    private readonly f?: Partial<Forge>,
    private readonly err?: Error,
  ) {}
  async forge(l: ForgeLookup): Promise<Forge> {
    this.lookups.push(l);
    if (this.err) throw this.err;
    return this.f as Forge;
  }
}

/** A forge with canned review context. */
class Reader {
  calls = 0;
  constructor(
    public thread?: ReviewThread,
    public diff?: FileDiff,
    public err?: Error,
  ) {}
  async reviewThread(): Promise<ReviewThread> {
    this.calls++;
    if (this.err) throw this.err;
    return this.thread!;
  }
  async fileDiff(): Promise<FileDiff> {
    this.calls++;
    if (this.err) throw this.err;
    return this.diff!;
  }
}

function reviewEvent(): Event {
  return {
    id: "e1",
    team_id: "team_1",
    factory_id: "fac_1",
    provider: "github",
    type: "pull_request_commented",
    source: { provider: "github", kind: "pull_request", repo: "acme/api", number: 7, thread_ref: "100" },
    actor: { provider: "github", external_id: "1", handle: "rev" },
    received_at: "2026-01-02T03:04:05Z",
    payload: {
      comment: {
        id: 101,
        body: "please rename",
        path: "pkg/x.go",
        line: 4,
        diff_hunk: "@@ -3,2 +3,2 @@\n-a\n+b",
        user: { login: "rev" },
      },
    },
  } as unknown as Event;
}

/** A GitHub client whose fetch serves the review comments and the files of pull request 7. */
function githubFake(status = 0): GitHubClient {
  const ok = (v: unknown, s = 200) =>
    new Response(JSON.stringify(v), { status: s, headers: { "content-type": "application/json" } });
  const fetch = async (input: Request | string | URL): Promise<Response> => {
    if (status) return ok({ message: "Resource not accessible" }, status);
    const u = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    switch (u.pathname) {
      case "/repos/acme/api/pulls/7/comments":
        return ok([
          {
            id: 100,
            body: "rename this",
            path: "pkg/x.go",
            line: 4,
            created_at: "2026-01-02T03:04:05Z",
            user: { login: "rev" },
          },
          { id: 200, body: "other thread", path: "pkg/y.go", line: 1, user: { login: "zed" } },
          { id: 300, body: "binary?", path: "big.bin", line: 1, user: { login: "zed" } },
          {
            id: 101,
            in_reply_to_id: 100,
            body: "please rename",
            path: "pkg/x.go",
            line: 4,
            created_at: "2026-01-02T04:00:00Z",
            user: { login: "dana" },
          },
        ]);
      case "/repos/acme/api/pulls/7/files":
        return ok([
          { filename: "pkg/y.go", patch: "@@ -1 +1 @@\n-q\n+r" },
          { filename: "pkg/x.go", patch: "@@ -1,5 +1,5 @@\n one\n two\n three\n-four\n+FOUR\n five" },
          { filename: "big.bin" },
        ]);
    }
    return ok({ message: "Not Found" }, 404);
  };
  return new GitHubClient({ baseURL: "https://gh.test", token: "t", fetch });
}

const fileMap = (b: Bundle | null) => Object.fromEntries((b?.files ?? []).map((f) => [f.path, dec.decode(f.data)]));
const omitted = (b: Bundle | null) => Object.fromEntries((b?.envelope.omitted ?? []).map((o) => [o.name, o.reason]));

describe("review context builder", () => {
  it("GitHub: reads the thread and the diff with the factory credential", async () => {
    const src = new Source(githubFake());
    const b = await build("m", reviewEvent(), true, reviewBuilders(src));
    const files = fileMap(b);
    const th = JSON.parse(files[DIR + "review_thread.json"]!) as ReviewThread;
    expect(th.ref).toBe("100");
    expect(th.path).toBe("pkg/x.go");
    expect(th.comments!.map((c) => c.author)).toEqual(["rev", "dana"]);
    const diff = files[DIR + "file_diff.patch"]!;
    expect(diff.startsWith("--- a/pkg/x.go\n+++ b/pkg/x.go\n@@ -1,5 +1,5 @@\n")).toBe(true);
    expect(diff).toContain("+FOUR");
    expect(diff).not.toContain("-q");
    // The payload builder still gives the hunk of the comment.
    expect(files[DIR + "diff.patch"]).toContain("+b");
    expect(files[DIR + "review_comment.json"]).toContain("please rename");
    expect(b!.envelope.omitted ?? []).toEqual([]);
    // The credential of the factory serves the call, scoped to the repository.
    expect(src.lookups[0]).toMatchObject({ factoryId: "fac_1", codeForge: "GITHUB", scope: ["acme/api"] });
    const sec = promptSection(b);
    expect(sec).toContain("context/review_thread.json");
    expect(sec).toContain("context/file_diff.patch");
  });

  it("lists what it cannot read and goes on", async () => {
    // The forge refuses the credential: both files are left out.
    let b = await build("m", reviewEvent(), true, reviewBuilders(new Source(githubFake(403))));
    let o = omitted(b);
    expect(o["context/review_thread.json"]).toContain("403");
    expect(o["context/file_diff.patch"]).toContain("403");
    expect(fileMap(b)[".openfactory/events/m/payload.json"]).toBeDefined();
    expect(promptSection(b)).toContain("Left out: context/review_thread.json");

    // A file without a patch: the thread is there, the diff is left out.
    const ev = reviewEvent();
    ev.source.thread_ref = "300";
    b = await build("m", ev, true, reviewBuilders(new Source(githubFake())));
    o = omitted(b);
    expect(fileMap(b)[DIR + "review_thread.json"]).toBeDefined();
    expect(o["context/file_diff.patch"]).toContain("has no patch");
    expect(Object.keys(o)).toHaveLength(1);

    // No forge client for the team.
    b = await build(
      "m",
      reviewEvent(),
      true,
      reviewBuilders(new Source(undefined, new Error("no code-forge integration configured"))),
    );
    o = omitted(b);
    expect(o["context/review_thread.json"]).toContain("no forge client: no code-forge integration");
    expect(Object.keys(o)).toHaveLength(2);

    // No forge source at all.
    b = await build("m", reviewEvent(), true, reviewBuilders(null));
    expect(omitted(b)["context/review_thread.json"]).toContain("no forge client");

    // A forge that cannot read review context (generic git).
    b = await build("m", reviewEvent(), true, reviewBuilders(new Source({})));
    expect(omitted(b)["context/file_diff.patch"]).toContain("cannot read review threads or diffs");

    // A forge that answers operation_not_supported for each call.
    const rf = new Reader(undefined, undefined, new OfError("operation_not_supported", "no"));
    b = await build("m", reviewEvent(), true, reviewBuilders(new Source(rf)));
    expect(omitted(b)["context/review_thread.json"]).toContain("this forge cannot read the review thread");
  });

  it("does not call the forge for a comment without a thread", async () => {
    const rf = new Reader();
    const src = new Source(rf);
    const evs = [
      { provider: "github", type: "issue_commented", source: { kind: "issue", repo: "acme/api", number: 3 } },
      {
        provider: "github",
        type: "pull_request_commented",
        source: { kind: "pull_request", repo: "acme/api", number: 3 },
      },
      { provider: "gitlab", type: "note", source: { kind: "issue", repo: "acme/api", number: 3, thread_ref: "d1" } },
      { provider: "slack", type: "message", source: { thread_ref: "1.2", repo: "a/b", number: 1 } },
    ];
    for (const e of evs) {
      const ev = { ...e, id: "x", team_id: "t", actor: { provider: e.provider }, received_at: "" } as unknown as Event;
      const b = await build("m", ev, true, reviewBuilders(src));
      expect(b!.envelope.omitted ?? [], `${e.provider} ${e.type}`).toEqual([]);
      expect(b!.envelope.context_files ?? [], `${e.provider} ${e.type}`).toEqual([]);
    }
    expect(rf.calls).toBe(0);
  });

  it("size limits: the diff is cut at a line end, older comments are left out", async () => {
    let patch = "@@ -1,9000 +1,9000 @@\n";
    const line = "+" + "é".repeat(40) + "\n";
    while (new TextEncoder().encode(patch).length < 600 << 10) patch += line;
    const comments: ThreadComment[] = [];
    for (let i = 0; i < 8; i++) comments.push({ id: String(i), author: "a", body: "x".repeat(50 << 10) });
    const rf = new Reader({ ref: "100", path: "pkg/x.go", comments }, { path: "pkg/x.go", patch });
    let b = await build("m", reviewEvent(), true, reviewBuilders(new Source(rf)));
    const raw = Object.fromEntries(b!.files.map((f) => [f.path, f.data]));
    const diff = raw[DIR + "file_diff.patch"]!;
    const thread = raw[DIR + "review_thread.json"]!;
    expect(diff.length).toBeLessThanOrEqual(MAX_REVIEW_FILE_BYTES);
    expect(diff.length).toBeGreaterThanOrEqual(MAX_REVIEW_FILE_BYTES - 200);
    expect(dec.decode(diff).endsWith("\n")).toBe(true);
    expect(thread.length).toBeLessThanOrEqual(MAX_REVIEW_FILE_BYTES);
    const td = JSON.parse(dec.decode(thread)) as { comments: ThreadComment[]; omitted_comments?: number };
    expect(td.omitted_comments).toBeGreaterThan(0);
    expect(td.comments.length + td.omitted_comments!).toBe(8);
    expect(td.comments.at(-1)!.id).toBe("7");
    const reasons = (b!.envelope.omitted ?? []).map((o) => `${o.name}: ${o.reason}`).join("\n");
    for (const want of ["file_diff.patch: cut at 256 KiB", "review_thread.json: ", "older comments are left out"])
      expect(reasons).toContain(want);

    // A comment of 1 MiB in a thread of its own is cut and stays valid JSON.
    const one = threadFile({ ref: "100", comments: [{ id: "", author: "", body: '"'.repeat(1 << 20) }] });
    expect(one.data.length).toBeLessThanOrEqual(MAX_REVIEW_FILE_BYTES);
    expect(one.dropped).toBe(0);
    expect(() => JSON.parse(dec.decode(one.data)) as unknown).not.toThrow();

    // The forge says that it cut the patch.
    rf.diff = { path: "pkg/x.go", patch: "@@ -1 +1 @@\n-a\n+b", truncated: true };
    b = await build("m", reviewEvent(), true, reviewBuilders(new Source(rf)));
    expect(b!.envelope.omitted).toContainEqual({ name: "context/file_diff.patch", reason: "the forge cut the diff" });
  });

  it("builder files go through the redactor like the payload", async () => {
    const rf = new Reader(
      { ref: "100", path: "cfg.go", comments: [{ id: "1", author: "a", body: "token is s3cr3t-value" }] },
      { path: "cfg.go", patch: '@@ -1 +1 @@\n-x\n+key = "s3cr3t-value"\n' },
    );
    const b = await build("m", reviewEvent(), true, reviewBuilders(new Source(rf)));
    const got = decode(encode(b!));
    let seen = 0;
    for (const f of redact(got.files, (s) => s.replaceAll("s3cr3t-value", "[REDACTED]"))) {
      const t = dec.decode(f.data);
      expect(t, f.path).not.toContain("s3cr3t-value");
      if (t.includes("[REDACTED]")) seen++;
    }
    expect(seen).toBeGreaterThanOrEqual(2);
  });
});
