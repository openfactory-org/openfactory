#!/bin/sh
# Start the emulator on loopback with telemetry off, the Pod Identity broker
# (settings CLUSTER_NAME and POD_IDENTITY_EXPIRY_CAP come from the
# environment), and the TLS front as the foreground process. A watcher stops
# the container when the emulator or the broker exits, so the Pod restarts
# instead of running half broken.
set -eu

# alive PID: true while the process runs. A process that exited stays a
# zombie (tlsfront, the new PID 1, does not reap it), so check the state too.
alive() {
  kill -0 "$1" 2>/dev/null || return 1
  while read -r key value; do
    if [ "$key" = "State:" ]; then
      case "$value" in Z*) return 1 ;; esac
    fi
  done < "/proc/$1/status" 2>/dev/null || return 0
  return 0
}

# The home directory is an emptyDir, so it keeps the per-run files of the
# previous container after a restart. Remove them: the TLS front must trust
# the CA of this run, not the old one.
run_dir="$HOME/.aws/emulator/9297"
rm -rf "$run_dir"

aws-emulator -b 127.0.0.1 -p 9297 -t off &
emulator=$!

# Wait for this run's CA before the TLS front reads it.
tries=0
until [ -s "$run_dir/ca.pem" ]; do
  tries=$((tries + 1))
  if [ "$tries" -gt 120 ] || ! alive "$emulator"; then
    echo "entrypoint: the emulator did not write $run_dir/ca.pem" >&2
    exit 1
  fi
  sleep 0.5
done
podidentity &
broker=$!
(
  while alive "$emulator" && alive "$broker"; do
    sleep 2
  done
  echo "entrypoint: the emulator or the Pod Identity broker exited; stopping the container" >&2
  kill 1
) &
exec tlsfront -listen :9443 -upstream https://127.0.0.1:9297 \
  -upstream-ca "$HOME/.aws/emulator/9297/ca.pem"
