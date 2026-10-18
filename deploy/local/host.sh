#!/bin/sh
# Host mode of the local stack: two `openfactory serve` processes on the
# workstation (ports 18081 and 18082). Test only.
#
#   sh deploy/local/host.sh start    Write the env file, stop old processes, start two new ones.
#   sh deploy/local/host.sh stop     Stop the processes.
#   sh deploy/local/host.sh status   Print one line for each process. Exit 1 when one is not running.
#
# The servers run bin/openfactory (make build). They reach PostgreSQL, the fake
# model and the AWS emulator by their host names, use the static keys of the
# emulator (aws/server.env), and start sandbox Pods in the cluster through a
# kubeconfig of the stack's context. Sandbox Pods call back on host.orb.internal.
# Files in $LOCAL_DIR: server-host.env, kubeconfig, pod-template.yaml, data/<n>,
# logs/server-<n>.log, run/server-<n>.pid.
set -eu
LOCAL_USE_SAVED=1
. "$(dirname "$0")/lib.sh"

BIN=$LOCAL_REPO/bin/openfactory
ENVFILE=$LOCAL_DIR/server-host.env
RUNDIR=$LOCAL_DIR/run
LOGDIR=$LOCAL_DIR/logs

port_of() { if [ "$1" = 1 ]; then echo "$HOST_PORT_1"; else echo "$HOST_PORT_2"; fi; }

is_ours() { # PID: true when it is an openfactory process
	[ -n "$1" ] && ps -p "$1" -o command= 2>/dev/null | grep -q 'openfactory'
}

pid_of() { [ -f "$RUNDIR/server-$1.pid" ] && cat "$RUNDIR/server-$1.pid" || true; }

stop_all() {
	for n in 1 2; do
		pid=$(pid_of "$n")
		if is_ours "$pid"; then
			kill "$pid" 2>/dev/null || true
			i=0
			while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 70 ]; do
				i=$((i + 1))
				sleep 0.2
			done
			kill -9 "$pid" 2>/dev/null || true
			note "stopped the host server $n (pid $pid)"
		fi
		rm -f "$RUNDIR/server-$n.pid"
	done
}

write_env() {
	[ -x "$BIN" ] || die "$BIN is missing: run make build"
	[ -f "$LOCAL_DIR/aws/server.env" ] || die "$LOCAL_DIR/aws/server.env is missing: the emulator step did not run"
	mkdir -p "$LOCAL_DIR/data/1" "$LOCAL_DIR/data/2" "$LOGDIR" "$RUNDIR"
	# A kubeconfig with the stack's context only, with the credentials inside.
	(umask 077 && k config view --minify --flatten > "$LOCAL_DIR/kubeconfig")
	chmod 600 "$LOCAL_DIR/kubeconfig"
	bucket=$(jq -r '.bucket // empty' "$LOCAL_DIR/outputs.json")
	[ -n "$bucket" ] || die "no bucket in $LOCAL_DIR/outputs.json"
	(
		umask 077
		{
			echo "# Environment of the host-mode servers of the local stack (test only). Do not share: it holds keys."
			echo "export OF_DATABASE_URL='postgres://openfactory:$(secret_value "$LOCAL_DIR/db.password")@$DB_HOST:5432/openfactory?sslmode=disable'"
			echo "export OF_SECRET_KEY='$(secret_value "$LOCAL_DIR/secret.key")'"
			echo "export OF_PUBLIC_URL='http://127.0.0.1:$HOST_PORT_1'"
			echo "export OF_OBJECT_STORE_URL='s3://$bucket/runs?region=us-east-1&endpoint=https://$AWS_HOST:9443&path_style=true'"
			echo "export OF_OBJECT_STORE_RETENTION='$LOCAL_RETENTION'"
			echo "export OF_OBJECT_STORE_RETENTION_INTERVAL=1m"
			echo "export OF_LOG_FORMAT='text'"
			echo "export OF_SANDBOX_BACKEND='kubernetes'"
			echo "export OF_K8S_NAMESPACE='$NS_SANDBOXES'"
			echo "export OF_KUBECONFIG='$LOCAL_DIR/kubeconfig'"
			echo "export OF_K8S_POD_TEMPLATE='$LOCAL_DIR/pod-template.yaml'"
			echo "export OF_K8S_IMAGE_PULL_POLICY='Never'"
			echo "export OF_SANDBOX_IMAGE='$(image_ref sandbox)'"
			echo "export OF_RUNNER_IMAGE='$(image_ref server)'"
			echo "export OF_CLAUDE_TOOL_IMAGE='$STANDIN_IMAGE'"
			# The static keys of the emulator and its CA (AWS_CA_BUNDLE). No Pod Identity here.
			grep '^export AWS_' "$LOCAL_DIR/aws/server.env"
		} > "$ENVFILE"
	)
	chmod 600 "$ENVFILE"
}

start_one() {
	n=$1
	port=$(port_of "$n")
	log_file=$LOGDIR/server-$n.log
	# nohup, env and sh all exec the next program, so $! is the pid of the server.
	# shellcheck disable=SC2016 # $1 and $2 belong to the inner sh -c; they expand later, not here
	nohup env OF_LISTEN=":$port" \
		OF_INTERNAL_URL="http://host.orb.internal:$port" \
		OF_REPLICA_ID="host-$n" \
		OF_DATA_DIR="$LOCAL_DIR/data/$n" \
		sh -c 'set -a; . "$1"; set +a; exec "$2" serve' sh "$ENVFILE" "$BIN" \
		</dev/null >>"$log_file" 2>&1 &
	echo $! > "$RUNDIR/server-$n.pid"
	note "host server $n: pid $!, port $port, log $log_file"
}

case ${1:-} in
start)
	check_settings
	mkdir -p "$RUNDIR" "$LOGDIR"
	stop_all
	write_env
	start_one 1
	start_one 2
	sleep 1
	for n in 1 2; do
		is_ours "$(pid_of "$n")" || die "host server $n stopped at once: see $LOGDIR/server-$n.log"
	done
	;;
stop)
	mkdir -p "$RUNDIR"
	stop_all
	;;
status)
	rc=0
	for n in 1 2; do
		pid=$(pid_of "$n")
		if is_ours "$pid"; then
			echo "host server $n: running (pid $pid, port $(port_of "$n"))"
		else
			echo "host server $n: not running"
			rc=1
		fi
	done
	exit $rc
	;;
*)
	echo "usage: sh deploy/local/host.sh start|stop|status" >&2
	exit 2
	;;
esac
