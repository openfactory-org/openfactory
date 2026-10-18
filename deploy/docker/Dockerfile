# syntax=docker/dockerfile:1.7
#
# Open Factory server and worker image.
#
#   docker build -f deploy/docker/Dockerfile -t ghcr.io/openfactory-org/openfactory:dev .
#
# Stages: the React dashboard (web/) is built with Node and embedded in the
# binary; the binary and the bundled gRPC plugins (GitLab, Azure DevOps) are
# static Go builds (CGO_ENABLED=0; SQLite is pure Go). The runtime image is
# Alpine because the server calls the git CLI (definition sync, and the local
# development backend). The process runs as the non-root user "openfactory"
# (10001). Bundled plugins live in /usr/local/lib/openfactory/plugins, the
# image's OF_PLUGIN_DIR.
#
# The image is also the runner image (the default of OF_RUNNER_IMAGE). The
# kubernetes backend copies the static binary /usr/local/bin/openfactory into
# each sandbox with an init container that runs `cp` as a user that is not root.
# Keep the binary static and readable by all users.

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.22
ARG NODE_VERSION=20

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY internal/api/openapi.json /src/internal/api/openapi.json
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# DEFAULT_SANDBOX_IMAGE sets the default sandbox image of the binary.
# DEFAULT_RUNNER_IMAGE sets the default of OF_RUNNER_IMAGE: set it to the name
# and tag of this image. Leave them empty to keep the defaults of the source.
ARG DEFAULT_SANDBOX_IMAGE=
ARG DEFAULT_RUNNER_IMAGE=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} ${DEFAULT_SANDBOX_IMAGE:+-X github.com/openfactory-org/openfactory/internal/sandbox.DefaultImage=${DEFAULT_SANDBOX_IMAGE}} ${DEFAULT_RUNNER_IMAGE:+-X github.com/openfactory-org/openfactory/internal/config.DefaultRunnerImage=${DEFAULT_RUNNER_IMAGE}}" \
      -o /out/openfactory ./cmd/openfactory \
 && for p in gitlab azuredevops; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags "-s -w" -o /out/plugins/openfactory-plugin-$p ./plugins/$p; \
    done

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates tzdata git openssh-client iptables \
 && addgroup -S -g 10001 openfactory \
 && adduser -S -D -H -u 10001 -G openfactory -h /var/lib/openfactory openfactory \
 && mkdir -p /var/lib/openfactory \
 && chown openfactory:openfactory /var/lib/openfactory
COPY --from=build /out/openfactory /usr/local/bin/openfactory
COPY --from=build /out/plugins/ /usr/local/lib/openfactory/plugins/

ARG VERSION=dev
LABEL org.opencontainers.image.title="openfactory" \
      org.opencontainers.image.description="Open Factory: self-hosted AI coding agent factories" \
      org.opencontainers.image.source="https://github.com/openfactory-org/openfactory" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

ENV HOME=/var/lib/openfactory \
    OF_DATA_DIR=/var/lib/openfactory \
    OF_PLUGIN_DIR=/usr/local/lib/openfactory/plugins \
    OF_LISTEN=:8080
USER 10001:10001
WORKDIR /var/lib/openfactory
VOLUME ["/var/lib/openfactory"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/openfactory"]
CMD ["serve"]
