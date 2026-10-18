# Object storage

Artifacts, raw harness transcripts, snapshots and prompt attachments go to object storage. The database keeps their metadata and the run messages. Clients (dashboard, CLI, SDKs) read run data through short-lived presigned URLs. The runner in a sandbox uploads artifacts and snapshots through presigned URLs, so sandbox Pods need no store credentials.

For all settings and defaults, see [Configuration](configuration.md#object-storage).

## Drivers

`OF_OBJECT_STORE_URL` selects the driver by its scheme. A path after the bucket or container is a prefix for every key.

| URL | Driver | Credentials |
|---|---|---|
| `file:///var/lib/openfactory/objects` | Local disk. Default `file://<data dir>/objects`. | None. Presigned links are signed with `OF_OBJECT_STORE_SIGNING_KEY`. |
| `s3://bucket/prefix?region=eu-west-1` | Amazon S3 and S3-compatible stores. For MinIO and similar, add `&endpoint=https://minio.example.com&path_style=true`. | Static keys, IRSA, EKS Pod Identity or an ECS sandbox role. See [Amazon S3 credentials](#amazon-s3-credentials). |
| `gs://bucket/prefix` | Google Cloud Storage. `?endpoint=` points at an emulator. | `GOOGLE_APPLICATION_CREDENTIALS` (service-account JSON), else the GCE metadata server (Workload Identity). |
| `azblob://account/container/prefix` | Azure Blob Storage. `?endpoint=http://127.0.0.1:10000/devstoreaccount1` points at Azurite. | `AZURE_STORAGE_KEY` (or `AZURE_STORAGE_ACCOUNT_KEY`). |

## Amazon S3 credentials

The S3 driver uses the first source that is set:

1. **Static keys:** `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (set both or neither), with optional `AWS_SESSION_TOKEN`.
2. **IRSA:** `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`, with optional `AWS_ROLE_SESSION_NAME`. EKS sets them when the service account has the annotation `eks.amazonaws.com/role-arn`. The driver calls STS `AssumeRoleWithWebIdentity` in the store region, or at `AWS_ENDPOINT_URL_STS`. IRSA needs an IAM OIDC provider for the cluster.
3. **Container credentials:** `AWS_CONTAINER_CREDENTIALS_FULL_URI` or `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`, with the token in `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE` or `AWS_CONTAINER_AUTHORIZATION_TOKEN`. EKS Pod Identity and ECS sandbox roles set them. Pod Identity needs no IAM OIDC provider. A plain `http` URL must point at a loopback address, `169.254.170.2` (ECS) or the Pod Identity agent (`169.254.170.23`, `fd00:ec2::23`).

The region comes from `?region=`, else `AWS_REGION`, else `AWS_DEFAULT_REGION`, else `us-east-1`.

Temporary credentials (sources 2 and 3) are fetched on first use and renewed 5 minutes before they expire. The token file is read again at each renewal, so token rotation works. A presigned URL gets credentials valid for its whole lifetime (`OF_OBJECT_STORE_PRESIGN_TTL`). If credentials cannot be fetched, the operation fails with the STS or endpoint error, and the next operation tries again.

## Choose a store

| Mode | Store |
|---|---|
| SQLite (one replica) | The default local disk. With the Helm chart it is on the database PersistentVolumeClaim, so one volume backup covers both. |
| PostgreSQL with more than one replica | S3, GCS or Azure Blob. Replicas do not share a disk. The chart refuses more than one replica without `objectStore.url`. |
| Run data in the customer's account | A bucket that the customer owns. The database keeps no run content. |

## Keys

The server builds each key from the run ID; the database does not store store URLs. The store URL prefix comes before each key.

```text
runs/{run_id}/harness/{session}.jsonl
runs/{run_id}/artifacts/{name}
runs/{run_id}/attachments/{name}
runs/{run_id}/sessions/{session}/{kind}/{name}   (runner uploads, before the server copies them)
attachments/{team_id}/{attachment_id}/{name}     (uploads with POST /attachments)
snapshots/{team_id}/{run_id}/{session}/{part}/{file}
```

## Retention

Without `OF_OBJECT_STORE_RETENTION`, nothing is deleted. With it, the leader runs retention every `OF_OBJECT_STORE_RETENTION_INTERVAL` (default `1h`) and deletes:

- the objects under `runs/{run_id}/` and `snapshots/.../{run_id}/` of a run whose newest object is older than the limit;
- the files under `attachments/{team_id}/{attachment_id}/` (in the artifacts store) uploaded longer ago than the limit, and their metadata.

Uploaded attachments are not linked to a run, so only the upload age counts. In a long-lived run, an attachment file can be deleted before the message that uses it; the link then returns `404`. A store that cannot delete metadata rows keeps the small rows of deleted attachments.

## Reach the store from sandboxes and clients

The server builds presigned URLs from the store URL and does not rewrite the host. So the `endpoint=` host must resolve and be reachable on the server, in sandbox Pods and in the client network. A name that only one of them resolves, such as a Docker network name, breaks the others. Amazon S3 needs no setting.

Test from a sandbox Pod, for example with a presigned URL from `GET /api/v1/runs/{runId}/artifacts`. A Pod that cannot reach the store cannot upload artifacts or snapshots. The runner reports a diagnostic such as `artifact/upload_failed`; the turn does not fail. If a NetworkPolicy limits sandbox egress, allow the store host (see [Network policy](network-policy.md)).

## Change the store URL

A new store URL does not move old data. After the restart, the server reads and writes only the new store:

- old artifacts, attachments and raw transcripts are no longer shown;
- a new session of an old run cannot restore its snapshot files, and the server logs a warning for each missing object.

The same applies when you change the prefix or set a category URL (`OF_OBJECT_STORE_TRANSCRIPTS_URL`, `_ARTIFACTS_URL`, `_ATTACHMENTS_URL`, `_SNAPSHOTS_URL`) for the first time.

To keep old data:

1. Choose a quiet time, or stop the server. A run that writes during the copy can leave its newest objects behind; copy again after the switch to catch them.
2. Copy every object and keep the key: new key = new prefix + the old key after the old prefix.

   ```bash
   # S3 to S3
   aws s3 sync s3://old-bucket/prod s3://new-bucket/prod
   # Local disk to S3 (where the disk is mounted)
   aws s3 sync /var/lib/openfactory/objects s3://new-bucket/prod
   # Any store with rclone (configure the remotes first)
   rclone copy old:bucket/prod new:bucket/prod --checksum
   ```

   With category stores, copy `runs/` (artifacts, transcripts, attachments) and `snapshots/` to the store of each category.
3. Compare object count and total size in both locations.
4. Set the new URL (`OF_OBJECT_STORE_URL`, or `objectStore.url` in Helm) and restart.
5. Open an old run in the dashboard and check that its artifacts and attachments load.
6. Delete the old store after you have checked a few runs.

With `file://`, keep the same `OF_OBJECT_STORE_SIGNING_KEY`, or presigned links that clients hold stop working.

## Helm values

```yaml
objectStore:
  url: s3://acme-factory-runs/prod?region=eu-west-1
  retention: 2160h          # 90 days
  # Static credentials (stored in the chart Secret). Prefer a role:
  # credentials:
  #   AWS_ACCESS_KEY_ID: AKIA...
  #   AWS_SECRET_ACCESS_KEY: ...
  # Or load credentials from an existing Secret:
  # existingSecret: factory-object-store
  # GCS service-account key, mounted as GOOGLE_APPLICATION_CREDENTIALS:
  # gcsCredentialsSecret: factory-gcs
# IRSA: annotate the server service account with the role.
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/factory-runs
```

For EKS Pod Identity, omit the annotation and create an association for the release namespace and the server service account (`openfactory` for a release of that name):

```bash
aws eks create-pod-identity-association --cluster-name prod \
  --namespace openfactory --service-account openfactory \
  --role-arn arn:aws:iam::123456789012:role/factory-runs
```

Restart the server Pods afterwards. EKS adds the variables only to Pods that start after the association exists.

The bucket must exist. Grant the server read, write, list and delete on the prefix.

To test the S3 driver against the AWS emulator, see [Local testing](local-testing.md).
