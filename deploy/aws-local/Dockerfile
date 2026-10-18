# Test-only image: the AWS emulator, tlsfront and the Pod Identity broker.
# NOT FOR PRODUCTION. Build it with up.sh, which compiles tlsfront and
# podidentity first.
FROM public.ecr.aws/amazonlinux/amazonlinux:2023-minimal
ARG TARGETARCH
# Always the latest emulator release; rebuild with --no-cache to upgrade.
RUN case "$TARGETARCH" in arm64) a=aarch64 ;; amd64) a=x86_64 ;; *) exit 1 ;; esac \
    && curl -fsSLo /tmp/aws-emulator.rpm \
       "https://downloads.aws.com/aws-emulator/packages/aws-emulator/LTS/latest/aws-emulator-$a.rpm" \
    && rpm -i /tmp/aws-emulator.rpm && rm /tmp/aws-emulator.rpm \
    && mkdir -p /home/emulator && chown 1000 /home/emulator
COPY tlsfront /usr/local/bin/tlsfront
COPY podidentity /usr/local/bin/podidentity
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
USER 1000
ENV HOME=/home/emulator
ENV PATH=/opt/aws-emulator/bin:$PATH
EXPOSE 9443
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
