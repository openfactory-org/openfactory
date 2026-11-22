#!/bin/sh
# Adds a CA to the system trust store. bootstrap.sh calls it for the
# Cloudflare HTTPS intercept CA, which the platform mounts after start.
#
#   trust-ca.sh /etc/cloudflare/certs/cloudflare-containers-ca.crt
#
# Waits up to 10 seconds for the file. Exit status 1 when it does not appear.

set -eu

CA=${1:?usage: trust-ca.sh CA_FILE}
i=0
while [ ! -s "$CA" ]; do
	i=$((i + 1))
	if [ $i -gt 20 ]; then
		echo "trust-ca: $CA did not appear" >&2
		exit 1
	fi
	sleep 0.5
done

mkdir -p /usr/local/share/ca-certificates
cp "$CA" /usr/local/share/ca-certificates/openfactory-intercept.crt
if command -v update-ca-certificates >/dev/null 2>&1; then
	update-ca-certificates >/dev/null 2>&1 || cat "$CA" >>/etc/ssl/certs/ca-certificates.crt
else
	cat "$CA" >>/etc/ssl/certs/ca-certificates.crt
fi
