// Runner uploads, downloads, git credentials and cloud tokens. Port of
// internal/runnerapi/transfer.go, uploadkey.go, gitcred.go and cloudtoken.go.
// Upload IDs carry the object key and an HMAC of it, so the control plane
// trusts the key of an artifact, transcript or snapshot event without state.
import type { Env } from "../contracts/env";
import type { Run, RunSession } from "../engine/types";
import { b64url, fromUtf8, hmacSha256, timingSafeEqual, tryFromB64url, utf8 } from "../lib/bytes";
import { presignR2 } from "../storage/presign";
import { validateKey } from "../storage/rundata";
import type { RunnerKey } from "./token";

export const DEFAULT_UPLOAD_TTL_S = 15 * 60;
export const PART_WORKSPACE = "workspace";
export const PART_HARNESS_STATE = "harness_state";
export const MAX_UPLOAD_BYTES: Record<string, number> = {
  artifact: 100 << 20,
  transcript: 200 << 20,
  snapshot: 2 * 1024 * 1024 * 1024,
};
export const CLOUD_TOKEN_TTL = { def: 3600, min: 60, max: 3600 };

function uploadMac(key: Uint8Array, objectKey: string): Promise<Uint8Array> {
  return hmacSha256(key, "upload:" + objectKey);
}

/** up_<b64url(key)>.<b64url(HMAC("upload:"+key))> with the current key. */
export async function uploadId(keys: RunnerKey[], objectKey: string): Promise<string> {
  return `up_${b64url(utf8(objectKey))}.${b64url(await uploadMac(keys[0]!.key, objectKey))}`;
}

/** The object key of an upload ID that this control plane issued (any known key), else undefined. */
export async function parseUploadId(keys: RunnerKey[], id: string): Promise<string | undefined> {
  if (!id.startsWith("up_")) return undefined;
  const rest = id.slice(3);
  const dot = rest.indexOf(".");
  if (dot < 0) return undefined;
  const kb = tryFromB64url(rest.slice(0, dot));
  const mac = tryFromB64url(rest.slice(dot + 1));
  if (!kb || !mac) return undefined;
  const key = fromUtf8(kb);
  for (const k of keys) if (timingSafeEqual(mac, await uploadMac(k.key, key))) return key;
  return undefined;
}

const teamOf = (run: Pick<Run, "team_id">) => run.team_id || "default";

/** objectPrefix: snapshots/<team>/<run>/<n>/<part>/ or runs/<run>/sessions/<n>/<kind>/. */
export function objectPrefix(run: Pick<Run, "id" | "team_id">, session: number, kind: string, part = ""): string {
  if (kind === "snapshot") return `snapshots/${teamOf(run)}/${run.id}/${session}/${part}/`;
  return `runs/${run.id}/sessions/${session}/${kind}/`;
}

export function ownsKey(run: Pick<Run, "id" | "team_id">, key: string): boolean {
  return key.startsWith(`runs/${run.id}/`) || key.startsWith(`snapshots/${teamOf(run)}/${run.id}/`);
}

export interface UploadRequest {
  kind: string;
  name: string;
  part?: string;
  bytes: number;
  sha256: string;
  content_type?: string;
}

export type TransferError = { status: number; code: string; message: string };
const bad = (message: string): TransferError => ({ status: 400, code: "bad_request", message });

/** UploadRequest.Validate (runnerproto). */
export function validateUpload(req: UploadRequest): TransferError | undefined {
  if (!["artifact", "snapshot", "transcript"].includes(req.kind))
    return bad(`invalid kind ${JSON.stringify(req.kind)}`);
  if (!req.name) return bad("name is empty");
  if (req.name.startsWith("/")) return bad("name starts with /");
  if (req.name.split("/").includes("..")) return bad("name has a .. path element");
  if (typeof req.bytes !== "number" || req.bytes < 0) return bad("bytes is negative");
  if (!/^[0-9a-f]{64}$/.test(req.sha256 ?? "")) return bad("sha256 is not 64 lower-case hex characters");
  if (req.kind === "snapshot") {
    if (req.part !== PART_WORKSPACE && req.part !== PART_HARNESS_STATE)
      return bad(`invalid part ${JSON.stringify(req.part ?? "")} for a snapshot`);
  } else if (req.part) return bad(`part must be empty for kind ${JSON.stringify(req.kind)}`);
  if (req.bytes <= 0) return bad("bytes must be more than 0");
  const max = MAX_UPLOAD_BYTES[req.kind] ?? MAX_UPLOAD_BYTES.artifact!;
  if (req.bytes > max)
    return { status: 413, code: "too_large", message: `a ${req.kind} upload is limited to ${max} bytes` };
  return undefined;
}

/** POST /session/uploads: a presigned R2 PUT URL and the upload ID. */
export async function presignUpload(
  env: Env,
  keys: RunnerKey[],
  run: Run,
  session: RunSession,
  req: UploadRequest,
  nowMs = Date.now(),
): Promise<Record<string, unknown> | TransferError> {
  const err = validateUpload(req);
  if (err) return err;
  const key = objectPrefix(run, session.session, req.kind, req.part ?? "") + req.name;
  try {
    validateKey(key);
  } catch (e) {
    return bad(`invalid name: ${(e as Error).message}`);
  }
  const url = await presignR2(env, {
    method: "PUT",
    key,
    expiresSeconds: DEFAULT_UPLOAD_TTL_S,
    ...(req.content_type ? { contentType: req.content_type } : {}),
  });
  return {
    upload_id: await uploadId(keys, key),
    method: "PUT",
    url,
    headers: req.content_type ? { "Content-Type": req.content_type } : {},
    expires_at: new Date(nowMs + DEFAULT_UPLOAD_TTL_S * 1000).toISOString(),
  };
}

/** GET /session/downloads?kind=snapshot: the files of the newest saved parts before this session. */
export async function snapshotDownloads(
  env: Env,
  keys: RunnerKey[],
  run: Run,
  parts: { session: number; part: string; uploadIds: string[] }[],
  nowMs = Date.now(),
): Promise<Record<string, unknown>[]> {
  const out: Record<string, unknown>[] = [];
  for (const want of [PART_WORKSPACE, PART_HARNESS_STATE]) {
    const p = parts.find((x) => x.part === want);
    if (!p) continue;
    const prefix = objectPrefix(run, p.session, "snapshot", want);
    for (const id of p.uploadIds) {
      const key = await parseUploadId(keys, id);
      if (!key || !ownsKey(run, key) || !key.startsWith(prefix) || key.length === prefix.length) {
        console.warn("runner-api: skip a bad snapshot upload ID", run.id, p.session, want);
        continue;
      }
      const head = await env.DATA.head(key);
      if (!head) {
        console.warn("runner-api: snapshot object is missing", run.id, p.session, key);
        continue;
      }
      out.push({
        part: want,
        name: key.slice(prefix.length),
        url: await presignR2(env, { method: "GET", key, expiresSeconds: DEFAULT_UPLOAD_TTL_S }),
        bytes: head.size,
        expires_at: new Date(nowMs + DEFAULT_UPLOAD_TTL_S * 1000).toISOString(),
      });
    }
  }
  return out;
}

// ---------- attachments (R2 holds the object and its metadata) ----------

export interface Attachment {
  id: string;
  team_id: string;
  name: string;
  content_type: string;
  bytes: number;
  key: string;
  created_by: string;
  created_at: string;
}

export const MAX_ATTACHMENT_BYTES = 25 << 20;
export const attachmentPrefix = (teamId: string, id: string) => `attachments/${teamId}/${id}/`;

/** The attachment metadata of an ID in a team, from the R2 object under attachments/<team>/<id>/. */
export async function getAttachment(env: Env, teamId: string, id: string): Promise<Attachment | undefined> {
  if (!/^att_[a-z0-9]+$/.test(id)) return undefined;
  const list = await env.DATA.list({
    prefix: attachmentPrefix(teamId, id),
    limit: 1,
    include: ["customMetadata", "httpMetadata"],
  });
  const obj = list.objects[0];
  if (!obj) return undefined;
  const m = obj.customMetadata ?? {};
  return {
    id,
    team_id: teamId,
    name: obj.key.slice(attachmentPrefix(teamId, id).length),
    content_type: obj.httpMetadata?.contentType ?? "application/octet-stream",
    bytes: obj.size,
    key: obj.key,
    created_by: m.created_by ?? "",
    created_at: m.created_at ?? obj.uploaded.toISOString(),
  };
}

/** GET /session/downloads?kind=attachment&id=: the attachment of an input of the run. */
export async function attachmentDownload(
  env: Env,
  run: Run,
  att: Attachment | undefined,
  nowMs = Date.now(),
): Promise<Record<string, unknown>[] | undefined> {
  if (!att || att.team_id !== teamOf(run) || !att.key) return undefined;
  return [
    {
      part: "attachment",
      name: att.name,
      url: await presignR2(env, { method: "GET", key: att.key, expiresSeconds: DEFAULT_UPLOAD_TTL_S }),
      bytes: att.bytes,
      expires_at: new Date(nowMs + DEFAULT_UPLOAD_TTL_S * 1000).toISOString(),
    },
  ];
}
