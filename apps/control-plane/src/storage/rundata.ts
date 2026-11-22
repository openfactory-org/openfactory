// Run data in R2. Port of internal/objectstore/rundata.go with the same key
// layout:
//
//   runs/{runID}/harness/{session}.jsonl  raw harness transcripts
//   runs/{runID}/artifacts/{name}
//   runs/{runID}/attachments/{name}
//   snapshots/{team}/{runID}/{session}/{part}/{file}
//
// Reads and writes go through the R2 binding; clients outside the Worker get
// presigned URLs (presign.ts). Retention is a WorkDO timer (03 §10), which
// calls deleteRun.
import { OfError } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { presignR2 } from "./presign";

export interface ObjectInfo {
  key: string;
  size: number;
  contentType?: string;
  lastModified: string;
}

/** ValidateKey: 1..1024 bytes, no leading/trailing slash, no control characters, backslash, empty or relative segments. */
export function validateKey(key: string): void {
  const bad = (why: string) => new OfError("invalid_request", `invalid object key: ${why}`);
  if (key === "") throw bad("empty");
  if (new TextEncoder().encode(key).length > 1024) throw bad("longer than 1024 bytes");
  if (key.startsWith("/") || key.endsWith("/")) throw bad(`${JSON.stringify(key)} has a leading or trailing slash`);
  for (const ch of key) {
    const c = ch.codePointAt(0)!;
    if (c < 0x20 || c === 0x7f || ch === "\\") throw bad(`${JSON.stringify(key)} contains a control character or backslash`);
  }
  for (const seg of key.split("/")) {
    if (seg === "") throw bad(`${JSON.stringify(key)} has an empty segment`);
    if (seg === "." || seg === "..") throw bad(`${JSON.stringify(key)} has a relative segment`);
  }
}

function checkRunId(runId: string): void {
  if (runId === "" || /[/\\]/.test(runId)) throw new OfError("invalid_request", `invalid object key: bad run id ${JSON.stringify(runId)}`);
  validateKey(runId);
}

export const runPrefix = (runId: string) => `runs/${runId}/`;
export const SNAPSHOTS_PREFIX = "snapshots/";

export function namedKey(runId: string, dir: "artifacts" | "attachments", name: string): string {
  checkRunId(runId);
  validateKey(name);
  return `${runPrefix(runId)}${dir}/${name}`;
}

export function harnessTranscriptKey(runId: string, session: number): string {
  checkRunId(runId);
  return `${runPrefix(runId)}harness/${session}.jsonl`;
}

/** snapshots/{team}/{run}/{session}/{part}/{file} */
export function snapshotKey(teamId: string, runId: string, session: number, part: string, file: string): string {
  checkRunId(runId);
  const k = `${SNAPSHOTS_PREFIX}${teamId}/${runId}/${session}/${part}/${file}`;
  validateKey(k);
  return k;
}

/** Run ID of a snapshot key, or undefined. */
export function snapshotRunId(key: string): string | undefined {
  const parts = key.slice(SNAPSHOTS_PREFIX.length).split("/");
  if (!key.startsWith(SNAPSHOTS_PREFIX) || parts.length < 3 || parts[1] === "") return undefined;
  return parts[1];
}

async function list(bucket: R2Bucket, prefix: string): Promise<ObjectInfo[]> {
  const out: ObjectInfo[] = [];
  let cursor: string | undefined;
  do {
    const page = await bucket.list({ prefix, ...(cursor ? { cursor } : {}), include: ["httpMetadata"] });
    for (const o of page.objects) {
      const info: ObjectInfo = { key: o.key, size: o.size, lastModified: o.uploaded.toISOString() };
      if (o.httpMetadata?.contentType) info.contentType = o.httpMetadata.contentType;
      out.push(info);
    }
    cursor = page.truncated ? page.cursor : undefined;
  } while (cursor);
  return out;
}

export class RunData {
  constructor(private readonly env: Env) {}

  private get bucket(): R2Bucket {
    return this.env.DATA;
  }

  async putHarnessTranscript(runId: string, session: number, body: ReadableStream | ArrayBuffer | string): Promise<string> {
    const key = harnessTranscriptKey(runId, session);
    await this.bucket.put(key, body, { httpMetadata: { contentType: "application/x-ndjson" } });
    return key;
  }

  private async put(dir: "artifacts" | "attachments", runId: string, name: string, body: ReadableStream | ArrayBuffer | string, contentType?: string) {
    const key = namedKey(runId, dir, name);
    await this.bucket.put(key, body, contentType ? { httpMetadata: { contentType } } : undefined);
    return key;
  }

  private async open(dir: "artifacts" | "attachments", runId: string, name: string): Promise<R2ObjectBody> {
    const o = await this.bucket.get(namedKey(runId, dir, name));
    if (!o) throw new OfError("resource_not_found", `${dir.slice(0, -1)} ${name} not found`);
    return o;
  }

  private async listDir(dir: string, runId: string): Promise<ObjectInfo[]> {
    checkRunId(runId);
    const p = `${runPrefix(runId)}${dir}/`;
    return (await list(this.bucket, p)).map((o) => ({ ...o, key: o.key.slice(p.length) }));
  }

  putArtifact(runId: string, name: string, body: ReadableStream | ArrayBuffer | string, contentType?: string) {
    return this.put("artifacts", runId, name, body, contentType);
  }
  openArtifact(runId: string, name: string) {
    return this.open("artifacts", runId, name);
  }
  listArtifacts(runId: string) {
    return this.listDir("artifacts", runId);
  }
  artifactUrl(runId: string, name: string, ttlSeconds?: number) {
    return presignR2(this.env, { method: "GET", key: namedKey(runId, "artifacts", name), ...(ttlSeconds ? { expiresSeconds: ttlSeconds } : {}) });
  }

  putAttachment(runId: string, name: string, body: ReadableStream | ArrayBuffer | string, contentType?: string) {
    return this.put("attachments", runId, name, body, contentType);
  }
  openAttachment(runId: string, name: string) {
    return this.open("attachments", runId, name);
  }
  listAttachments(runId: string) {
    return this.listDir("attachments", runId);
  }
  attachmentUrl(runId: string, name: string, ttlSeconds?: number) {
    return presignR2(this.env, { method: "GET", key: namedKey(runId, "attachments", name), ...(ttlSeconds ? { expiresSeconds: ttlSeconds } : {}) });
  }
  /** Presigned PUT; the client sends Content-Type: contentType. */
  attachmentUploadUrl(runId: string, name: string, contentType: string, ttlSeconds?: number) {
    return presignR2(this.env, {
      method: "PUT",
      key: namedKey(runId, "attachments", name),
      contentType,
      ...(ttlSeconds ? { expiresSeconds: ttlSeconds } : {}),
    });
  }

  /** Deletes every object of a run (runs/{id}/ and its snapshots under snapshots/{team}/{id}/). */
  async deleteRun(runId: string, teamId: string): Promise<number> {
    checkRunId(runId);
    const keys = [
      ...(await list(this.bucket, runPrefix(runId))).map((o) => o.key),
      ...(await list(this.bucket, `${SNAPSHOTS_PREFIX}${teamId}/${runId}/`)).map((o) => o.key),
    ];
    for (let i = 0; i < keys.length; i += 1000) await this.bucket.delete(keys.slice(i, i + 1000));
    return keys.length;
  }

  /** Newest object time of a run in ms (for retention), 0 when none. */
  async newest(runId: string, teamId: string): Promise<number> {
    checkRunId(runId);
    let newest = 0;
    for (const p of [runPrefix(runId), `${SNAPSHOTS_PREFIX}${teamId}/${runId}/`]) {
      for (const o of await list(this.bucket, p)) newest = Math.max(newest, Date.parse(o.lastModified));
    }
    return newest;
  }
}
