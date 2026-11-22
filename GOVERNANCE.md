# Governance

The founding maintainers of the `openfactory-org` GitHub organization maintain this project. They review, merge, triage and release.

## Decisions

Decisions happen in issues and pull requests, in public. A maintainer who is not the author reviews each pull request before it is merged.

## Breaking changes

These are the public contracts:

- The definition schema `v1`.
- The REST API under `/api/v1`.
- The runner protocol and the worker protocol (version 2).
- The plugin API (`@openfactory/plugin-api`).
- The Durable Object migrations between releases.

Open an issue to discuss a breaking change to a contract before you send a pull request. Record the change and its migration steps in [CHANGELOG.md](CHANGELOG.md).

Before `v1.0.0`, a breaking change can go into a minor release (`v0.x`). From `v1.0.0`, a breaking change needs a new major version.

## Future changes

The maintainers will formalize the governance model as the contributor community grows.
