# Local testing

The local stack runs the whole Open Factory flow on a workstation, in the shape of a production install on AWS: the AWS emulator gives the AWS APIs, and a local Kubernetes cluster runs the server and sandboxes. [Real-world checks](#real-world-checks) need real infrastructure.

> **Warning:** The local stack is for tests only. It keeps AWS state in memory, checks only some IAM policies, and gives every user of the workstation access to its services. Do not put production data or production credentials in it.

## What runs where

| Production on AWS | Local stack | Role of the AWS emulator |
|---|---|---|
| Amazon S3 (run data, snapshots, attachments) | AWS emulator | Stores the objects. |
| AWS IAM and AWS STS | AWS emulator | Holds roles and policies. Issues temporary credentials. |
| Amazon EKS Pod Identity | AWS emulator and a local Pod Identity agent | Holds the association. Issues service account tokens and exchanges them for role credentials. |
| Amazon EKS nodes | Your local cluster | Cluster record only. |
| Amazon RDS for PostgreSQL | PostgreSQL 16 in your cluster | Instance record only. Its database has no advisory locks, which the server needs. |
| Amazon ECR | Your local Docker | Repository records only. |
| Public webhook URL | Signed deliveries that the checks send | None. |
| Model API | A stand-in for the `claude` CLI, or the real CLI | None. |

## Parts of the stack

| Namespace | Contents | Address |
|---|---|---|
| `aws-local` | AWS emulator and `tlsfront` in one Pod | `https://aws.k8s.orb.local:9443` |
| `of-local-infra` | PostgreSQL 16 | `db.k8s.orb.local:5432` |
| | Git server (with `LOCAL_FORGE=none`) | `git://git.k8s.orb.local:9418/<repo>.git` |
| `of-local` | Helm release `openfactory`: two server replicas in PostgreSQL mode, each with a Pod Identity agent sidecar | `http://of.k8s.orb.local` |
| `of-local-sandboxes` | Sandbox Jobs, Pods and NetworkPolicies | |

Each name resolves to a LoadBalancer Service on the workstation and in Pods, so every client uses the same URLs. For kind, see [Test on a kind cluster](#test-on-a-kind-cluster).

### Why the emulator has a TLS front

The emulator's certificate names only `localhost` and `127.0.0.1`, but presigned S3 URLs sign the host name, so all clients must use one name. `tlsfront` (`deploy/aws-local/tlsfront`) serves port 9443 with a certificate for that name from a local CA. It forwards to the emulator on `127.0.0.1` and keeps the `Host` header, so signatures stay valid. For unsigned requests, which the emulator routes by `Host`, it sets `sts.amazonaws.com` on a form `POST` to `/` (`sts:AssumeRoleWithWebIdentity`) and `s3.amazonaws.com` on the rest.

### How the server gets AWS credentials

The server has no static AWS keys. It uses EKS Pod Identity, as on Amazon EKS:

1. The S3 driver sends the Pod's projected token (audience `pods.eks.amazonaws.com`) to the agent sidecar at `http://127.0.0.1:2703/v1/credentials` (`AWS_CONTAINER_CREDENTIALS_FULL_URI`, `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`).
2. The agent checks the token with a `TokenReview` and reads the namespace and service account. On Amazon EKS, the EKS Pod Identity agent does this step and the next.
3. The agent gets a token for them from the emulator's EKS cluster and calls `eks-auth:AssumeRoleForPodIdentity`.
4. The emulator returns credentials for the role `of-local-server`. The S3 driver renews them 5 minutes before they expire.

Sandbox Pods get no AWS credentials: the runner uses presigned URLs only.

## Before you start

- A cluster that enforces NetworkPolicy: OrbStack Kubernetes (the default), or a kind cluster that `make local-up` creates.
- Docker, Go (see `go.mod`), `kubectl`, AWS CLI v2, `openssl`, `curl` and `jq`.
- Access to `downloads.aws.com` and the image registries.
- About 6 GB of free memory.
- For `LOCAL_MODEL=claude`: a token from `claude setup-token`. For `LOCAL_FORGE=github`: a test repository and a fine-grained token for it.

## Start the stack

```bash
make local-up
```

`make local-up` runs `deploy/local/up.sh`. Each step is safe to run again:

1. Build the images `server`, `tool-claude` and `sandbox` (tag `local`) with `scripts/images/build.sh`, and the test images `of-local/testkit` and `of-local/tool-claude-standin`. Pods use them with the pull policy `Never`.
2. Build and start the AWS emulator (`of-local/aws-emulator`, `deploy/aws-local/up.sh`). Stop if `aws sts get-caller-identity` does not return the account `000000000000`.
3. Run the [provisioning script](#provisioning) against the emulator.
4. Start PostgreSQL and the git server.
5. Render the Helm chart with `deploy/local/values.yaml` and apply it. A Go renderer in `deploy/helm` does this, so Helm is not necessary.
6. Wait for both replicas. Create the first team and the admin `admin@of-local.test` with `POST /api/v1/bootstrap`. If the API key in `env` still works, keep it.
7. Write `env` and print a summary.

Files in `LOCAL_DIR` (default `~/.openfactory-local`):

| File | Contents |
|---|---|
| `env` | `OF_SERVER`, `OF_API_KEY`, `OF_PUBLIC_URL` and the stack settings. |
| `aws/connect.env` | AWS CLI settings. It removes real AWS credentials, so use it in a subshell: `( . ~/.openfactory-local/aws/connect.env && aws s3 ls )`. |
| `aws/ca.crt` | The CA of `tlsfront`. Add it to your browser to open presigned links. |
| `outputs.json` | The provisioning outputs. |
| `secret.key`, `db.password`, `webhook.secret` | `OF_SECRET_KEY`, the PostgreSQL password and the webhook secret. |

```bash
eval "$(make -s local-env)"
openfactory factory list
```

Sign in at <http://of.k8s.orb.local/login> with `OF_API_KEY`. `make local-status` prints the stack health, then `RESULT: HEALTHY` or `RESULT: UNHEALTHY`.

### Settings

Set these variables before `make local-up`:

| Variable | Default | Description |
|---|---|---|
| `LOCAL_SERVER_MODE` | `chart` | `chart`: the server runs in the cluster. `host`: two processes on the workstation (ports 18081, 18082) with the static emulator keys, for a debugger. Checks that need `chart` are `SKIP`. |
| `LOCAL_MODEL` | `fake` | `fake`: the [stand-in](#claude-stand-in). `claude`: the image also holds the real CLI as `claude-real`, used by runs with a model credential (check R4). |
| `LOCAL_FORGE` | `none` | `none`: the local git server. `github`: adds the GitHub checks. Needs `LOCAL_GITHUB_REPO=<owner>/<repo>`. |
| `KUBE_CONTEXT` | `orbstack` | The only context used. `kind-<name>`: a kind cluster. |
| `LOCAL_DOMAIN` | `k8s.orb.local` (kind: `127.0.0.1.nip.io`) | Domain of `aws.`, `db.`, `git.` and `of.`. |
| `LOCAL_DIR` | `~/.openfactory-local` | Files of the stack. |
| `LOCAL_BUILD` | `1` | `0` uses existing images. |
| `LOCAL_RETENTION` | `5m` | `OF_OBJECT_STORE_RETENTION`, swept every minute. Set for example `2h` to keep runs for the dashboard. R3 then fails until a sweep runs: repeat it later, or set `LOCAL_R3_WAIT=1` for `make local-check`. |

## Test on a kind cluster

kind runs on Linux and in CI, where OrbStack does not. You need `kind` on the `PATH` and Internet access for Calico and the test Pod images.

```bash
KUBE_CONTEXT=kind-of-local make local-up
make local-check
```

- If the cluster does not exist, `make local-up` creates it from `deploy/local/kind.yaml` with Calico, because the default kind network plugin does not enforce NetworkPolicy. Your current `kubectl` context does not change.
- `make local-up` loads the local images into the cluster, for the node platform only.
- Each Service gets a fixed node port, mapped to the same port on `127.0.0.1`: 80 (server), 9443 (emulator), 5432 (PostgreSQL), 9418 (git). These ports must be free.
- On the workstation, public DNS resolves every `*.127.0.0.1.nip.io` name to `127.0.0.1`. In Pods, a `hosts` block that `make local-up` adds to CoreDNS answers the same names with the Service addresses.
- `LOCAL_SERVER_MODE=host` is not available: sandbox Pods reach host servers through `host.orb.internal`, which only OrbStack has.
- `make local-down` keeps the cluster. Delete it with `kind delete cluster --name of-local`.
- Calico drops blocked packets, so a blocked connection times out (`curl` exit code 28). OrbStack rejects them at once (exit code 7).

If `nip.io` names do not resolve on your workstation, `make local-up` stops and prints a line for `/etc/hosts`. Or use another domain:

```text
127.0.0.1 aws.of-local.test db.of-local.test git.of-local.test of.of-local.test
```

```bash
KUBE_CONTEXT=kind-of-local LOCAL_DOMAIN=of-local.test make local-up
```

## Provisioning

`deploy/aws/provision.sh` creates the AWS resources of an install. The calls and policies are the same for the emulator and a real account.

```bash
AWS_TARGET=emulator sh deploy/aws/provision.sh   # make local-up runs this
AWS_TARGET=aws AWS_PROFILE=<profile> AWS_REGION=<region> sh deploy/aws/provision.sh
```

| Resource | Name (`PREFIX=of-local`) | Notes |
|---|---|---|
| VPC, two subnets in two Availability Zones | tag `of-local` | EKS needs two zones. |
| EKS cluster | `of-local` | |
| IAM role of the cluster | `of-local-cluster` | Trusts `eks.amazonaws.com`. |
| IAM role of the server | `of-local-server` | Trusts `pods.eks.amazonaws.com` (`sts:AssumeRole`, `sts:TagSession`). Allows `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket` on the run data bucket only. |
| Pod Identity association | `of-local` / `openfactory` | Server service account to server role. |
| S3 bucket | `of-local-runs` | Block Public Access and default encryption on. |
| ECR repositories | `openfactory`, `tool-claude`, `tool-codex`, `sandbox` | |
| DB subnet group, RDS instance | `of-local` (PostgreSQL 16) | |

- The script writes `outputs.json`. It never deletes or changes an existing resource: it reuses a match and fails on a mismatch.
- On a real account, it lists the costly resources and asks first (`ASSUME_YES=1` skips this). RDS keeps the master password in AWS Secrets Manager. No node group, DNS name or internet route is made; see [Kubernetes](kubernetes.md).

## Run the checks

```bash
make local-check                          # all checks
LOCAL_CHECKS=R1,R2,A2 make local-check    # some checks
```

- The first line names the log directory, `$LOCAL_DIR/check-logs/<timestamp>/`. Logs mask the known secrets.
- Each check prints `PASS <ID> <name>`, or `FAIL` or `SKIP` with a reason. A check that the settings exclude is `SKIP`, not `FAIL`.
- The last line is `RESULT: PASS` or `RESULT: FAIL`. Exit status: 0 pass, 1 fail, 2 wrong setup.

| ID | Check | Needs |
|---|---|---|
| A1 | Provisioning succeeds, and a second run changes nothing. | |
| A2 | The server writes run data with Pod Identity credentials. Its Pods have no `AWS_ACCESS_KEY_ID`. | `chart` |
| A3 | The server renews its Pod Identity credentials before they expire. | `chart` |
| A4 | A service account without a Pod Identity association gets no credentials. | `chart` |
| P1 | Both server replicas are ready. | |
| P2 | The database migrations ran once. | |
| P3 | The Kubernetes preflight and NetworkPolicy probe passed. One replica holds the leader lease. | |
| S1 | The live tests of `internal/objectstore` pass against the emulator (as `make test-aws-local`). | |
| S2 | A sandbox Pod opens a presigned URL that the server made. | |
| R1 | A `claude` run puts its transcript, an artifact and its snapshot in S3. | |
| R2 | After an idle timeout of 0, a follow-up starts a new Pod that restores the snapshot (with its `harness_state` part) and resumes the same harness session. | |
| R3 | A prompt attachment is uploaded, used, and deleted by retention. | |
| R4 | A run with the real `claude` CLI reads the rule files and answers. | `claude` |
| N1 | The `TestK8sNet*` tests pass. A restricted run passes the egress wait and shows a blocked host in its diagnostics. | `chart` |
| N2 | A sandbox Job that passes its deadline fails with a clear failure code (`TestK8sDeadline`). | |
| N3 | A Role without `jobs` makes the preflight fail with a clear message (`TestK8sRoleWithoutJobs`). | |
| H1 | When the leader stops during a turn, the turn finishes and the other replica takes the lease. | `chart` |
| H2 | A rolling update during a turn does not stop the turn. | `chart` |
| W1 | A webhook `ping` gets `200`. | |
| W2 | A delivery with a bad signature gets `401`. | |
| W3 | A repeated delivery starts no second turn. | |
| D1 | A managed definition: apply, new version, invalid change refused, restore. | |
| G1 | An issue with the label `factory:<alias>` makes a pull request. | `github` |
| G2 | A review comment makes a follow-up commit and a reply in the thread. | `github` |
| G3 | A merge closes the work item. | `github` |
| G4 | A pull request that changes the definition gets the `openfactory/definition` status. | `github` |
| UI1 | The dashboard pages load and show the runs of the checks. | |

Runs use the stand-in and cost nothing, except one short R4 turn on your Claude subscription. The checks never print a secret.

### Run the checks on Amazon EKS

`LOCAL_TARGET=eks` runs `scripts/local/check.sh` against an install on a real EKS cluster, with real S3 and EKS Pod Identity. `make local-up` does not make this install. Install the chart yourself with the namespaces of the local stack: the server in `of-local` (release `openfactory`), sandboxes in `of-local-sandboxes`, and the git server of `deploy/local/infra.yaml` in `of-local-infra`.

Write `$LOCAL_DIR/env` (use a separate `LOCAL_DIR`) with `OF_SERVER`, `OF_API_KEY`, `KUBE_CONTEXT`, `LOCAL_TARGET=eks`, and these settings:

| Variable | Value |
|---|---|
| `LOCAL_CHECK_IMAGE` | The testkit image (`deploy/local/testkit/Dockerfile`) in your registry. |
| `LOCAL_PULL_POLICY`, `E2E_PULL_POLICY` | `IfNotPresent`. |
| `LOCAL_PG_HOST` | The database host. The checks run `psql` in a Deployment `postgres` in `of-local-infra`, for example the image `postgres:16` with the command `sleep infinity`. |
| `LOCAL_GIT_PORT` | The local port of a `kubectl port-forward` to the git Service, with `LOCAL_DOMAIN=127.0.0.1.nip.io`. |
| `E2E_EGRESS_WAIT_ADDR` | The same value as `OF_K8S_EGRESS_WAIT_ADDR` of the server. See [Network policy](network-policy.md). |

`$LOCAL_DIR` also holds `db.password`, `webhook.secret` and `outputs.json` (`accountId`, `region`, `clusterName`, `bucket`). `KUBE_CONTEXT` must use a static token, because the Go cluster tests refuse a kubeconfig with an exec plugin. The checks A1 and A3 are `SKIP` on EKS. A2, A4, S1 and S2 use the real services.

## Cluster tests without the stack

The Go tests of `make e2e-k8s` (`internal/sandbox/k8stask`, build tag `e2e_k8s`) check the limits of sandbox Jobs. Checks N1 to N3 run them on the stack. They also run on any cluster where you may create a namespace, with no Helm and no model. For the backend, see [Kubernetes](kubernetes.md).

| Test | What it checks |
|---|---|
| `TestK8sDeadline` | A Job whose control URL does not answer passes its 8-second deadline: end reason `deadline_exceeded`, failure code `infrastructure_timeout`. Kubernetes removes the Pod, then the Job. |
| `TestK8sToolImages` | A tool image is copied into a Pod that does not run as root, and its `bin` is on the `PATH`. |
| `TestK8sRoleWithoutJobs` | Without the `jobs` rule, `Preflight` and `openfactory serve` stop with an error that names `jobs.batch (create, delete, get, list, watch)`. |
| `TestK8sNetEnforcementProbe` | The backend probe matches the test's own NetworkPolicy measurement and leaves nothing behind. Without enforcement, `OF_K8S_NETWORK_POLICY_ENFORCEMENT=require` refuses a restricted sandbox, and `warn` starts it with a warning. |
| `TestK8sNetEgressProxy` | The proxy sidecar gives `200` for the allowed host, `403` for another, and reports the blocked host. |
| `TestK8sNetEgressLock` | `OF_K8S_EGRESS_LOCK=iptables` stops a program that ignores the proxy variables. The proxy path still works. |
| `TestK8sNetPolicyBlocksDirectEgress` | A restricted sandbox cannot connect directly to ports other than 80 and 443 (with the lock, to any port), also with its first command, because the init container `egress-wait` waits for the policy. Skips without enforcement; on kind with Calico, N1 then fails. |
| `TestK8sNetPolicyWaitTimeout` | A late policy ends the sandbox with `network_policy_timeout` (`egress-wait` exit code 75) before the sandbox container starts. |

Each test makes and deletes its own namespace (`of-e2e-*` or `of-net-*`). The network tests pull `nginx:1.27-alpine` and `curlimages/curl:8.10.1`.

The server image is also the runner image:

```bash
OF_E2E_KUBE_CONTEXT=<context> \
OF_E2E_RUNNER_IMAGE=ghcr.io/openfactory-org/openfactory:<tag> \
OF_E2E_PROXY_IMAGE=ghcr.io/openfactory-org/openfactory:<tag> \
OF_E2E_TOOL_IMAGE=ghcr.io/openfactory-org/tool-claude:<tag> \
  make e2e-k8s
```

| Variable | Meaning |
|---|---|
| `OF_E2E_KUBE_CONTEXT` | Required. The only context used. `OF_E2E_KUBECONFIG` sets the file. |
| `OF_E2E_RUNNER_IMAGE` | Server image for `TestK8sDeadline` and `TestK8sToolImages`. They skip without it. |
| `OF_E2E_PROXY_IMAGE` | Server image (`openfactory` and `iptables`) for the network tests. They skip without it. |
| `OF_E2E_TOOL_IMAGE` | Tool image for `TestK8sToolImages`. `OF_E2E_TOOL_COMMAND`: its program (default `claude`). `OF_E2E_TASK_IMAGE`: the sandbox image (default `ubuntu:22.04`). |
| `OF_E2E_PULL_POLICY` | Default `IfNotPresent`. `Never` only when every image is on the nodes. |
| `OF_E2E_ARCH` | `amd64` or `arm64`. Default: the test machine's. |
| `OF_E2E_CLIENT_IMAGE`, `OF_E2E_NGINX_IMAGE`, `OF_E2E_WEB_COMMAND` | Other network test images. The server image works as the client. |
| `OF_E2E_NETPOL_ENFORCED` | `true` or `false`. A different measurement fails the tests. |
| `OF_E2E_CA_FILE` | Cluster CA for the startup test, when the API address is not local. |
| `OF_E2E_SERVER_BIN` | Binary for the startup test. Default: build `./cmd/openfactory`. |
| `OF_E2E_KEEP`, `OF_E2E_NO_WAIT` | `1` keeps the namespaces; `1` turns the egress wait off (a negative control). |

Expected result: `--- PASS` for each test, then `PASS`. Then `kubectl --context <context> get ns | grep -E 'of-e2e|of-net'` must print nothing.

## Tests with PostgreSQL or S3

Some Go tests need a real database or S3-compatible store, and skip without their variable. Docker is enough.

PostgreSQL (store and upgrade tests):

```bash
docker run -d --name of-test-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=of_test postgres:16
OF_TEST_POSTGRES_DSN='postgres://postgres:postgres@localhost:55432/of_test?sslmode=disable' \
  go test -count=1 ./internal/store/...
docker rm -f of-test-pg
```

S3 pagination (more than 1000 objects, which the emulator cannot hold). Wait a few seconds after the start before you create the bucket:

```bash
docker run -d --name of-test-s3 -p 127.0.0.1:8333:8333 \
  -e AWS_ACCESS_KEY_ID=of-test -e AWS_SECRET_ACCESS_KEY=of-test-secret \
  chrislusf/seaweedfs:4.53 server -dir=/data -s3
docker exec of-test-s3 sh -c 'echo "s3.bucket.create -name of-test" | weed shell'
AWS_ACCESS_KEY_ID=of-test AWS_SECRET_ACCESS_KEY=of-test-secret \
OF_TEST_S3_URL='s3://of-test?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true' \
  go test -count=1 -run Live -v ./internal/objectstore
docker rm -f of-test-s3
```

`TestLiveS3ListPagination` must pass, not skip.

## Model and forge

### Claude stand-in

`deploy/local/testkit` holds a stand-in for the `claude` CLI that speaks the stream-json format of `claude -p` with fixed answers. The stack sets its tool image `of-local/tool-claude-standin:latest` as `OF_CLAUDE_TOOL_IMAGE`. The test factories (`scripts/local/factories`) use `model: claude-sonnet-5-5`.

| Directive in the prompt | Result |
|---|---|
| none | Reply `READY`. |
| `[[reply: TEXT]]` | Reply `TEXT`. |
| `[[tool: bash {"command": ...}]]` | One `bash` call. |
| `[[tool: write_file {"path": ..., "content": ...}]]` | One `write_file` call. |
| `[[tool: read_file {"path": ...}]]` | One `read_file` call. |
| `[[tool: mcp__<server>__<tool> {...}]]` | One call to a tool of an HTTP MCP server. |

Tool directives run in order, win over a reply directive, and end with the reply `DONE`. An unknown tool gives the result `NO_TOOL <name>`.

### Real Claude

With `LOCAL_MODEL=claude`, store your token as a team secret before `make local-check`:

```bash
eval "$(make -s local-env)"
openfactory secret set CLAUDE_OAUTH_TOKEN --type anthropic_oauth_token
```

Without a value argument, the command reads standard input until Ctrl-D, so the token stays out of shell history. The input is not hidden.

### Local git server

With `LOCAL_FORGE=none`, runs clone from and push to `git://git.k8s.orb.local:9418/<repo>.git`. It has no pull requests.

### GitHub

With `LOCAL_FORGE=github`:

1. Set `LOCAL_GITHUB_REPO=<owner>/<repo>` before `make local-up`.
2. Store a fine-grained token for that repository only: `openfactory secret set GITHUB_TOKEN`.
3. Export `LOCAL_GITHUB_REPO` and `GITHUB_TOKEN` for `make local-check` too. The checks read the repository with the GitHub API. Without them, G1 to G4 are `SKIP`.

GitHub cannot reach the workstation, so the checks send signed deliveries with GitHub's payloads.

## Limits

These limits of the AWS emulator hold for every release so far.

| Limit | Effect |
|---|---|
| State in memory | An emulator Pod restart deletes every resource and makes new keys. `make local-status` detects it: run `make local-up`. |
| Some permission checks only | Identity policies of assumed-role sessions apply, so the `of-local-server` policy matters. Trust and bucket policies do not, and the emulator's own keys can do everything. A4 tests the association, not a trust policy. |
| One account, one Region | `000000000000`, `us-east-1`. |
| 1000 objects per bucket | The next write fails with `ValidationException`. The stack sets retention and skips the pagination test (`OF_TEST_S3_MAX_OBJECTS=1000`). Test pagination with [another store](#tests-with-postgresql-or-s3). |
| No anonymous S3 access | Every request must be signed. |
| No outbound connections | Web identity tokens verify only for its own Amazon Cognito issuers. The Open Factory issuer gets `InvalidIdentityToken`, so `cloudProviders.aws` cannot be tested. An IAM OIDC provider needs `--thumbprint-list`. |
| Not a database, node or registry | RDS refuses `pg_advisory_xact_lock` and `CREATE SCHEMA`. EKS has no nodes. ECR has no registry. |

Before you rely on another AWS operation, run the emulator's compatibility check for it. `NotImplemented` (HTTP 501) is an emulator gap, not a configuration error.

## Stop the stack

```bash
make local-down
```

`make local-down` stops host-mode servers and deletes the four namespaces, with all AWS state, the database and the runs. It deletes `env` and `outputs.json`, and keeps the CA and secrets. Delete `~/.openfactory-local` to start from nothing.

## Troubleshooting

| Problem | Cause and fix |
|---|---|
| `make local-up` stops at "not reachable at https://aws.k8s.orb.local:9443" | The name does not resolve to the Service. On kind, it must resolve to `127.0.0.1`, and port 9443 must be free. |
| `x509: certificate signed by unknown authority` from the S3 driver | The server or a Pod does not trust `aws/ca.crt` (the chart sets `AWS_CA_BUNDLE`; sandbox Pods use `SSL_CERT_DIR`). Delete the CA and run `make local-up` again. |
| `ValidationException`, "maximum of 1000 objects" | The bucket is full. Run `make local-down` and `make local-up`, or lower `LOCAL_RETENTION`. |
| `NoSuchBucket` or `InvalidClientTokenId` after a pause | The emulator restarted and lost its state. Run `make local-up`. |
| A2 fails with `InvalidTokenException` | The emulator refuses the agent's token. Read `kubectl -n of-local logs deploy/openfactory -c pod-identity-agent`. |
| `make local-up` stops with "already bootstrapped" | `env` has no working API key. Run `make local-down`, then `make local-up`. This deletes the database and all runs. |

## Real-world checks

The local stack does not test:

- The EKS Pod Identity agent on real nodes, and real IAM, bucket and trust policies.
- Cloud identity for agents (checks 4 and 4b).
- GitHub webhooks over the internet, GitHub App check runs and account linking.
- VPC CNI packet drops. OrbStack rejects blocked packets, so the egress wait finishes faster.
- S3 pagination beyond 1000 objects, a Helm install, a node drain and the PodDisruptionBudget.
- The real `codex` program, and long turns with the real `claude` program.

The checks below cover some of these. CI cannot run them. Run them on your own machine before a release.

| # | Check | Needs | Time | Command |
|---|---|---|---|---|
| 1 | Build and test every image | Docker or Podman | 10–30 min | `make images-check` |
| 3 | A push 2 hours after the sandbox started | Server, forge connection, test repository | About 2 h 15 min | `sh scripts/checks/real-world.sh forge-refresh` |
| 4 | A cloud read at the end of a 90-minute turn | Server, AWS or Google Cloud account | About 1 h 45 min | `sh scripts/checks/real-world.sh cloud-refresh` |
| 4b | Cloud identity against a real AWS account | AWS account that allows IAM OIDC providers | About 15 min | `go test -tags aws_live ./internal/cloudid` |
| 5 | One turn with the real `claude` and `codex` (optional) | Server, an API key per program | A few minutes | `sh scripts/checks/real-world.sh harness-turn` |

- The scripts are POSIX `sh`. Checks 3 to 5 also need `curl` and `jq`.
- No script holds an account or a secret, and none prints a secret. You give every value in an environment variable.
- Each step prints `PASS` or `FAIL`. The end is `<N> checks passed, <M> failed` and `RESULT: PASS` or `RESULT: FAIL`.
- `real-world.sh` exits 0 (pass), 1 (fail) or 2 (wrong setup). `OF_CHECK_DRY_RUN=1` prints the request and starts nothing.

### Check 1: Build and test every image

**Proves:** every Dockerfile builds, and each image holds the programs that the platform needs.

**Needs:** a running Docker or Podman (Docker first; `CONTAINER_ENGINE=podman` to choose) and Internet access. The large sandbox image makes the first build slow.

```bash
make images-check                  # or: sh scripts/images/build.sh all
make images                        # build only
make images-smoke                  # test existing images only
make images-check IMAGES="server tool-claude"
sh scripts/images/build.sh list    # names and tags
```

The script uses the Dockerfiles and build arguments of CI and releases, and the Makefile image names with `IMAGE_TAG` (default: `git describe`). It never pushes.

| Image | Dockerfile | Smoke test |
|---|---|---|
| `server` | `deploy/docker/Dockerfile` | Build version, user 10001, `git`, `ssh`, `iptables`, plugins in `OF_PLUGIN_DIR`, `netguard` answers `403` for a denied host. As the runner image: `openfactory runner` exists, and the static binary copied by user 1000 runs in a Debian image as another user. |
| `sandbox` | `deploy/sandbox/Dockerfile` | glibc, `sh`, `cp`, `git`, `node`, `gh`, user `agent`, `/workspace`. No `claude` or `codex`. |
| `tool-claude` | `images/tool-claude/Dockerfile` | `sh`, `cp`, `node`, and `claude` in `/opt/openfactory/tool/bin`. User 1000 can run `cp -RP /opt/openfactory/tool/.`. |
| `tool-codex` | `images/tool-codex/Dockerfile` | As `tool-claude`, for `codex`. |
| `tool-example` | `images/tool-example/Dockerfile` | `sh` and `cp` exist. The tool script runs. |
| `release` (only when named) | `deploy/docker/Dockerfile.release` | As `server`. Needs Go: the script builds the Linux binaries first. |

**Expected result:**

```text
==> smoke server (ghcr.io/openfactory-org/openfactory:<tag>)
  PASS  openfactory version prints the version
  ...
N checks passed, 0 failed
RESULT: PASS
```

A failed step shows the command output under its `FAIL` line. The script continues, then exits with 1.

This check runs no sandbox. The [local stack checks](#run-the-checks) and `TestK8sToolImages` do. To test the real `claude` harness in an `ubuntu:22.04` sandbox, start a run by hand.

### Check 3: A push 2 hours after the sandbox started

**Proves:** the forge credential refreshes. A forge App token lives 1 hour. The agent waits 2 hours, then pushes a branch. Without a refresh, the push fails.

**Needs:**

- A server (`OF_SERVER`) and an editor or admin API key (`OF_API_KEY`).
- A test factory with one repository, a forge connection with short-lived tokens (for example a GitHub App), and an agent that runs shell commands and can push (for example `implement`). The agent must reach the forge host ([Network policy](network-policy.md#required-hosts)).
- `OF_MAX_SESSION_DURATION` (default `8h`) above 3 hours.
- Local git that can read the repository (`git ls-remote`).

```bash
export OF_SERVER=https://factories.example.com
export OF_API_KEY=...                       # your key
export OF_CHECK_FACTORY=my-test-factory     # UID, name or alias
export OF_CHECK_AGENT=implement             # optional, default: the foreman
export OF_CHECK_REPO_URL=https://forge.example.com/acme/test-repo.git

sh scripts/checks/real-world.sh setup                                       # checks the key and the factory
OF_CHECK_WAIT_SECONDS=300 sh scripts/checks/real-world.sh forge-refresh     # 5-minute rehearsal
sh scripts/checks/real-world.sh forge-refresh                               # the real check
```

Rehearse first to find setup mistakes. A wait below 3700 seconds proves nothing, and the script says so.

The agent runs `sleep 240` until 7200 seconds have passed (about 30 short model calls), prints `OF_CHECK_ELAPSED=<seconds>`, then pushes one commit to the branch `of-check-<time>`. Keep the terminal open. If the script stops, set `OF_CHECK_RUN_ID` and `OF_CHECK_BRANCH` and run it again to watch the same run.

**Expected result:**

```text
  PASS  the run finished with the state SUCCEEDED
  PASS  the agent pushed 7243 seconds after it started the wait (needed: 7200)
  PASS  the branch of-check-1794443011 exists on the remote (3f2a9c1d8e0b)
    wall time of this script: 7391 seconds. Delete the branch when you are done: git push <repo url> --delete of-check-1794443011

3 checks passed, 0 failed
RESULT: PASS
```

A failed push shows an authentication error in `openfactory run logs <run id>`: the token did not refresh. Delete the branch at the end.

### Check 4: A cloud read at the end of a 90-minute turn

**Proves:** the cloud identity of a run refreshes. A cloud identity token lives 1 hour. The agent waits 90 minutes, then makes its first cloud call: a read of an object with a value that you chose.

**Needs:**

- A server, API key and test factory, as in check 3.
- The cloud in the factory's [`cloudProviders`](../definition-reference.md#cloudproviders). The cloud account trusts the server issuer, and reaches its URL (`OF_OIDC_ISSUER_URL`, else `OF_PUBLIC_URL`) over HTTPS.
- A test object with a random one-line value, and a role that can read it:

  ```bash
  openssl rand -hex 16 > /tmp/of-check-value
  aws s3 cp /tmp/of-check-value s3://my-test-bucket/of-check.txt           # AWS
  gcloud storage cp /tmp/of-check-value gs://my-test-bucket/of-check.txt   # Google Cloud
  ```

- `aws` or `gcloud` in the agent's sandbox image (the default image has neither), or `OF_CHECK_CLOUD_COMMAND` set to another command that prints the object.
- `OF_MAX_SESSION_DURATION` above 3 hours.

```bash
export OF_CHECK_CLOUD=aws                    # or gcp
export OF_CHECK_BUCKET=my-test-bucket
export OF_CHECK_OBJECT=of-check.txt
export OF_CHECK_EXPECTED="$(cat /tmp/of-check-value)"

OF_CHECK_WAIT_SECONDS=300 sh scripts/checks/real-world.sh cloud-refresh   # rehearsal
sh scripts/checks/real-world.sh cloud-refresh                             # the real check
```

The agent prints `OF_CHECK_ELAPSED=<seconds>` and `OF_CHECK_RESULT=<content>`. The script looks for the exact value in the transcript. The agent never sees it, so it cannot invent it.

**Expected result:**

```text
  PASS  the run finished with the state SUCCEEDED
  PASS  the agent read the object 5421 seconds after it started the wait (needed: 5400)
  PASS  the object read from the bucket has the expected content

3 checks passed, 0 failed
RESULT: PASS
```

If the read fails, `ExpiredToken` or `invalid_grant` in the tool result means no refresh. `AccessDenied` or a missing command is a setup problem. Delete the object and the role at the end.

### Check 4b: Cloud identity against a real AWS account

Needs only an AWS account where you can create an IAM OIDC provider: no server, no sandbox. It uses the real token code in `internal/cloudid`.

1. Write the issuer documents for a random seed:

   ```bash
   export OF_AWS_LIVE_SEED=$(openssl rand -hex 32)
   export OF_AWS_LIVE_REGION=<region>
   export OF_AWS_LIVE_ISSUER=https://<issuer-bucket>.s3.<region>.amazonaws.com
   OF_AWS_LIVE_OUT=./issuer go test -tags aws_live -run TestAWSLivePublish -v ./internal/cloudid
   ```

2. Upload the two files of `./issuer/.well-known/` to the issuer bucket. Allow public read of `.well-known/*` only.
3. Create an IAM OIDC provider for the issuer URL, audience `sts.amazonaws.com`.
4. Create a role that trusts it for the subject `team/team_live/factory/fac_live/agent/reader`, with `s3:GetObject` on one test object.
5. Run the check. It needs the AWS CLI on the `PATH`, but no profile:

   ```bash
   OF_AWS_LIVE_ROLE_ARN=arn:aws:iam::<account>:role/<role> \
   OF_AWS_LIVE_OBJECT=s3://<data-bucket>/<key> OF_AWS_LIVE_EXPECT=<file content> \
   go test -tags aws_live -run TestAWSLiveIdentity -v -timeout 15m ./internal/cloudid
   ```

Expected result: four `PASS` lines. Run credentials read the object. A token of another agent and an expired token are refused. A fresh token in the same file reads the object again.

Delete the role, the provider and both buckets afterwards. This check cannot run where a service control policy denies `iam:CreateOpenIDConnectProvider`.

### Check 5 (optional): One turn with the real `claude` and `codex` programs

**Proves:**

- The real programs start in a sandbox and finish one turn.
- With `OF_CHECK_RULE_MARKER`: each program reads the repository rule file by itself.
- With `OF_CHECK_RESUME=1`: a follow-up after the sandbox ended starts a new session that still knows the code word of the first message.

**Needs:**

- A server, API key and test factory, as in check 3.
- An API key per program as a managed secret (`openfactory secret set <secret name>`, value on standard input), named in `OF_CHECK_CLAUDE_SECRET` and `OF_CHECK_CODEX_SECRET`. A secret or model makes the run use a harness of that type with only these settings. Without them, the agent must already use the harness under test, with its own `auth`.
- The program in the sandbox. On Kubernetes, the `tool-claude` and `tool-codex` images deliver it into any glibc sandbox image. Mirror them to a registry that your cluster can read ([Private registries](private-registries.md)).
- For the rule check: the line `End every reply with the word RULEMARK.` in `CLAUDE.md` and `AGENTS.md` of the test repository, and `OF_CHECK_RULE_MARKER=RULEMARK`.

```bash
export OF_CHECK_CLAUDE_SECRET=<secret name>
export OF_CHECK_CODEX_SECRET=<secret name>
export OF_CHECK_RULE_MARKER=RULEMARK         # optional
export OF_CHECK_RESUME=1                     # optional

sh scripts/checks/real-world.sh harness-turn             # both programs
sh scripts/checks/real-world.sh harness-turn claude      # one program
```

Optional: `OF_CHECK_CLAUDE_MODEL`, `OF_CHECK_CODEX_MODEL`, and `OF_CHECK_TURN_TIMEOUT` (default 900 seconds). With `OF_CHECK_RESUME=1`, the idle timeout is 1 minute, and the script waits up to `OF_CHECK_RESUME_WAIT` seconds (default 600) for the sandbox to end. The cost is a few short turns.

**Expected result:**

```text
==> one turn with the real claude CLI
  PASS  the run finished with the state SUCCEEDED
  PASS  the run used the claude harness
  PASS  claude answered the prompt (the real CLI ran in the sandbox)
  PASS  claude followed the rule file of the repository (the reply has RULEMARK)
  PASS  the follow-up started a new session (outcome new_session)
  PASS  claude remembered the code word in the new sandbox (the conversation was restored)
...
RESULT: PASS
```

### After the checks

- Keep the output of each check. A bug report needs the `FAIL` line and the run ID.
- Delete the test branches, the test object, the managed secrets and the test factory.
- Fix the first real problem, then run the same check again. Do not change a script to make a result pass.

## Related pages

- [Configuration](configuration.md)
- [Private registries](private-registries.md)
- [Cloud providers in a definition](../definition-reference.md#cloudproviders)
- [Kubernetes](kubernetes.md)
- [Object storage](object-storage.md)
- [Network policy](network-policy.md)
