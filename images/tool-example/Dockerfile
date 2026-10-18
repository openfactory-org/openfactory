# syntax=docker/dockerfile:1.7
#
# Example tool image for Open Factory.
#
#   docker build -f images/tool-example/Dockerfile -t example.test/tool-hello:1 images/tool-example
#
# A tool image meets this contract (see docs/self-hosting/kubernetes.md):
#   - It has /bin/sh and cp. The backend runs
#     `cp -RP /opt/openfactory/tool/. /dest/` in an init container.
#   - It has all files of the tool under /opt/openfactory/tool/.
#   - The files are readable by the Pod user. Executable files keep their mode.
# The task container finds the files at /opt/openfactory/tools/<name>.
# This example holds one script.

FROM debian:bookworm-slim

RUN mkdir -p /opt/openfactory/tool/bin
COPY hello /opt/openfactory/tool/bin/hello
RUN chmod 0755 /opt/openfactory/tool/bin/hello

LABEL org.opencontainers.image.title="Open Factory example tool" \
      org.opencontainers.image.description="Example of the Open Factory tool image contract" \
      org.opencontainers.image.source="https://github.com/openfactory-org/openfactory" \
      org.opencontainers.image.licenses="Apache-2.0"
