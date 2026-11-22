// REST handlers: attachments. Owner: WS5. Port of internal/api/attachments.go.
// The object and its metadata live in R2 under attachments/<team>/<id>/<name>
// (custom metadata holds the creator), so no DO row is needed; the runner
// downloads read the same object (src/runner-api/transfer.ts).
import { newId, normalizeAttachmentName, OfError } from "@openfactory/core";
import type { Handler } from "../http/handler";
import { validateKey } from "../storage/rundata";
import { attachmentPrefix, getAttachment, MAX_ATTACHMENT_BYTES, type Attachment } from "../runner-api/transfer";
import type { Env } from "../contracts/env";
import { json, teamOf } from "./runs";

/** The API form: the object key stays hidden. */
function publicAttachment(a: Attachment): Omit<Attachment, "key"> {
  const { key: _k, ...rest } = a;
  return rest;
}

/** 413 payload_too_large (CodePayloadTooLarge; not an OfError code). */
function tooLarge(instance: string): Response {
  const detail = `an attachment is limited to ${MAX_ATTACHMENT_BYTES} bytes`;
  const title = "The request body is too large.";
  return new Response(
    JSON.stringify({
      type: "/errors/payload-too-large",
      title,
      status: 413,
      code: "payload_too_large",
      error: { code: "payload_too_large", message: `${title} (${detail})` },
      retryable: false,
      detail,
      instance,
    }),
    { status: 413, headers: { "content-type": "application/problem+json" } },
  );
}

/** POST /api/v1/attachments?name=: stores the request body as an attachment. */
export const uploadAttachment: Handler = async (req, env, ctx) => {
  const name = normalizeAttachmentName(ctx.url.searchParams.get("name") ?? "");
  if (Number(req.headers.get("content-length") ?? "0") > MAX_ATTACHMENT_BYTES) return tooLarge(ctx.url.pathname);
  const data = new Uint8Array(await req.arrayBuffer());
  if (data.byteLength > MAX_ATTACHMENT_BYTES) return tooLarge(ctx.url.pathname);
  const ct = (req.headers.get("content-type") ?? "").trim() || "application/octet-stream";
  const team = teamOf(ctx);
  const id = newId("att");
  const key = attachmentPrefix(team, id) + name;
  try {
    validateKey(key);
  } catch (e) {
    throw new OfError("invalid_request", `invalid attachment name: ${(e as Error).message}`);
  }
  const createdBy = ctx.principal?.userId || ctx.principal?.id || "";
  const createdAt = new Date().toISOString();
  await env.DATA.put(key, data, {
    httpMetadata: { contentType: ct },
    customMetadata: { created_by: createdBy, created_at: createdAt },
  });
  const att: Attachment = {
    id,
    team_id: team,
    name,
    content_type: ct,
    bytes: data.byteLength,
    key,
    created_by: createdBy,
    created_at: createdAt,
  };
  return json(201, publicAttachment(att));
};

/** checkAttachments: each ID exists in the team; at most 20 per message. */
export async function checkAttachments(env: Env, teamId: string, ids: string[]): Promise<void> {
  if (ids.length === 0) return;
  if (ids.length > 20) throw new OfError("invalid_request", "a message can have at most 20 attachments");
  for (const id of ids) {
    if (!(await getAttachment(env, teamId, id)))
      throw new OfError("invalid_request", `attachment ${JSON.stringify(id)} does not exist`);
  }
}
