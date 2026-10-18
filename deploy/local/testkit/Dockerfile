# syntax=docker/dockerfile:1.7
#
# Test programs of the local stack (docs/self-hosting/local-testing.md): the
# Pod Identity agent stand-in and the claude CLI stand-in, in one binary. The
# default image also has git, so that `git daemon` can serve test repositories,
# and curl, for the Pods that the checks start (scripts/local/check.sh).
#
#   docker build -t of-local/testkit:latest -f deploy/local/testkit/Dockerfile .
#
# The target tool-claude is a tool image with the claude CLI stand-in. It has
# the layout of images/tool-claude: the CLI is /opt/openfactory/tool/bin/claude.
# The binary is static, so the sandbox image can be any Linux image.
#
#   docker build --target tool-claude -t of-local/tool-claude-standin:latest -f deploy/local/testkit/Dockerfile .
#
# With --build-arg TOOL_BASE=<the tool-claude image>, the image also keeps the
# real CLI as claude-real. A run with a model credential then uses the real
# CLI, and a run without one gets the fixed answers of the stand-in.
#
# The build context is the repository root. For tests only.

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.22
ARG TOOL_BASE=alpine:3.22

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/testkit ./deploy/local/testkit

FROM ${TOOL_BASE} AS tool-claude
USER 0
RUN mkdir -p /opt/openfactory/tool/bin \
 && if [ -e /opt/openfactory/tool/bin/claude ]; then mv /opt/openfactory/tool/bin/claude /opt/openfactory/tool/bin/claude-real; fi
COPY --from=build --chmod=0755 /out/testkit /opt/openfactory/tool/bin/claude
ENV PATH=/opt/openfactory/tool/bin:$PATH
LABEL org.opencontainers.image.title="Open Factory tool claude stand-in" \
      org.opencontainers.image.description="A stand-in for the claude CLI with fixed answers. For tests only."
USER 1000:1000

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache git git-daemon ca-certificates curl \
 && adduser -D -u 10001 -h /home/testkit testkit \
 && mkdir -p /srv/git && chown testkit /srv/git
COPY --from=build /out/testkit /usr/local/bin/testkit
USER 10001
WORKDIR /srv/git
ENTRYPOINT ["testkit"]
