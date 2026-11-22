// Builders that read the review thread and the file diff of a review comment
// from the forge.
import {
  cutBytes,
  oneLine,
  splitRepo,
  truncate,
  type Event,
  type FileDiff,
  type ReviewThread,
} from "@openfactory/core";
import { isNotSupported } from "@openfactory/plugin-api";
import type { ForgeResolver } from "../../forges/forge";
import { utf8 } from "../../lib/bytes";
import { MAX_REVIEW_FILE_BYTES, indentJSON, type Builder, type File, type Omitted } from "../types";
import { payloadObj } from "./payload";

const REVIEW_THREAD_FILE = "review_thread.json";
const FILE_DIFF_FILE = "file_diff.patch";

/** The text on one line, at most 300 characters. */
const shortLine = (s: string): string => truncate(oneLine(s), 300);

function readReason(what: string, err: unknown): string {
  // A plugin Worker's OfError crosses RPC as a plain Error: match the code, not the class.
  if (isNotSupported(err)) return "this forge cannot read " + what;
  return "could not read " + what + ": " + shortLine((err as Error).message);
}

function payloadPath(ev: Event): string {
  const p = payloadObj(ev) as {
    comment?: { path?: string } | null;
    object_attributes?: { position?: { new_path?: string; old_path?: string } | null } | null;
  } | null;
  if (!p) return "";
  if (p.comment?.path) return p.comment.path;
  const pos = p.object_attributes?.position;
  if (pos) return pos.new_path || pos.old_path || "";
  return "";
}

/** Encodes the thread; drops oldest comments, then cuts the last body, to fit the limit. */
export function threadFile(th: ReviewThread): { data: Uint8Array; dropped: number } {
  const d: ReviewThread & { omitted_comments?: number } = { ...th, comments: [...(th.comments ?? [])] };
  let dropped = 0;
  for (;;) {
    if (dropped > 0) d.omitted_comments = dropped;
    const b = indentJSON(d);
    if (b.length <= MAX_REVIEW_FILE_BYTES) return { data: b, dropped };
    if (d.comments!.length > 1) {
      d.comments = d.comments!.slice(1);
      dropped++;
      continue;
    }
    const c = { ...d.comments![0]! };
    if (!c.body) return { data: utf8("{}"), dropped };
    const bodyBytes = utf8(c.body);
    const cutTo = Math.max(0, bodyBytes.length - 2 * (b.length - MAX_REVIEW_FILE_BYTES) - 64);
    c.body = new TextDecoder().decode(cutBytes(bodyBytes, cutTo));
    d.comments = [c];
  }
}

/** The diff file with its header, cut at a line end over the limit. */
export function diffFile(path: string, patch0: string): { data: Uint8Array; left: number } {
  const header = `--- a/${path}\n+++ b/${path}\n`;
  let patch = patch0.endsWith("\n") ? patch0 : patch0 + "\n";
  const room = Math.max(0, MAX_REVIEW_FILE_BYTES - utf8(header).length);
  const pb = utf8(patch);
  let left = 0;
  if (pb.length > room) {
    let kept = new TextDecoder().decode(cutBytes(pb, room));
    const i = kept.lastIndexOf("\n");
    if (i >= 0) kept = kept.slice(0, i + 1);
    left = pb.length - utf8(kept).length;
    patch = kept;
  }
  return { data: utf8(header + patch), left };
}

/** Builders that read the review thread and the file diff of a review comment from the forge. */
export function reviewBuilders(src: ForgeResolver | null): Builder[] {
  return [
    ["github", "GITHUB"],
    ["gitlab", "GITLAB"],
    ["azure_devops", "AZURE_DEVOPS"],
  ].map(([provider, codeForge]) => ({
    provider: () => provider!,
    async eventContext(ev: Event) {
      const s = ev.source;
      if (!s.thread_ref || !s.number || s.number <= 0 || !s.repo || (s.kind && s.kind !== "pull_request"))
        return { files: [] };
      const [owner, repo] = splitRepo(s.repo);
      if (!owner || !repo) return { files: [] };
      const leftOut = (reason: string) => ({
        files: [],
        omitted: [
          { name: "context/" + REVIEW_THREAD_FILE, reason },
          { name: "context/" + FILE_DIFF_FILE, reason },
        ],
      });
      if (!src) return leftOut("no forge client: no forge source");
      let f;
      try {
        f = await src.forge({
          teamId: ev.team_id,
          ...(ev.factory_id ? { factoryId: ev.factory_id } : {}),
          codeForge: codeForge!,
          repo: s.repo,
          scope: [s.repo],
        });
      } catch (e) {
        return leftOut("no forge client: " + shortLine((e as Error).message));
      }
      if (!f.reviewThread || !f.fileDiff) return leftOut(`the ${provider} forge cannot read review threads or diffs`);
      const files: File[] = [];
      const omitted: Omitted[] = [];
      let path = payloadPath(ev);
      try {
        const th = await f.reviewThread(owner, repo, s.number, s.thread_ref);
        const { data, dropped } = threadFile(th);
        files.push({ path: REVIEW_THREAD_FILE, data });
        if (dropped > 0)
          omitted.push({
            name: "context/" + REVIEW_THREAD_FILE,
            reason: `${dropped} older comments are left out (limit ${MAX_REVIEW_FILE_BYTES >> 10} KiB)`,
          });
        if (th.path) path = th.path;
      } catch (e) {
        omitted.push({ name: "context/" + REVIEW_THREAD_FILE, reason: readReason("the review thread", e) });
      }
      if (path === "") {
        omitted.push({ name: "context/" + FILE_DIFF_FILE, reason: "the comment is not on a file" });
        return { files, omitted };
      }
      let d: FileDiff;
      try {
        d = await f.fileDiff(owner, repo, s.number, path);
      } catch (e) {
        omitted.push({ name: "context/" + FILE_DIFF_FILE, reason: readReason("the diff of " + path, e) });
        return { files, omitted };
      }
      const { data, left } = diffFile(path, d.patch);
      files.push({ path: FILE_DIFF_FILE, data });
      if (left > 0)
        omitted.push({
          name: "context/" + FILE_DIFF_FILE,
          reason: `cut at ${MAX_REVIEW_FILE_BYTES >> 10} KiB, ${left} bytes of the diff are left out`,
        });
      else if (d.truncated) omitted.push({ name: "context/" + FILE_DIFF_FILE, reason: "the forge cut the diff" });
      return { files, omitted };
    },
  }));
}
