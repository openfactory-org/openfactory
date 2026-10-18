# Private registries

The kubelet pulls sandbox images, so registry credentials are Kubernetes image pull secrets in the sandbox namespace. The backend ignores the machine field `registryCredentialSecretName`; definitions still accept it.

## Use an image pull secret

1. Create the secret in the sandbox namespace.

   ```bash
   kubectl -n openfactory-sandboxes create secret docker-registry internal-registry \
     --docker-server=registry.example.com \
     --docker-username=factory-bot \
     --docker-password="$REGISTRY_TOKEN"
   ```

2. Reference it in the pod template (`sandbox.podTemplate` in the Helm chart).

   ```yaml
   imagePullSecrets:
     - name: internal-registry
   ```

3. Set the image on the machine (or the environment).

   ```yaml
   # machines/linux-build.yaml
   platform:
     os: linux
     linux:
       image: registry.example.com/factory/build:2026.11
   ```

Instead of step 2, you can attach the pull secret to the service account of the sandbox pods (`serviceAccountName` in the [pod template](kubernetes.md#pod-template)).

## Amazon ECR, Google Artifact Registry and Azure Container Registry

Prefer node or workload identity over static credentials:

- **ECR.** Give the node role (or the kubelet credential provider) pull permission. ECR tokens in a static pull secret expire after 12 hours, so a static secret needs a controller that refreshes it.
- **Artifact Registry.** Give the node service account `roles/artifactregistry.reader`.
- **ACR.** Attach the registry to the cluster (`az aks update --attach-acr`), or grant the kubelet identity `AcrPull`.

## Mirror the images

When nodes cannot pull from `ghcr.io`, mirror the four images of a Kubernetes install:

| Image | Use |
| --- | --- |
| `ghcr.io/openfactory-org/openfactory` | Server and worker. Also the runner image: the backend copies its `openfactory` binary into each sandbox. |
| `ghcr.io/openfactory-org/sandbox` | The default sandbox image. It has no harness CLI. |
| `ghcr.io/openfactory-org/tool-claude` | Tool image with the `claude` CLI. |
| `ghcr.io/openfactory-org/tool-codex` | Tool image with the `codex` CLI. |

Use the same tag for all four images.

1. Copy the images with any OCI tool, for example `crane` or `skopeo`.

   ```bash
   for name in openfactory sandbox tool-claude tool-codex; do
     crane copy "ghcr.io/openfactory-org/$name:1.2.3" "registry.example.com/mirror/$name:1.2.3"
   done
   ```

2. Set the images. With Helm, set `image.repository` and `sandbox.image` (and the `sandbox` image in `sandbox.podTemplate`, if you set one). `runner.image` defaults to `image.repository` and `image.tag`. Without Helm, set `OF_RUNNER_IMAGE` too, because its default is the release image on `ghcr.io`:

   ```bash
   export OF_SANDBOX_IMAGE=registry.example.com/mirror/sandbox:1.2.3
   export OF_RUNNER_IMAGE=registry.example.com/mirror/openfactory:1.2.3
   ```

3. If the repositories `openfactory`, `tool-claude` and `tool-codex` share one registry path, the server derives the tool image names from the runner image. Otherwise set `OF_CLAUDE_TOOL_IMAGE` and `OF_CODEX_TOOL_IMAGE`.

A sandbox without a tool image must have its CLI on the `PATH` of its image, for example `claude` for the `claude` harness.

There is no fallback image. A pull that fails for more than 5 minutes ends the sandbox with the reason `image_pull`. See [Startup timeouts](kubernetes.md#startup-timeouts), [Sandbox image](kubernetes.md#sandbox-image) and [Image pull failures](../troubleshooting.md#image-pull-failures).
