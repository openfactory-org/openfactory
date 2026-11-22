# Private registries

Private images matter for the sandbox image and for custom images that machines name in `platform.linux.image`. How a provider pulls an image differs.

## By provider

| Provider | How it gets the image |
|---|---|
| `cloudflare-sandbox` | The image is built into the provider Worker and pushed to the Cloudflare registry when you deploy. A machine image does not apply. |
| `daytona` | A Daytona snapshot built from the sandbox image. Build the snapshot from your own registry image, with the registry credentials that Daytona holds for it. |
| `boatdev` | A saved boat.dev snapshot. Build it with `make boat-snapshot`. |
| microVM worker | The host pulls the image itself and converts it to a disk image. See [Registry credentials on a worker](#registry-credentials-on-a-worker). |

For the hosted providers, the image is prepared when you build the snapshot or deploy the Worker, so no pull secret reaches a run.

## Registry credentials on a worker

A microVM worker pulls the image of each session on the host. For a private image, name a team secret of the type `docker_registry` in the machine:

```yaml
# machines/linux-build.yaml
platform:
  os: linux
  linux:
    image: registry.example.com/factory/build:2026.11
    registryCredentialSecretName: REGISTRY_PULL_CREDENTIAL
```

The control plane gives the worker a short-lived pull credential for that secret when it assigns the session. The credential stays in memory on the host.

## Mirror the sandbox image

When a host or provider cannot pull from `ghcr.io`, copy the sandbox image to your registry with any OCI tool, for example `crane`:

```bash
crane copy ghcr.io/openfactory-org/sandbox:1.2.3 registry.example.com/mirror/sandbox:1.2.3
```

Then build the Daytona snapshot or the boat.dev snapshot from the mirror, or set the machine `platform.linux.image` for microVM workers. The sandbox image contains the runtime bootstrap, so a custom image must follow the [image contract](../sandbox-providers.md#build-the-sandbox-image).

There is no fallback image. A pull that fails ends the session with `sandbox_create_failed`. See [Image pull failures](../troubleshooting.md#image-pull-failures).
