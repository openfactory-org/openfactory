# syntax=docker/dockerfile:1.7
#
# Open Factory tool image with the `claude` harness CLI.
#
#   docker build -f images/tool-claude/Dockerfile -t ghcr.io/openfactory-org/tool-claude:dev images/tool-claude
#
# The image meets the tool image contract (see docs/self-hosting/kubernetes.md).
# Node.js and the CLI live under /opt/openfactory/tool. The kubernetes backend
# copies that directory into each task of the claude harness, and puts
# /opt/openfactory/tools/claude/bin in front of the PATH. The task image must be
# glibc-based, because node is a glibc build.
# The package name and the version are build arguments, so that a team can use
# a mirror or pin a release.

ARG NODE_VERSION=22
ARG DEBIAN_VERSION=bookworm

FROM node:${NODE_VERSION}-${DEBIAN_VERSION}-slim AS build
ARG CLAUDE_CLI_PACKAGE=@anthropic-ai/claude-code
ARG CLAUDE_CLI_VERSION=latest
RUN mkdir -p /opt/openfactory/tool/bin /opt/openfactory/tool/lib \
 && cp /usr/local/bin/node /opt/openfactory/tool/bin/node \
 && cp -R /usr/local/lib/node_modules /opt/openfactory/tool/lib/node_modules \
 && ln -s ../lib/node_modules/npm/bin/npm-cli.js /opt/openfactory/tool/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /opt/openfactory/tool/bin/npx \
 && export PATH=/opt/openfactory/tool/bin:$PATH \
 && npm install --global --prefix /opt/openfactory/tool "${CLAUDE_CLI_PACKAGE}@${CLAUDE_CLI_VERSION}" \
 && npm cache clean --force \
 && chmod -R a+rX /opt/openfactory/tool \
 && test -x /opt/openfactory/tool/bin/claude

# The contract needs /bin/sh and cp. A slim glibc image also lets the smoke
# checks run the CLI in place.
FROM debian:${DEBIAN_VERSION}-slim
COPY --from=build /opt/openfactory/tool /opt/openfactory/tool
ENV PATH=/opt/openfactory/tool/bin:$PATH

LABEL org.opencontainers.image.title="Open Factory tool claude" \
      org.opencontainers.image.description="Open Factory tool image with Node.js and the claude harness CLI" \
      org.opencontainers.image.source="https://github.com/openfactory-org/openfactory" \
      org.opencontainers.image.licenses="Apache-2.0"

USER 1000:1000
