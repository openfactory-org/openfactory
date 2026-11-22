// Bundles in R2, stored by the SHA-256 of their JSON: WorkDO keeps only the
// reference (the R2 key).
import { OfError } from "@openfactory/core";
import { sha256Hex } from "../lib/bytes";
import { decode, encode } from "./bundle";
import type { Bundle } from "./types";

/** R2 key prefix of stored bundles. */
export const R2_PREFIX = "eventctx/";

/** Stores a bundle in R2 by the SHA-256 of its JSON; returns the reference (R2 key). */
export async function storeBundle(bucket: R2Bucket, b: Bundle): Promise<string> {
  const text = encode(b);
  const key = R2_PREFIX + (await sha256Hex(text));
  if (!(await bucket.head(key))) await bucket.put(key, text, { httpMetadata: { contentType: "application/json" } });
  return key;
}

/** Loads a bundle stored with storeBundle; null when it is gone. */
export async function loadBundle(bucket: R2Bucket, ref: string): Promise<Bundle | null> {
  if (!ref.startsWith(R2_PREFIX)) throw new OfError("invalid_request", `eventctx: bad bundle reference "${ref}"`);
  const obj = await bucket.get(ref);
  if (!obj) return null;
  return decode(await obj.text());
}

/**
 * How long bundles stay in R2. A bundle has no single owner: the runs that one
 * event starts share its key, and a retry stores the same key again. So a run
 * end never removes a bundle; only expire does.
 */
export const RETENTION_MS = 7 * 24 * 3600_000;

/** Removes bundles uploaded before now minus RETENTION_MS. Returns the number removed. */
export async function expire(bucket: R2Bucket, nowMs: number): Promise<number> {
  let n = 0;
  let cursor: string | undefined;
  do {
    const page = await bucket.list({ prefix: R2_PREFIX, ...(cursor ? { cursor } : {}) });
    const old = page.objects.filter((o) => o.uploaded.getTime() < nowMs - RETENTION_MS).map((o) => o.key);
    if (old.length > 0) {
      await bucket.delete(old);
      n += old.length;
    }
    cursor = page.truncated ? page.cursor : undefined;
  } while (cursor);
  return n;
}
