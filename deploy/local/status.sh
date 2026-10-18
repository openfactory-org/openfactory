#!/bin/sh
# Show the state of each part of the local stack. Exit status 0 when every part
# is healthy, 1 otherwise. Prints no secret.
#
#   make local-status
set -eu
LOCAL_USE_SAVED=1
. "$(dirname "$0")/lib.sh"
cd "$LOCAL_REPO"

BAD=0
ok() { printf '  ok    %-22s %s\n' "$1" "$2"; }
bad() { printf '  FAIL  %-22s %s\n' "$1" "$2"; BAD=1; }

have kubectl || die "kubectl is not on the PATH"
echo "Local stack (context $KUBE_CONTEXT, server mode $LOCAL_SERVER_MODE, domain $LOCAL_DOMAIN)"
if ! k get ns >/dev/null 2>&1; then
	bad cluster "the context $KUBE_CONTEXT does not answer"
	exit 1
fi

# ready_of NS NAME  prints "ready/wanted" of a Deployment, or nothing when it does not exist.
ready_of() {
	k -n "$1" get deploy "$2" -o jsonpath='{.status.readyReplicas}/{.spec.replicas}' 2>/dev/null | sed 's#^/#0/#'
}

# deploy_check NS NAME  ok when every replica is ready.
deploy_check() {
	r=$(ready_of "$1" "$2" || true)
	if [ -z "$r" ]; then
		bad "$1/$2" "missing"
	elif [ "${r%/*}" = "${r#*/}" ] && [ "${r%/*}" != 0 ]; then
		ok "$1/$2" "$r ready"
	else
		bad "$1/$2" "$r ready"
	fi
}

# ---------- AWS emulator ----------
deploy_check "$NS_AWS" aws-emulator
# OrbStack sometimes fails the first lookup of a *.k8s.orb.local name: try 3 times.
emulator_health() {
	[ -f "$LOCAL_DIR/aws/ca.crt" ] || return 1
	for _ in 1 2 3; do
		curl -fsS --max-time 5 --cacert "$LOCAL_DIR/aws/ca.crt" "https://$AWS_HOST:9443/__admin/health" >/dev/null 2>&1 && return 0
		sleep 1
	done
	return 1
}
if emulator_health; then
	ok "emulator health" "https://$AWS_HOST:9443"
	cluster=$(jq -r '.clusterName // "of-local"' "$LOCAL_DIR/outputs.json" 2>/dev/null || echo of-local)
	if ! emulator_env_ok; then
		bad "emulator state" "the stored emulator keys are refused: the emulator lost its state. Run: make local-up"
	elif ! (
		# shellcheck source=/dev/null
		. "$LOCAL_DIR/aws/connect.env" && aws eks describe-cluster --name "$cluster" >/dev/null 2>&1
	); then
		bad "emulator state" "the cluster $cluster does not exist: the emulator lost its state. Run: make local-up"
	else
		ok "emulator state" "account 000000000000, cluster $cluster"
	fi
else
	bad "emulator health" "no answer at https://$AWS_HOST:9443 (does $AWS_HOST resolve? is $LOCAL_DIR/aws/ca.crt there?). Run: make local-up"
fi

# ---------- infrastructure ----------
for d in postgres git; do
	deploy_check "$NS_INFRA" "$d"
done

# ---------- server ----------
url=$(server_url)
if [ "$LOCAL_SERVER_MODE" = host ]; then
	out=$(sh deploy/local/host.sh status 2>&1 || true)
	printf '%s\n' "$out" | while IFS= read -r line; do
		case $line in
		*"not running"*) printf '  FAIL  %s\n' "$line" ;;
		*) printf '  ok    %s\n' "$line" ;;
		esac
	done
	case $out in *"not running"*) BAD=1 ;; esac
	for port in "$HOST_PORT_1" "$HOST_PORT_2"; do
		if curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$port/readyz" 2>/dev/null; then
			ok "readyz :$port" "ready"
		else
			bad "readyz :$port" "not ready: see $LOCAL_DIR/logs"
		fi
	done
else
	deploy_check "$NS_SERVER" "$RELEASE"
	if curl -fsS --max-time 5 -o /dev/null "$url/readyz" 2>/dev/null; then
		ok "readyz" "$url"
	else
		bad "readyz" "no answer at $url/readyz"
	fi
fi

# ---------- API key ----------
key=$(env_value OF_API_KEY)
if [ -z "$key" ]; then
	bad "API key" "$LOCAL_DIR/env has no key. Run: make local-up"
else
	code=$(curl_auth "$key" -s -o /dev/null -w '%{http_code}' --max-time 10 "$url/api/v1/me" || true)
	if [ "$code" = 200 ]; then
		ok "API key" "accepted by $url"
	else
		bad "API key" "HTTP $code from $url/api/v1/me. Run: make local-up (or make local-down if the database was reset)"
	fi
fi

if [ "$BAD" = 0 ]; then
	echo "RESULT: HEALTHY"
else
	echo "RESULT: UNHEALTHY"
	exit 1
fi
