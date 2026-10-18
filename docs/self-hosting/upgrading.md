# Upgrading

## Versions

No release is tagged yet. The first releases are `v0.x`, with no promise of stable contracts:

| Contract | In `v0.x` |
|---|---|
| Definition schema `v1` | The name of the schema, not the product version. It can still change. |
| REST API `/api/v1` | Endpoints and fields can be renamed or removed. |
| Database | Each release migrates databases from every earlier release; CI tests this. A database made by a development build before the first release does not open. Create it again. |
| Go packages under `pkg/` | Experimental. They can change in any release. |

The release notes list each change. Read them before each upgrade, and pin the version that you run.

`v1.0.0` is the first release with stable contracts. Nobody creates the tag until all of these are true:

1. The run model, runner protocol, REST API and definition schema are final, and the code agrees with them.
2. All REST API paths follow one style, with no old aliases.
3. `make openapi-baseline` has written the baseline from the final API document, and `make web-check-api` passes.
4. The upgrade tests pass from every earlier `v0.x` release, on SQLite and PostgreSQL.
5. At least one `v0.x` release has run for at least 4 weeks in at least 3 installs that the maintainers do not run. Each install has upgraded at least once. No open bug about data loss or a broken upgrade remains.
6. The security work planned for the first public release is complete.
7. The docs match the code of the release candidate.
8. A release candidate `v1.0.0-rc.N` stayed unchanged for at least 7 days. A fix makes a new candidate.

## Upgrade

1. Read the release notes.
2. Back up the database (the SQLite volume, or a PostgreSQL dump). Keep `OF_SECRET_KEY`.
3. List the runs in progress:

   ```bash
   openfactory run list --state queued,pending,claimed,running
   ```

4. Upgrade the chart:

   ```bash
   helm upgrade openfactory deploy/helm/openfactory -n openfactory -f values.yaml
   ```

5. Check that each active work item continues. If a foreman stays idle, send it a follow-up message.

The server applies migrations at start (see [Migrations](configuration.md#migrations)). In PostgreSQL mode, an advisory lock lets one replica migrate while the others wait, and the rolling update replaces one pod at a time. In SQLite mode, the single pod restarts, so the control plane is down for a short time.

### Runs during an upgrade

- A server replica holds no agent turn. A pod restart does not interrupt or repeat a turn: the runner sends events with a cursor and reconnects to any replica.
- Queued runs start after the restart.
- Sandboxes keep running in their Jobs and on remote workers. A worker restart does not stop its sandboxes.

## How a release is cut

For maintainers. A tag that starts with `v` runs `.github/workflows/release.yml`.

1. Check that CI is green on `main`. Before `v1.0.0`, the OpenAPI diff in CI only reports, so read its summary.
2. Choose the version. In `v0.x`, raise the minor number (`v0.2.0`) for a breaking change, and the patch number (`v0.1.1`) for fixes only.
3. Run `make openapi-baseline` and commit the result, so later changes are compared with this release.
4. Tag the commit and push the tag. Use `v0.1.0-rc.1` for a candidate; GoReleaser marks a tag with a suffix as a pre-release.

   ```bash
   git tag -a v0.1.0 -m "Open Factory v0.1.0"
   git push origin v0.1.0
   ```

5. The workflow runs all tests, then publishes:
   - the binary archives, checksums and the GitHub release, with a change list from the commit messages;
   - the server image `ghcr.io/openfactory-org/openfactory:<version>`, the sandbox image and the tool images `tool-claude` and `tool-codex`;
   - the Helm chart, with the tag version, to `oci://ghcr.io/openfactory-org/charts`.
6. Edit the release notes. Add a "Breaking changes" section with each contract change and the step that users must take.
