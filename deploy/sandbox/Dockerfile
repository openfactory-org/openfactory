# syntax=docker/dockerfile:1.7
#
# Sandbox image: ghcr.io/openfactory-org/sandbox
#
#   docker build -f deploy/sandbox/Dockerfile -t ghcr.io/openfactory-org/sandbox:dev deploy/sandbox
#
# Used for the Daytona snapshot (imageName), as the base of
# Dockerfile.cloudflare, and as a generic sandbox image. All build steps are in
# install.sh, which scripts/boat-snapshot.sh runs on boat.dev too.
#
# Contract: users agent (uid 1001) and ofproxy; no sudo and no setuid
# binaries; iproute2, iptables, ip6tables, git, claude and codex;
# /opt/openfactory/bootstrap.sh starts the runner (see bootstrap.sh).
#
# A baked runner is optional: put openfactory-runtime-linux-<arch> into
# deploy/sandbox/runner/ before the build (scripts/images/build.sh does it).
# Without it, bootstrap.sh downloads the runner from OF_RUNNER_URL.

ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG TARGETARCH
ARG CLAUDE_CLI_VERSION=latest
ARG CODEX_CLI_VERSION=latest
ARG GO_VERSION=1.27.4
ARG NODE_MAJOR=22

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

RUN --mount=type=bind,target=/src \
    set -eu; \
    runner=""; \
    if [ -f "/src/runner/openfactory-runtime-linux-${TARGETARCH}" ]; then \
      runner="/src/runner/openfactory-runtime-linux-${TARGETARCH}"; \
    fi; \
    sh /src/install.sh --profile base \
      --claude-version "${CLAUDE_CLI_VERSION}" --codex-version "${CODEX_CLI_VERSION}" \
      --go-version "${GO_VERSION}" --node-major "${NODE_MAJOR}" \
      ${runner:+--runner "$runner"}

ENV PATH=/opt/openfactory/tools/bin:/usr/local/go/bin:$PATH

LABEL org.opencontainers.image.title="openfactory-sandbox" \
      org.opencontainers.image.description="Sandbox image for Open Factory agent runs" \
      org.opencontainers.image.source="https://github.com/openfactory-org/openfactory" \
      org.opencontainers.image.licenses="Apache-2.0"

# The bootstrap runs as root; the runner starts the harness as agent.
USER root
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]
CMD ["/opt/openfactory/bootstrap.sh"]
