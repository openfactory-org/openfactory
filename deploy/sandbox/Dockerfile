# syntax=docker/dockerfile:1.7
#
# Default sandbox image: ghcr.io/openfactory-org/sandbox
#
#   docker build -f deploy/sandbox/Dockerfile -t ghcr.io/openfactory-org/sandbox:latest deploy/sandbox
#
# Agents run inside this image on the docker and kubernetes backends. It is
# glibc-based and ships sh, git and coreutils (required by the backends) and
# the common build tools. It has no harness CLI: the kubernetes backend copies
# the claude or codex CLI from a tool image (tool-claude, tool-codex) into the
# sandbox. A sandbox image that runs without a tool image must hold its CLI.
# Override it per machine (platform.linux.image), per environment, or
# with OF_SANDBOX_IMAGE.

ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG NODE_MAJOR=22
ARG GO_VERSION=1.27.4
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl wget gnupg git git-lfs openssh-client \
      build-essential pkg-config make unzip zip xz-utils less procps \
      python3 python3-pip python3-venv pipx \
      ripgrep jq fd-find tree sudo \
 && rm -rf /var/lib/apt/lists/*

# Node.js (NodeSource) and npm.
RUN mkdir -p /etc/apt/keyrings \
 && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
      | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
 && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
      > /etc/apt/sources.list.d/nodesource.list \
 && apt-get update && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# GitHub CLI.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# Go toolchain.
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH

# fd is packaged as fdfind on Ubuntu.
RUN ln -s "$(command -v fdfind)" /usr/local/bin/fd

# A non-root user is available; the backends run commands as the image user.
RUN useradd -m -u 1001 -s /bin/bash agent \
 && echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent \
 && mkdir -p /workspace && chown agent:agent /workspace

LABEL org.opencontainers.image.title="openfactory-sandbox" \
      org.opencontainers.image.description="Default sandbox image for Open Factory agent runs" \
      org.opencontainers.image.source="https://github.com/openfactory-org/openfactory" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /workspace
CMD ["sleep", "infinity"]
