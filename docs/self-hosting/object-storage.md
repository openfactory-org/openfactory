# Object storage

Artifacts, raw harness transcripts, workspace snapshots, prompt attachments and the runner binary go to one R2 bucket. The Durable Objects keep their metadata and the run messages. Clients (dashboard, CLI) read run data through short-lived presigned URLs. The runner in a sandbox uploads artifacts and snapshots through presigned URLs too.

## Set up the bucket

1. Create the bucket:

   ```bash
   npx wrangler r2 bucket create openfactory-data
   ```

2. Create an R2 API token with read and write access to this bucket. Note the access key ID and the secret.
3. Set the bucket name and your account ID in `wrangler.jsonc`:

   | Variable | Value |
   |---|---|
   | `OF_R2_BUCKET` | The bucket name. |
   | `OF_R2_ACCOUNT_ID` | Your Cloudflare account ID. |
   | `OF_OBJECT_STORE_PRESIGN_TTL` | Life of presigned URLs (default `15m`, at most 7 days). |

4. Store the token as secrets:

   ```bash
   npx wrangler secret put OF_R2_ACCESS_KEY_ID -c apps/control-plane/wrangler.jsonc
   npx wrangler secret put OF_R2_SECRET_ACCESS_KEY -c apps/control-plane/wrangler.jsonc
   ```

The Worker reads and writes objects through the `DATA` binding. It uses the S3 credentials only to sign URLs for sandboxes and clients. The URL host is `<account>.r2.cloudflarestorage.com`.

## Keys

The control plane builds each key from the run ID. Large definition trees, payloads and event bundles also go to the bucket, by content hash.

```text
runs/{run_id}/harness/{session}.jsonl
runs/{run_id}/artifacts/{name}
runs/{run_id}/attachments/{name}
runs/{run_id}/sessions/{session}/{kind}/{name}   (runner uploads, before the control plane copies them)
attachments/{team_id}/{attachment_id}/{name}     (uploads with POST /attachments)
snapshots/{team_id}/{run_id}/{session}/{part}/{file}
```

## Reach the bucket from sandboxes

A sandbox must reach the R2 S3 host to upload snapshots and artifacts. A restricted [network policy](network-policy.md#required-hosts) always allows this host. A sandbox that cannot reach it cannot upload. The runner reports a diagnostic such as `artifact/upload_failed`, and the turn does not fail.

Snapshots can contain every workspace file, including files with secrets. Do not make the bucket public.

## Retention

Run data is kept until an admin sets a retention time in the team settings. The retention job then deletes the objects of runs that are older than the limit, and attachments that were uploaded longer ago than the limit. In a long-lived run, an attachment can be deleted before the message that uses it. The link then returns `404`.

## Change the bucket

A new bucket does not move old data. After the change, the control plane reads and writes only the new bucket: old artifacts, attachments and transcripts are no longer shown, and a new session of an old run cannot restore its snapshot.

To keep old data, copy every object with the same key to the new bucket before you change `OF_R2_BUCKET`, for example with `rclone copy`. Compare the object count and total size, deploy, then open an old run to check that its artifacts load.
