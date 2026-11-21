# shellcheck shell=sh
# Shared helpers of scripts/local/check.sh. Source this file; do not run it.
# POSIX sh. A check runs in its own subshell: pass, fail and skip write the
# result and end that subshell. All output of a check goes to its log file.

# ---------- logging and results ----------

# log TEXT...  Write one line to the log of the check.
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

# evidence TEXT...  A log line that names a fact which a PASS relies on.
evidence() { printf '%s EVIDENCE %s\n' "$(date +%H:%M:%S)" "$*"; }

# cancel_later RUN_ID  Cancel a run when the check ends. A run with idle_timeout_minutes 0
# can start a new session again and again, and it must not stay behind.
cancel_later() { printf '%s\n' "$1" >>"$WORK/cancel-runs"; }

_cancel_pending() {
	[ -f "$WORK/cancel-runs" ] || return 0
	while read -r _cp_r; do
		[ -z "$_cp_r" ] || API_TIMEOUT=10 api_call POST "/runs/$_cp_r/cancel" '{}'
	done <"$WORK/cancel-runs"
	rm -f "$WORK/cancel-runs"
}

_result() {
	_cancel_pending >/dev/null 2>&1
	_r_status=$1
	shift
	_r_text=$(printf '%s' "$*" | tr '\n\t' '  ' | cut -c1-400)
	printf '%s\t%s\n' "$_r_status" "$_r_text" >"$RESULT_FILE"
	log "$_r_status $*"
	exit 0
}
pass() { _result PASS "${1:-}"; }
fail() { _result FAIL "$*"; }
skip() { _result SKIP "$*"; }
# not_implemented WHY  A check that cannot be done faithfully fails, it never passes.
not_implemented() { fail "not implemented: $*"; }

# need_cmd NAME...  Fail the check when a tool is missing.
need_cmd() {
	for _nc in "$@"; do
		command -v "$_nc" >/dev/null 2>&1 || fail "the tool $_nc is not on the PATH"
	done
}

# need_file PATH  Fail the check when a file of the stack is missing.
need_file() {
	[ -f "$1" ] || fail "the file $1 does not exist (is the stack up? run make local-up)"
}

# need_chart  The check needs the Helm chart mode.
need_chart() {
	[ "$LOCAL_SERVER_MODE" = chart ] || skip "needs LOCAL_SERVER_MODE=chart (it is $LOCAL_SERVER_MODE)"
}

# Run a command, log it and its output, and keep its status.
run_logged() {
	log "\$ $*"
	"$@"
	_rl=$?
	[ "$_rl" -eq 0 ] || log "(exit status $_rl)"
	return "$_rl"
}

# ---------- clocks and polling ----------

now() { date +%s; }

# wait_until MAX_SECONDS INTERVAL COMMAND...  Poll until the command succeeds.
# It works with shell functions. Returns 1 when the time ends.
wait_until() {
	_wu_max=$1
	_wu_iv=$2
	shift 2
	_wu_t0=$(now)
	while :; do
		if "$@"; then
			return 0
		fi
		[ $(($(now) - _wu_t0)) -lt "$_wu_max" ] || return 1
		sleep "$_wu_iv"
	done
}

# iso_to_epoch TEXT  Print the epoch seconds of an RFC 3339 time.
iso_to_epoch() {
	printf '%s\n' "$1" | jq -Rr 'sub("\\.[0-9]+";"") | sub("\\+00:00$";"Z") | fromdateiso8601' 2>/dev/null
}

# rand_token  A short random word for one check (letters and digits).
rand_token() {
	od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n'
}

# ---------- kubectl and aws (the only places that call them) ----------

# kc ARGS...  kubectl on the context of the stack, always.
kc() { kubectl --context "$KUBE_CONTEXT" "$@"; }

# aws_emu ARGS...  The AWS CLI against the emulator, in a subshell that
# removes real AWS credentials (connect.env does that).
aws_emu() {
	if [ "$LOCAL_TARGET" = eks ]; then
		aws_real "$@"
		return
	fi
	# shellcheck source=/dev/null
	( . "$LOCAL_DIR/aws/connect.env" && aws "$@" )
}

# aws_real ARGS...  The AWS CLI with the real credentials of the workstation.
# Only LOCAL_TARGET=eks may use it.
aws_real() { [ "$LOCAL_TARGET" = eks ] || fail "aws_real needs LOCAL_TARGET=eks"; aws "$@"; }

# aws_guard  Stop the check unless the AWS CLI talks to the emulator account.
aws_guard() {
	need_cmd aws
	if [ "$LOCAL_TARGET" = eks ]; then
		_ag=$(aws_real sts get-caller-identity --query Account --output text 2>&1) || fail "aws sts get-caller-identity failed: $_ag"
		[ "$_ag" = "$(out_json accountId)" ] || fail "aws sts get-caller-identity shows account $_ag, not $(out_json accountId) of outputs.json"
		return 0
	fi
	need_file "$LOCAL_DIR/aws/connect.env"
	# The first lookup of a host name can fail on OrbStack: try three times.
	_ag_try=0
	while :; do
		_ag_try=$((_ag_try + 1))
		_ag=$(aws_emu sts get-caller-identity --query Account --output text 2>&1) && break
		[ "$_ag_try" -lt 3 ] || fail "aws sts get-caller-identity failed: $_ag"
		sleep 3
	done
	[ "$_ag" = 000000000000 ] || fail "aws sts get-caller-identity shows account $_ag, not the emulator account 000000000000"
}

# ---------- values of the stack ----------

# out_json KEY  A value of outputs.json.
out_json() {
	jq -r --arg k "$1" '.[$k] // empty' "$LOCAL_DIR/outputs.json" 2>/dev/null
}

# server_pods  Names of the server Pods that are not ending (chart mode), one per line.
server_pods() {
	kc -n of-local get pods -l app.kubernetes.io/instance=openfactory,app.kubernetes.io/component=server -o json 2>/dev/null |
		jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name'
}

# server_logs  The logs of every server replica, labelled with the replica.
# Chart mode: kubectl logs of each Pod. Host mode: the log files of the stack.
server_logs() {
	if [ "${LOCAL_SERVER_MODE:-}" = chart ]; then
		for _sl_pod in $(server_pods); do
			kc -n of-local logs "$_sl_pod" -c server --tail=-1 2>&1 | sed "s|^|[$_sl_pod] |"
		done
	else
		for _sl_f in "$LOCAL_DIR"/logs/*; do
			[ -f "$_sl_f" ] || continue
			sed "s|^|[$(basename "$_sl_f")] |" "$_sl_f"
		done
	fi
}

# server_env_value NAME  A setting of the servers (Helm ConfigMap, or server-host.env).
server_env_value() {
	if [ "${LOCAL_SERVER_MODE:-}" = chart ]; then
		kc -n of-local get configmap openfactory -o json 2>/dev/null | jq -r --arg k "$1" '.data[$k] // empty'
	elif [ -f "$LOCAL_DIR/server-host.env" ]; then
		sed -n "s/^\(export \)\{0,1\}$1=//p" "$LOCAL_DIR/server-host.env" | tail -n 1 | sed "s/^['\"]//;s/['\"]$//"
	fi
}

# runner_image  The runner image of the stack (the server image). It has no curl.
runner_image() {
	_ri=$(server_env_value OF_RUNNER_IMAGE)
	if [ -z "$_ri" ]; then
		_ri=$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep '/openfactory:' | head -n 1)
	fi
	printf '%s\n' "$_ri"
}

# check_pod_image  The image of the Pods that the checks start: the testkit image
# of the stack (TESTKIT_IMAGE in deploy/local/lib.sh). It holds sh and curl.
check_pod_image() { echo "${LOCAL_CHECK_IMAGE:-of-local/testkit:latest}"; }

# psql_query SQL  Run a query in PostgreSQL (kubectl exec into the database
# Pod). The password goes in on standard input, never in the arguments.
psql_query() {
	_pq_pw=$(cat "$LOCAL_DIR/db.password" 2>/dev/null) || return 1
	_pq_target=deploy/postgres
	kc -n of-local-infra get deploy postgres >/dev/null 2>&1 || _pq_target=statefulset/postgres
	# shellcheck disable=SC2016 # $0 belongs to the inner sh -c in the Pod; it must expand later, not here
	printf '%s\n' "$_pq_pw" | kc -n of-local-infra exec -i "$_pq_target" -- env PGHOST="${LOCAL_PG_HOST:-127.0.0.1}" sh -c \
		'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -h "${PGHOST:-127.0.0.1}" -U openfactory -d openfactory -X -q -t -A -F "|" -c "$0"' "$1"
}

# ---------- HTTP ----------

# api_call METHOD PATH [BODY]  Call the REST API of the stack. It sets API_CODE
# and API_BODY. The key goes to curl on standard input, not in the arguments.
api_call() {
	_ac_m=$1
	_ac_p=$2
	_ac_b=${3:-}
	if [ -n "$_ac_b" ]; then
		printf '%s' "$_ac_b" >"$WORK/api.in"
		set -- -H 'Content-Type: application/json' --data-binary @"$WORK/api.in"
	else
		set --
	fi
	# A lookup of a *.k8s.orb.local name sometimes fails on OrbStack: try again
	# when curl itself fails (no HTTP answer), up to 5 times.
	for _ac_try in 1 2 3 4 5; do
		rm -f "$WORK/api.out"
		API_CODE=$(printf 'header = "Authorization: Bearer %s"\n' "$OF_API_KEY" |
			curl -sS -m "${API_TIMEOUT:-30}" -K - -X "$_ac_m" "$@" -o "$WORK/api.out" -w '%{http_code}' \
				"${OF_SERVER%/}/api/v1$_ac_p" 2>>"$CHECK_LOG") && break
		API_CODE=000
		sleep "$_ac_try"
	done
	API_BODY=$(cat "$WORK/api.out" 2>/dev/null || true)
}

# api_ok METHOD PATH [BODY]  Like api_call, but fail the check on a status outside 2xx.
api_ok() {
	api_call "$@"
	case $API_CODE in
	2??) ;;
	*) fail "$1 $2 answered $API_CODE: $(printf '%s' "$API_BODY" | cut -c1-200)" ;;
	esac
}

# api_upload PATH FILE CONTENT_TYPE  Send a file as the request body (POST).
api_upload() {
	for _au_try in 1 2 3 4 5; do
		rm -f "$WORK/api.out"
		API_CODE=$(printf 'header = "Authorization: Bearer %s"\n' "$OF_API_KEY" |
			curl -sS -m 60 -K - -X POST -H "Content-Type: $3" --data-binary @"$2" -o "$WORK/api.out" -w '%{http_code}' \
				"${OF_SERVER%/}/api/v1$1" 2>>"$CHECK_LOG") && break
		API_CODE=000
		sleep "$_au_try"
	done
	API_BODY=$(cat "$WORK/api.out" 2>/dev/null || true)
}

# urlenc TEXT  Percent-encode text for a query string.
urlenc() { printf '%s' "$1" | jq -sRr @uri; }

# http_code URL [CURL ARGS...]  Print the status code of a GET.
http_code() {
	_hc_url=$1
	shift
	_hc=$(curl -sS -m 20 -o /dev/null -w '%{http_code}' "$@" "$_hc_url" 2>>"$CHECK_LOG") ||
		_hc=$(curl -sS -m 20 -o /dev/null -w '%{http_code}' "$@" "$_hc_url" 2>>"$CHECK_LOG") || _hc=000
	printf '%s' "${_hc:-000}"
}

# ---------- masking ----------

# mask_file FILE  Replace the known secrets and signed URL parts in a log file.
mask_file() {
	[ -f "$1" ] || return 0
	MASK_1=${OF_API_KEY:-} MASK_2=${MASK_WEBHOOK:-} MASK_3=${MASK_DB:-} MASK_4=${MASK_KEY:-} MASK_5=${GITHUB_TOKEN:-} \
		MASK_6=${MASK_AWS_ID:-} MASK_7=${MASK_AWS_SECRET:-} \
		awk 'BEGIN { n = split("MASK_1 MASK_2 MASK_3 MASK_4 MASK_5 MASK_6 MASK_7", names, " ") }
		{
			for (i = 1; i <= n; i++) {
				v = ENVIRON[names[i]]
				if (length(v) < 8) continue
				while ((p = index($0, v)) > 0) $0 = substr($0, 1, p - 1) "***" substr($0, p + length(v))
			}
			print
		}' "$1" |
		sed -E -e 's/(X-Amz-Signature=)[0-9a-fA-F]*/\1***/g' \
			-e 's/(X-Amz-Credential=)[^& "]*/\1***/g' \
			-e 's/(X-Amz-Security-Token=)[^& "]*/\1***/g' \
			-e 's/(Authorization: *(Bearer|token|Basic) )[^ "]*/\1***/g' \
			-e 's/(sha256=)[0-9a-f]{64}/\1***/g' \
			-e 's/(of_[a-z0-9]*_)[A-Za-z0-9]{12,}/\1***/g' \
			-e 's/(gh[pousr]_)[A-Za-z0-9]{20,}/\1***/g' \
			-e 's/(github_pat_)[A-Za-z0-9_]{20,}/\1***/g' >"$1.masked" && mv "$1.masked" "$1"
}

# git_retry COMMAND...  Run a git command up to 5 times. A lookup of a
# *.k8s.orb.local name sometimes fails on OrbStack. A command that fails for
# another reason (for example a missing branch) fails 5 times, which only costs time.
git_retry() {
	for _gt_try in 1 2 3 4 5; do
		"$@" && return 0
		_gt_rc=$?
		[ "$_gt_try" = 5 ] || sleep "$_gt_try"
	done
	return "$_gt_rc"
}
