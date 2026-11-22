# Upgrading

## Versions

No release is tagged yet. The first releases are `v0.x`, with no promise of stable contracts:

| Contract | In `v0.x` |
|---|---|
| Definition schema `v1` | The name of the schema, not the product version. It can still change. |
| REST API `/api/v1` | Endpoints and fields can be renamed or removed. |
| Durable Object schema | Each release migrates the data of every earlier release; CI tests this. |
| Worker protocol | Worker protocol v2. A worker needs the minimum runtime version of the control plane. |

The release notes list each change. Read them before each upgrade, and pin the version that you run.

`v1.0.0` is the first release with stable contracts. Nobody creates the tag until all of these are true:

1. The run model, runner protocol, REST API and definition schema are final, and the code agrees with them.
2. All REST API paths follow one style, with no old aliases.
3. The OpenAPI baseline `api/openapi.v1.baseline.json` comes from the final API document, and the contract suite passes.
4. The Durable Object migration tests pass from every earlier `v0.x` release.
5. At least one `v0.x` release has run for at least 4 weeks in at least 3 installs that the maintainers do not run. Each install has upgraded at least once. No open bug about data loss or a broken upgrade remains.
6. The security work planned for the first public release is complete.
7. The docs match the code of the release candidate.
8. A release candidate `v1.0.0-rc.N` stayed unchanged for at least 7 days. A fix makes a new candidate.

## Upgrade

1. Read the release notes.
2. Keep `OF_SECRET_KEY` and `OF_SIGNING_KEY`. List the runs in progress:

   ```bash
   openfactory run list
   ```

3. Check out the release and build:

   ```bash
   git checkout v0.2.0
   npm ci
   npm run build
   ```

4. Deploy the provider and plugin Workers that changed, then the control plane:

   ```bash
   npx wrangler deploy -c providers/cloudflare-sandbox/wrangler.jsonc
   npx wrangler deploy -c apps/control-plane/wrangler.jsonc
   ```

5. Set `OF_RUNNER_VERSION` to the version of the `openfactory-runtime` release, and publish that runtime. Session start refuses a runner with another version.
6. Update the runtime on each microVM worker and restart its service. A worker with a protocol below the minimum shows "update available".
7. Check that each active work item continues. If a foreman stays idle, send it a follow-up message.

### Durable Object migrations

Each Durable Object applies its schema migrations when it starts, in order, and records the version. A migration never changes a released migration file: a change is a new file. The `exports` map of `wrangler.jsonc` declares each class. To remove a class, add a `state: "deleted"` entry, because the move is one way.

There is no way to roll a migration back. Before an upgrade that adds migrations, read the release notes. To return to an earlier release, deploy it again: a release can read the data of the release before it.

### Runs during an upgrade

- The control plane holds no agent turn. A deploy does not interrupt or repeat a turn: the runner sends events with a cursor and reconnects.
- A Durable Object keeps running its current code for up to 300 seconds after a deploy, so that a turn can finish (`code_update_strategy`). Held connections reconnect after a reset.
- Queued runs start after the deploy.
- Sandboxes keep running. A deploy of the control plane does not restart `cloudflare-sandbox` sandboxes. A worker restart does not stop its sessions.

## How a release is cut

For maintainers. A tag that starts with `v` runs `.github/workflows/release.yml`.

1. Check that CI is green on `main`. Before `v1.0.0`, the OpenAPI diff in CI only reports, so read its summary.
2. Choose the version. In `v0.x`, raise the minor number (`v0.2.0`) for a breaking change, and the patch number (`v0.1.1`) for fixes only.
3. Write the baseline from the final API document to `api/openapi.v1.baseline.json` and commit it, so later changes are compared with this release.
4. Tag the commit and push the tag. Use `v0.1.0-rc.1` for a candidate; GoReleaser marks a tag with a suffix as a pre-release.

   ```bash
   git tag -a v0.1.0 -m "Open Factory v0.1.0"
   git push origin v0.1.0
   ```

5. The workflow runs all tests, then publishes:
   - the `openfactory-runtime` binaries and checksums, the guest kernel for microVM workers, and the GitHub release, with a change list from the commit messages;
   - the sandbox image `ghcr.io/openfactory-org/sandbox:<version>`;
   - the npm packages `openfactory` (the CLI) and `@openfactory/control-plane`.
6. Edit the release notes. Add a "Breaking changes" section with each contract change and the step that users must take.
