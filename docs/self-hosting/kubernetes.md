# Kubernetes

The Helm chart is the main supported install, and `kubernetes` is the only production sandbox backend. Each sandbox gets its own Job and Pod in a sandbox namespace. The runner (`openfactory runner`) runs in the Pod, runs the harness and talks to the control plane over HTTP with a session token. The control plane never runs a command in a Pod.

## Quick install

You need a Kubernetes cluster (k3s, kind, a local or a managed cluster) and Helm 3.8 or later. Install the chart from a checkout of the repository:

```bash
git clone https://github.com/openfactory-org/openfactory.git
cd openfactory
helm install openfactory deploy/helm/openfactory \
  --namespace openfactory --create-namespace
```

Available from the first release (no release is published yet): `helm install openfactory oci://ghcr.io/openfactory-org/charts/openfactory --namespace openfactory --create-namespace`.

With no values file, the chart installs one server in SQLite mode, with no ingress. A hook Job creates the master key in the Secret `openfactory-key`. **Back up this Secret.** Then create the first team, admin and API key:

```bash
kubectl -n openfactory rollout status statefulset/openfactory
kubectl -n openfactory port-forward svc/openfactory 8080:80
curl -s -X POST http://localhost:8080/api/v1/bootstrap \
  -H 'Content-Type: application/json' \
  -d '{"team_name":"Acme","admin_email":"admin@example.com","admin_name":"Admin"}'
```

Store the API key from the response: it is shown once. Open http://localhost:8080 and sign in with the key. The chart prints these commands after the install. For a team, add an ingress with TLS and [OIDC login](oidc.md) (see [Install with Helm](#install-with-helm)).

## Choose a database mode

Every replica serves the API, the dashboard and webhooks, and runs the engine and (on the lease holder) the scheduler. A replica holds no agent turn, so a restart or rolling update does not interrupt a turn.

| `database.mode` | Workload | Replicas | Storage |
|---|---|---|---|
| `sqlite` (default) | StatefulSet | Exactly 1 | A PersistentVolumeClaim holds the database and the local object store. |
| `postgres` | Deployment with a readiness probe, rolling updates (`maxUnavailable: 0`) and a PodDisruptionBudget | `server.replicaCount` | Your database (see [Use PostgreSQL](#use-postgresql)). With more than one replica, S3, GCS or Azure Blob for run data. |

Each replica gets `OF_REPLICA_ID` (for leases) from its Pod name.

## Install with Helm

Run these commands from the root of a repository checkout. The chart is in [`deploy/helm/openfactory/`](../../deploy/helm/openfactory/).

1. Optional: generate a master key. Without one, the chart creates it (see [Master key](#master-key)).

   ```bash
   openssl rand -base64 32
   ```

2. Create a values file. Pick one database example.

   SQLite mode (one replica):

   ```yaml
   # values.yaml
   secretKey: "<the key from step 1>"

   database:
     mode: sqlite
     sqlite:
       persistence:
         size: 20Gi

   server:
     publicURL: https://factories.example.com
   ```

   PostgreSQL mode (three replicas, S3 for run data):

   ```yaml
   # values.yaml
   secretKey: "<the key from step 1>"

   database:
     mode: postgres
     postgres:
       # A database that you supply. See "Use PostgreSQL".
       existingSecret: factory-db        # key "database-url": postgres://...
   server:
     replicaCount: 3
     publicURL: https://factories.example.com

   objectStore:
     url: s3://acme-factory-runs/prod?region=eu-west-1
   serviceAccount:
     annotations:
       eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/factory-runs
   ```

   Add the parts that both modes share:

   ```yaml
   sandbox:
     namespace: openfactory-sandboxes
     createNamespace: true

   oidc:
     enabled: true
     issuer: https://login.example.com
     clientID: openfactory
     existingSecret: factory-oidc        # key "client-secret"
     adminGroups: platform-admins

   ingress:
     enabled: true
     className: nginx
     hosts:
       - host: factories.example.com
         paths:
           - path: /
             pathType: Prefix
     tls:
       - secretName: factories-tls
         hosts: [factories.example.com]
   ```

3. Install the chart.

   ```bash
   helm upgrade --install openfactory deploy/helm/openfactory \
     --namespace openfactory --create-namespace \
     -f values.yaml
   ```

4. Make sure that the server Pods are ready.

   ```bash
   kubectl -n openfactory get pods -l app.kubernetes.io/component=server
   kubectl -n openfactory logs -l app.kubernetes.io/component=server
   ```

5. Sign in with OIDC, or create the first team, admin and API key. See [OIDC login](oidc.md) and [Security](../security.md#bootstrap-the-first-admin).

6. Store the harness model credential as a team secret before the first run, for example `ANTHROPIC_API_KEY` of type `anthropic_api_key`. See [Harnesses](../harnesses.md).

### Main values

| Value | Description |
|---|---|
| `secretKey` | The master key (`OF_SECRET_KEY`). Keep it stable across upgrades. Empty: the chart creates it (see [Master key](#master-key)). |
| `existingSecret` | A Secret with `OF_SECRET_KEY`, instead of `secretKey`. The server loads all `OF_*` keys of this Secret, so put `OF_DATABASE_URL`, `OF_OIDC_CLIENT_SECRET` and object storage credentials there too. |
| `database.mode` | `sqlite` (default) or `postgres`. |
| `database.sqlite.persistence.*` | Size, storage class and access modes of the volume claim. `existingClaim` reuses a PVC. |
| `database.postgres.url`, `database.postgres.existingSecret` | PostgreSQL URL, inline or from a Secret. See [Use PostgreSQL](#use-postgresql). |
| `server.replicaCount` | Replicas. Must be 1 in SQLite mode. |
| `server.podDisruptionBudget.*`, `server.topologySpread` | Availability of PostgreSQL mode with more than one replica. |
| `server.publicURL` | `OF_PUBLIC_URL`. Default: the first ingress host. |
| `server.internalURL` | `OF_INTERNAL_URL`. Default: the server Service, `http://<service>.<namespace>.svc:<port>`. |
| `server.oidcIssuerURL` | `OF_OIDC_ISSUER_URL`, issuer of cloud identity tokens. Default: the public URL. |
| `server.sandboxBackend` | `kubernetes` only. The chart refuses `local`, which runs agents in the server Pod without isolation. For development, run `openfactory serve` with `OF_DEV=true` on a workstation. |
| `server.logLevel`, `server.logFormat` | `OF_LOG_LEVEL`, `OF_LOG_FORMAT`. |
| `server.env`, `server.secretEnv` | Extra environment variables, for example `OF_LEADER_LEASE_TTL`. Use `secretEnv` for API keys. Integration credentials are team secrets, not server settings. |
| `runner.image` | `OF_RUNNER_IMAGE`. Default: the server image. See [Runner images](#runner-images). |
| `runner.claudeToolImage`, `runner.codexToolImage` | `OF_CLAUDE_TOOL_IMAGE`, `OF_CODEX_TOOL_IMAGE`. See [Tool images](#tool-images). |
| `objectStore.*` | Run data storage. See [Object storage](object-storage.md). |
| `oidc.*` | Single sign-on. See [OIDC login](oidc.md). |
| `plugins.envAllow`, `plugins.env`, `plugins.images` | `OF_PLUGIN_ENV_ALLOW`, `OF_PLUGIN_ENV` and plugin images. See [Plugins](plugins.md#configure-plugins). |
| `serviceAccount.annotations` | Workload identity for object storage (IRSA, GKE Workload Identity). For EKS Pod Identity, create an association instead. See [Amazon S3 credentials](object-storage.md#amazon-s3-credentials). |
| `sandbox.namespace`, `sandbox.createNamespace` | Namespace for sandbox Jobs and Pods. Empty means the release namespace. |
| `sandbox.rbac.create` | Create the [RBAC](#rbac) Role and RoleBindings (default `true`). |
| `sandbox.image` | `OF_SANDBOX_IMAGE`. It must contain `sh` and `git`. |
| `sandbox.podTemplate` | [Pod template](#pod-template) (YAML). The chart puts it in a ConfigMap and sets `OF_K8S_POD_TEMPLATE`. |
| `sandbox.k8s.*` | See [Settings of sandbox Jobs](#settings-of-sandbox-jobs). |
| `worker.*` | An optional remote worker in the cluster: `enabled`, `id`, `capacity`, `apiKey` or `existingSecret`, `serverURL`, and `metricsAddr` (`OF_METRICS_ADDR`, empty disables it). |
| `ingress.*` | Ingress for the dashboard, API, webhooks and worker connections. |

[`values.yaml`](../../deploy/helm/openfactory/values.yaml) has the complete list. Every server environment variable is in [Configuration](configuration.md).

The chart refuses to render with: an unknown `database.mode`; `server.replicaCount` below 1, or above 1 in SQLite mode; PostgreSQL mode without a database URL; more than one PostgreSQL replica without `objectStore.url`; `oidc.enabled` without an issuer and a client ID; a `server.sandboxBackend` other than `kubernetes`.

Sandbox Pods reach the control plane at `OF_INTERNAL_URL`, by default the server Service. So this traffic stays in the cluster, also when the public URL is behind single sign-on or an IP allowlist. A custom `server.internalURL` must be reachable from the sandbox namespace. An ingress must forward WebSocket upgrades for `/api/v1/workers/connect`.

The server creates sandbox Jobs itself, so `worker.enabled` is optional. Enable it when the server runs in another cluster, or to route selected agents to a separate worker id, for example on a dedicated node pool. See [Workers](workers.md).

### Master key

The server encrypts stored secrets and signs sessions with the master key (`OF_SECRET_KEY`). The chart takes the key from one of three places:

1. `secretKey`: the chart puts it in its own Secret.
2. `existingSecret`: a Secret that you create, with the key `OF_SECRET_KEY`.
3. Neither: a hook Job creates the Secret `<release>-openfactory-key` (`openfactory-key` when the release is named `openfactory`).

The hook Job runs before each install and upgrade, with the server image and a ServiceAccount that can only create Secrets in the release namespace. It never changes a Secret that exists. So `helm upgrade` and a GitOps tool that runs the hooks keep the key, and `helm template` prints no key.

`helm uninstall` keeps the Secret. Back it up with your data. **If you lose it, the stored secrets cannot be decrypted.** To use your own key, create the Secret before the first install:

```bash
kubectl -n openfactory create secret generic openfactory-key \
  --from-literal=OF_SECRET_KEY="$(openssl rand -base64 32)"
```

## Use PostgreSQL

The chart has no bundled PostgreSQL. Set `database.mode: postgres` and one of:

- `database.postgres.url`: the URL. The chart stores it in its Secret.
- `database.postgres.existingSecret`: a Secret with the URL. `database.postgres.existingSecretKey` names the key (default `database-url`).

A managed database from your cloud provider:

```bash
kubectl -n openfactory create secret generic factory-db \
  --from-literal=database-url='postgres://openfactory:<password>@db.example.com:5432/openfactory?sslmode=require'
```

```yaml
database:
  mode: postgres
  postgres:
    existingSecret: factory-db
```

A PostgreSQL operator usually creates a Secret with the connection URL. Point the chart to that Secret and key:

```yaml
database:
  mode: postgres
  postgres:
    existingSecret: factory-pg-app
    existingSecretKey: uri
```

## Kubernetes backend configuration

The chart sets these. All other settings are in [Configuration](configuration.md#sandbox-backend).

| Variable | Default | Description |
|---|---|---|
| `OF_K8S_NAMESPACE` | namespace of the kubeconfig context or service account, else `default` | Namespace for sandbox Jobs and Pods. |
| `OF_K8S_POD_TEMPLATE` | none | Path to a [pod template](#pod-template) file (YAML or JSON). |
| `OF_KUBECONFIG` | see [Cluster credentials](#cluster-credentials) | Explicit kubeconfig path. |
| `OF_SANDBOX_IMAGE` | `ghcr.io/openfactory-org/sandbox:latest` | Sandbox image when the run sets none. See [Sandbox image](#sandbox-image). |
| `OF_RUNNER_IMAGE` | the server image of the release | See [Runner images](#runner-images). |

### Cluster credentials

The backend uses the first Kubernetes API credentials it finds:

1. The kubeconfig in `OF_KUBECONFIG`.
2. The in-cluster service account, when the process runs in a Pod.
3. The first file in `$KUBECONFIG`, else `~/.kube/config`, with its current context.

A kubeconfig user must use a static token, a token file or a client certificate. A user with an exec or auth-provider plugin fails with `uses an exec/auth-provider plugin, which is not supported`. The backend reads token files on every request, so rotated projected service account tokens work.

### RBAC

The server (or worker) service account needs these rules in the sandbox namespace. With `sandbox.rbac.create: true` (default), the chart creates the Role `<fullname>-sandbox` and a RoleBinding for the server and for the worker.

| API group | Resource | Verbs |
|---|---|---|
| `""` | `pods` | `create`, `get`, `list`, `watch`, `delete`, `patch` |
| `""` | `pods/log` | `get` |
| `""` | `events` | `get`, `list`, `watch` |
| `batch` | `jobs` (one for each sandbox) | `create`, `get`, `list`, `watch`, `delete`, `patch` |
| `networking.k8s.io` | `networkpolicies` (one for each restricted sandbox) | `create`, `get`, `delete`, `patch` |

The Role has no `pods/exec`. Sandbox Pods use a different service account (`serviceAccountName` in the pod template); give it only what sandboxes need.

## How the backend runs a sandbox

1. The backend builds a Job from the pod template (see [Merge rules](#merge-rules)), with `backoffLimit: 0`, one Pod, and an active deadline of the session limit plus the stop grace time.
2. An init container copies the runner binary from `OF_RUNNER_IMAGE` into the Pod.
3. The runner (`openfactory runner`, the main process) connects to the control plane, prepares `/workspace` and runs the harness.
4. When the session ends, the control plane deletes the Job.

### Startup timeouts

The sandbox end reason for each startup failure:

| Condition | Result |
|---|---|
| An image pull fails (`ErrImagePull`, `ImagePullBackOff`, `InvalidImageName`, `ErrImageNeverPull`) for more than 5 minutes. | `image_pull` |
| The Pod stays `Unschedulable` for more than 10 minutes. | `unschedulable` |
| A container waits with `CreateContainerConfigError`. | `invalid_spec`, at once |
| The init container `egress-wait` of a restricted sandbox ends with exit code 75: the NetworkPolicy was not active within 30 seconds. | The sandbox and the run end with `network_policy_timeout`. The sandbox command never ran. See [The sandbox does not start before the policy is active](network-policy.md#the-sandbox-does-not-start-before-the-policy-is-active). |
| A container ends with a non-zero exit code. | `process_failed` |
| The session does not start within `OF_SANDBOX_START_TIMEOUT` (default 30 minutes). | The session ends with `sandbox_create_failed`. |

### Labels and annotations

| Key | Kind | Value |
|---|---|---|
| `openfactory.dev/sandbox` | label | Sandbox label |
| `openfactory.dev/sandbox-id` | annotation | Sandbox id |
| `openfactory.run` | label | Run id |
| `app.kubernetes.io/managed-by` | label | `openfactory` |

`OF_K8S_LABELS` and `OF_K8S_ANNOTATIONS` add more. List sandbox Pods:

```bash
kubectl -n openfactory-sandboxes get pods -l app.kubernetes.io/managed-by=openfactory
```

## Pod template

The pod template is a raw PodSpec in YAML or JSON (a wrapper with a `spec` key also works). Use it for scheduling, the sandbox service account, image pull secrets, tolerations, resources and environment variables from Kubernetes Secrets. Start from [`deploy/kubernetes/pod-template.yaml`](../../deploy/kubernetes/pod-template.yaml). With Helm, set `sandbox.podTemplate`.

```yaml
nodeSelector:
  kubernetes.io/os: linux
serviceAccountName: factory-sandbox
imagePullSecrets:
  - name: internal-registry
tolerations:
  - key: dedicated
    operator: Equal
    value: factories
    effect: NoSchedule
containers:
  - name: sandbox
    resources:
      requests:
        cpu: "2"
        memory: 4Gi
      limits:
        memory: 8Gi
    env:
      - name: GITHUB_TOKEN
        valueFrom:
          secretKeyRef:
            name: factory-sandbox-secrets
            key: github-token
```

### Merge rules

| Field | Rule |
|---|---|
| Sandbox container | The container named `sandbox`, added if missing. Other containers stay unchanged. |
| Image | The image of the run replaces the template image. See [Sandbox image](#sandbox-image). |
| `imagePullPolicy` | Kept from the template, else `OF_K8S_IMAGE_PULL_POLICY` (default `IfNotPresent`). |
| `command`, `args` | `command` becomes `openfactory runner`. `args` is removed. |
| `workingDir` | `/workspace`. |
| Resources | A machine `instanceShape` sets CPU and memory `requests` and `limits` to the shape value. Other template resources stay. |
| Workspace | The `emptyDir` volume `of-workspace`, mounted at `/workspace`. Template volumes and mounts stay. |
| `restartPolicy` | `Never`. |
| Metadata | Set by the backend. The template `metadata` is ignored; use `OF_K8S_LABELS` and `OF_K8S_ANNOTATIONS`. |

### Sandbox image

The image of a sandbox is the first one that is set:

1. The image of the environment of the run.
2. The `image` of the machine of the agent.
3. `OF_SANDBOX_IMAGE` (Helm: `sandbox.image`).
4. The built-in default, `ghcr.io/openfactory-org/sandbox:latest` for a build from source. A release binary uses the sandbox image of the same release.

The kubelet pulls the image, and there is no fallback image: a failed pull fails the sandbox. To use a mirror, see [Private registries](private-registries.md#mirror-the-images).

A sandbox image must meet these requirements:

- The runner binary comes from `OF_RUNNER_IMAGE`, or the image has `openfactory` on its `PATH`. The default sandbox image has no `openfactory` binary.
- It is based on glibc (not Alpine or musl).
- It provides `sh`, `git` and coreutils.
- Its architecture matches the nodes that run sandbox Pods.
- If a harness has no [tool image](#tool-images), the image contains its CLI (`claude` or `codex`).

### Registry credentials

The `kubernetes` backend ignores the machine's `registryCredentialSecretName`. Use `imagePullSecrets` in the pod template. See [Private registries](private-registries.md#use-an-image-pull-secret).

## Network policy

A run with a restricted network policy gets a NetworkPolicy and a proxy sidecar in its sandbox Pod. The NetworkPolicy allows DNS, the control plane and, for the sidecar, ports 80 and 443. The sidecar enforces the host list. The chart sets the sidecar image (`sandbox.k8s.egressProxy.sidecarImage`) to the runner image. **Without the sidecar, a restricted sandbox can reach DNS and the control plane only.** Details, the CNI requirement and the optional egress lock are in [Network policy](network-policy.md#kubernetes).

The chart sets the control plane rule. If `sandbox.k8s.controlPlane.selector` is empty, `server.enabled` is `true` and `server.internalURL` is empty, the chart sets:

| Variable | Value |
|---|---|
| `OF_K8S_CONTROL_PLANE_SELECTOR` | `app.kubernetes.io/component=server,app.kubernetes.io/instance=<release>,app.kubernetes.io/name=openfactory` (`nameOverride` replaces `openfactory`) |
| `OF_K8S_CONTROL_PLANE_NAMESPACE` | `sandbox.k8s.controlPlane.namespace`, or the release namespace if that is empty |

The rule allows all ports of the server Pods. A sandbox dials the Service on port 80, and the NetworkPolicy matches the server Pod on port 8080 after the Service forwards the connection.

To select other Pods, set `sandbox.k8s.controlPlane.selector` (and `namespace` if the Pods are in another namespace). With a custom `server.internalURL` or a worker-only install, the chart sets nothing: set the selector yourself, or use an IP address as the URL host.

## Sandbox Jobs

### Settings of sandbox Jobs

Each value under `sandbox.k8s` sets one variable for the server and the worker. Empty keeps the default. Validation rules are in [Configuration](configuration.md#sandbox-backend).

| Helm value | Variable | Effect |
|---|---|---|
| `labels`, `annotations` | `OF_K8S_LABELS`, `OF_K8S_ANNOTATIONS` | Extra Job and Pod metadata. Helm takes a map; the variable takes `key=value,key=value`. |
| `imagePullPolicy` | `OF_K8S_IMAGE_PULL_POLICY` | `Always`, `IfNotPresent` (default) or `Never`. Applies to the init containers, and to the sandbox container when the template sets none. |
| `runAsUser` | `OF_K8S_RUN_AS_USER` | See [Sandbox user](#sandbox-user). |
| `workspaceSizeLimit` | `OF_K8S_WORKSPACE_SIZE_LIMIT` | `sizeLimit` of the workspace, for example `20Gi`. |
| `preflightJob` | `OF_K8S_PREFLIGHT_JOB` | See [Preflight](#preflight). |
| `controlPlane.namespace`, `controlPlane.selector` | `OF_K8S_CONTROL_PLANE_NAMESPACE`, `OF_K8S_CONTROL_PLANE_SELECTOR` | Server Pods that a restricted sandbox can reach. Chart default: the server Pods of the release. See [Network policy](#network-policy). |
| `egressProxy.sidecarImage` | `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` | Proxy sidecar image (chart default: the runner image). Needs Kubernetes 1.29 or later. See [Proxy sidecar](network-policy.md#proxy-sidecar-for-each-sandbox). |
| `egressProxy.lock` | `OF_K8S_EGRESS_LOCK` | `none` (default) or `iptables`. See [Egress lock](network-policy.md#egress-lock-optional). |
| `networkPolicy.check` | `OF_K8S_NETWORK_POLICY_CHECK` | `probe` (default) or `off`. See [Check that the cluster enforces NetworkPolicy](network-policy.md#check-that-the-cluster-enforces-networkpolicy). |
| `networkPolicy.enforcement` | `OF_K8S_NETWORK_POLICY_ENFORCEMENT` | `warn` (default) or `require`. |
| `networkPolicy.waitTimeout`, `networkPolicy.waitAddr` | `OF_K8S_EGRESS_WAIT_TIMEOUT`, `OF_K8S_EGRESS_WAIT_ADDR` | Wait until the NetworkPolicy is active. See [The sandbox does not start before the policy is active](network-policy.md#the-sandbox-does-not-start-before-the-policy-is-active). |
| `allowUnenforcedNetworkPolicy` | `OF_ALLOW_UNENFORCED_NETWORK_POLICY` | Start a restricted sandbox without an enforced NetworkPolicy. Only if another control enforces egress. See [Network policy](network-policy.md#kubernetes). |

### Sandbox user

By default the Pod runs with `runAsNonRoot: true` and user, group and `fsGroup` 1000, which the restricted Pod Security profile accepts. Images that start as root usually work too, because the backend overrides the user. The image must let that user run its tools. The workspace, the state directory and `/home/agent` are writable volumes.

The user is the first value that is set: `platform.linux.user` of the machine, then `OF_K8S_RUN_AS_USER`, then `1000`. Each accepts a number of 1 or more, or `image`.

- With a number, the backend adds `runAsNonRoot`, `runAsUser`, `runAsGroup` and `fsGroup`. Fields that the pod template sets in its `securityContext` stay.
- With `image`, the backend adds none of them. The Pod uses the user of the pod template and the image. Kubernetes refuses a root image only if your template sets `runAsNonRoot`.

The init containers always run as a user that is not root. If the Pod user is not the image user, `HOME` is `/home/agent`.

### Tool images

A tool image holds a harness CLI (`claude` or `codex`), so the sandbox image does not need it: `ubuntu:22.04` works with the `claude` harness. The control plane adds the tool image of the harness to the `tool_images` list of each sandbox.

Set the images with `OF_CLAUDE_TOOL_IMAGE` and `OF_CODEX_TOOL_IMAGE` (Helm: `runner.claudeToolImage`, `runner.codexToolImage`). Without them, the control plane derives the names from `OF_RUNNER_IMAGE`, but only if its repository is named `openfactory` and it has a tag: `registry.example.com/of/openfactory:1.2.3` gives `registry.example.com/of/tool-claude:1.2.3` and `registry.example.com/of/tool-codex:1.2.3`. If the runner image has a digest or another name, set both settings.

For each tool image, an init container runs `/bin/sh -c "cp -RP /opt/openfactory/tool/. /dest/"` as a non-root user. `/dest` is a shared `emptyDir`, mounted in the sandbox container at `/opt/openfactory/tools/<name>`. (`cp -a` would fail: it sets the owner, mode and times of the root-owned target.)

The sandbox container puts `/opt/openfactory/tools/<name>/bin` of each tool in front of the image `PATH`, then starts the runner. So that `bin` directory must also hold what the CLI needs, for example `node`.

The [preflight Job](#preflight) copies each tool image that you set, not derived names, so a missing configured image stops the server at startup.

A tool image must meet these rules:

- It has `/bin/sh` and `cp`.
- All files of the tool are under `/opt/openfactory/tool/`, readable by the Pod user. Executables keep their mode bits.
- The tool name has at most 40 characters: lower case letters, digits and dashes.

The backend refuses a sandbox with an invalid tool name or two tool images of the same name. The run ends with `invalid_configuration`.

### Runner images

The runner image holds the static `openfactory` binary at `/usr/local/bin/openfactory`. The server image is the runner image, so a release needs no separate runner image.

`OF_RUNNER_IMAGE` defaults to the server image of the same release, for example `ghcr.io/openfactory-org/openfactory:1.2.3` (`latest` for a build from source). In the chart, `runner.image` defaults to `image.repository` and `image.tag`. Set it only if you mirror the images or use your own image.

An init container copies the binary into each sandbox (`cp /usr/local/bin/openfactory /opt/openfactory/openfactory`), as a user that is not root (the Pod user, else 1000). The binary is static, so it runs in any Linux sandbox image.

| Dockerfile | Image | Content |
|---|---|---|
| `deploy/docker/Dockerfile` | `ghcr.io/openfactory-org/openfactory` | Server, worker and runner (Alpine base). |
| `deploy/sandbox/Dockerfile` | `ghcr.io/openfactory-org/sandbox` | The default sandbox image. No harness CLI. |
| `images/tool-claude/Dockerfile` | `ghcr.io/openfactory-org/tool-claude` | Node.js and the `claude` CLI under `/opt/openfactory/tool`. |
| `images/tool-codex/Dockerfile` | `ghcr.io/openfactory-org/tool-codex` | Node.js and the `codex` CLI under `/opt/openfactory/tool`. |
| `images/tool-example/Dockerfile` | none | An example of the tool image contract. |

The tool images use a Debian base with a glibc build of Node.js, so the sandbox image must be glibc-based. Build them from the repository root with `make image-tool-claude` and `make image-tool-codex`. The build arguments `CLAUDE_CLI_PACKAGE` and `CLAUDE_CLI_VERSION` (and the codex equivalents) select the npm package and version (default `latest`). Pin the version for a repeatable build:

```bash
make image-tool-claude CLAUDE_CLI_VERSION=1.2.3 TOOL_CLAUDE_IMAGE=registry.example.com/openfactory/tool-claude IMAGE_TAG=1.2.3
```

To change the built-in default sandbox image, set `DEFAULT_SANDBOX_IMAGE` for `make build`. The build arguments `DEFAULT_SANDBOX_IMAGE` and `DEFAULT_RUNNER_IMAGE` of `deploy/docker/Dockerfile` do the same for the server image. `OF_SANDBOX_IMAGE` and `OF_RUNNER_IMAGE` override them at run time.

### Preflight

At startup, the server checks the cluster:

1. Access reviews for `jobs` (create, get, list, watch, delete), `pods` (get, list, watch), `pods/log` (get) and `events` (list). A missing rule stops the start; the error names it, for example `jobs.batch (create, delete, get, list, watch)`.
2. A list of Jobs and Pods in the sandbox namespace. A missing permission stops the start.
3. An access review for NetworkPolicy (create, get, patch, delete). A missing rule logs an error but does not stop the start, because unrestricted sandboxes do not need it.
4. The NetworkPolicy probe, in the background. See [Check that the cluster enforces NetworkPolicy](network-policy.md#check-that-the-cluster-enforces-networkpolicy).

With `OF_K8S_PREFLIGHT_JOB=true`, the server also runs a test Job from the pod template, with the runner image (else the default sandbox image). The Job runs `/bin/sh -c "exit 0"` (active deadline 120 seconds), so the image needs `/bin/sh`. The server polls every 2 seconds for up to 3 minutes, then deletes the Job. If the Job fails, the server does not start:

| Reason | Meaning |
|---|---|
| `image pull` | The cluster cannot pull the image. Check the name, the registry and the image pull secrets. |
| `unschedulable` | No node can run the Pod. Check the resources, the taints and the node selectors. |
| the cluster refused the Pod | An admission policy blocked the Pod. |
| `timeout` | The Job did not end in 3 minutes. |

### Protect sandbox Pods from disruption

A node drain or autoscaler can stop a sandbox Pod mid-run. Add a do-not-disrupt annotation with `OF_K8S_ANNOTATIONS`:

```bash
OF_K8S_ANNOTATIONS='cluster-autoscaler.kubernetes.io/safe-to-evict=false'
```

With Helm:

```yaml
sandbox:
  k8s:
    annotations:
      cluster-autoscaler.kubernetes.io/safe-to-evict: "false"
```

Other autoscalers document their own keys. An annotation does not stop a node failure or a node-pressure eviction. The backend maps these endings to a reason:

| Event | Reason |
|---|---|
| The container is killed for memory. | `container_oom` |
| Kubernetes evicts the Pod. | `evicted` |
| The Job passes its deadline. | `deadline_exceeded` |
| The node is lost or shuts down. | `node_lost` |

A PodDisruptionBudget protects sandbox Pods only against evictions through the eviction API.

### Setup commands and sudo

The hardened pod template sets `allowPrivilegeEscalation: false`, so `sudo` in a setup command fails. Install tools in the sandbox image instead. To permit `sudo`, set `allowPrivilegeEscalation: true` on the `sandbox` container of a custom pod template and add the sandbox user to `sudoers` in the image. The restricted Pod Security profile refuses this.

## Containers in the sandbox

The sandbox container cannot build or run containers on its own: it is not root, it has no Docker daemon, and the hardened pod template blocks privilege escalation. Open Factory does not start a container engine.

The safe way is a rootless Docker daemon in a second container of the Pod. The agent talks to it over `localhost`. The daemon's user and capabilities do not reach the sandbox container, and the daemon and its images go away with the Pod. Add it to the pod template:

```yaml
containers:
  - name: sandbox
    env:
      # The Docker CLI in the sandbox container uses the daemon of the sidecar.
      - name: DOCKER_HOST
        value: tcp://127.0.0.1:2375
  - name: docker
    image: docker:27-dind-rootless
    env:
      # No TLS: the daemon listens on localhost of the Pod only.
      - name: DOCKER_TLS_CERTDIR
        value: ""
    args: ["--host=tcp://127.0.0.1:2375"]
    securityContext:
      # Rootless Docker needs more than the restricted profile allows.
      runAsUser: 1000
      runAsGroup: 1000
      seccompProfile:
        type: Unconfined
      appArmorProfile:
        type: Unconfined
    resources:
      requests: {cpu: "500m", memory: 1Gi}
      limits: {memory: 8Gi}
    volumeMounts:
      - name: docker-data
        mountPath: /home/rootless/.local/share/docker
volumes:
  - name: docker-data
    emptyDir: {}
```

- The backend keeps template containers that are not named `sandbox`. The sandbox image needs only the Docker CLI.
- `Unconfined` profiles are refused by the `restricted` and `baseline` Pod Security profiles. Use a sandbox namespace with `privileged`, or a runtime that fits your policy.
- **Do not use a privileged Docker daemon that shares the node.** An agent that reads text from issues could then reach the node.
- The sidecar shares the Pod network and NetworkPolicy. With a restricted policy, allow the registry hosts in `allowedHosts` and give the daemon the proxy variables.
- Set a memory limit and an `emptyDir` size limit that fit your builds.

Alternatives: a remote build service over HTTPS, or a daemonless build tool such as `buildah` or `kaniko` in the sandbox image.

## Operational notes

- The workspace is an `emptyDir`. If Kubernetes evicts or deletes the Pod, the workspace is lost and the run fails.
- If Kubernetes deletes a Pod while a run is idle, a later follow-up starts a new sandbox.
- The scheduler places Pods by their requests. An instance shape sets requests equal to limits, so a large shape needs a node with that much free capacity.

## Related pages

- [Self-hosting overview](README.md)
- [Configuration](configuration.md)
- [Private registries](private-registries.md)
- [Troubleshooting](../troubleshooting.md#kubernetes-sandbox-pods)
