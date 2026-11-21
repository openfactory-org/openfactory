#!/bin/sh
# Checks of the local test stack (make local-up). See docs/self-hosting/local-testing.md.
#
#   sh scripts/local/check.sh              # make local-check does this
#   LOCAL_CHECKS=R1,R2,A2 sh scripts/local/check.sh
#
# It prints one line for each check:
#   PASS <ID> <name>
#   FAIL <ID> <name>: <reason>
#   SKIP <ID> <name>: <reason>
# The first line names the directory of the logs. The last line is
# "RESULT: PASS" or "RESULT: FAIL". Exit status: 0 pass, 1 fail, 2 wrong setup.
# The details of each check are in $LOCAL_DIR/check-logs/<timestamp>/<ID>.log.
# The logs never hold a secret: the script masks the known secrets in them.
#
# Settings (environment variables). The file $LOCAL_DIR/env sets most of them:
#   LOCAL_DIR          Directory of the stack. Default: ~/.openfactory-local
#   OF_SERVER, OF_API_KEY   Server URL and API key of the stack.
#   LOCAL_SERVER_MODE  chart (default) or host. The checks A2, A3, A4, N1, H1 and H2 need chart:
#                      host servers are outside the cluster (no NetworkPolicy path, no shared address).
#   LOCAL_MODEL        fake (default) or claude. R4 needs claude: it runs the real CLI.
#   LOCAL_FORGE        none (default) or github. G1 to G4 need github.
#   LOCAL_DOMAIN       Domain of the host names. Default: k8s.orb.local
#   KUBE_CONTEXT       kubectl context. Default: orbstack (kind-<name> for a kind cluster).
#                      Every kubectl call uses it.
#   LOCAL_CHECKS       Comma-separated IDs. Only these checks run.
#   LOCAL_GITHUB_REPO, GITHUB_TOKEN   Only for G1 to G4 (set them in the environment of
#                      this script). Without them the G checks are SKIP.
#   LOCAL_TARGET       local (default) or eks. eks: a real EKS cluster with real S3 and Pod
#                      Identity. A1 and A3 are SKIP; A2, A4, S1 and S2 use the real services.
#   LOCAL_CHECK_IMAGE, LOCAL_PULL_POLICY, LOCAL_PG_HOST   Image and pull policy of the check
#                      Pods, and the PostgreSQL host for psql (default 127.0.0.1 in the Pod).
#   LOCAL_GIT_PORT     Port of the git server on the workstation. Default: 9418.
#   LOCAL_R3_WAIT=1    R3 waits for the hourly retention sweep (up to LOCAL_R3_WAIT_MAX seconds,
#                      default 10800) instead of failing as "not observed yet".
#
# The checks start runs with the claude harness. The stack installs a stand-in
# for the claude CLI that returns fixed answers (deploy/local/testkit), so the
# checks cost nothing. The test factories are in scripts/local/factories/.

set -u

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
PATH="$HOME/.orbstack/bin:$PATH"
export PATH

case ${1:-} in
-h | --help | help)
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac

LOCAL_DIR=${LOCAL_DIR:-$HOME/.openfactory-local}
if [ ! -f "$LOCAL_DIR/env" ]; then
	echo "error: $LOCAL_DIR/env does not exist. Start the stack first: make local-up" >&2
	exit 2
fi
# shellcheck disable=SC1090,SC1091
. "$LOCAL_DIR/env"
LOCAL_SERVER_MODE=${LOCAL_SERVER_MODE:-chart}
LOCAL_MODEL=${LOCAL_MODEL:-fake}
LOCAL_FORGE=${LOCAL_FORGE:-none}
# LOCAL_TARGET=eks runs the checks against a real EKS cluster (see docs/self-hosting/local-testing.md).
LOCAL_TARGET=${LOCAL_TARGET:-local}
case $LOCAL_TARGET in local | eks) ;; *)
	echo "error: LOCAL_TARGET must be local or eks, not $LOCAL_TARGET" >&2
	exit 2
	;;
esac
LOCAL_DOMAIN=${LOCAL_DOMAIN:-k8s.orb.local}
KUBE_CONTEXT=${KUBE_CONTEXT:-orbstack}
OF_SERVER=${OF_SERVER:-http://of.$LOCAL_DOMAIN}
OF_API_KEY=${OF_API_KEY:-}
if [ -z "$OF_API_KEY" ]; then
	echo "error: OF_API_KEY is empty in $LOCAL_DIR/env" >&2
	exit 2
fi
case $LOCAL_SERVER_MODE in chart | host) ;; *)
	echo "error: LOCAL_SERVER_MODE must be chart or host, not $LOCAL_SERVER_MODE" >&2
	exit 2
	;;
esac
export LOCAL_GIT_PORT LOCAL_CHECK_IMAGE LOCAL_PULL_POLICY LOCAL_PG_HOST LOCAL_TARGET LOCAL_DIR LOCAL_SERVER_MODE LOCAL_MODEL LOCAL_FORGE LOCAL_DOMAIN KUBE_CONTEXT OF_SERVER OF_API_KEY

# shellcheck source=lib/common.sh
. "$HERE/lib/common.sh"
# shellcheck source=lib/runs.sh
. "$HERE/lib/runs.sh"
# shellcheck source=lib/checks_aws.sh
. "$HERE/lib/checks_aws.sh"
# shellcheck source=lib/checks_platform.sh
. "$HERE/lib/checks_platform.sh"
# shellcheck source=lib/checks_runs.sh
. "$HERE/lib/checks_runs.sh"
# shellcheck source=lib/checks_ha.sh
. "$HERE/lib/checks_ha.sh"
# shellcheck source=lib/checks_hooks.sh
. "$HERE/lib/checks_hooks.sh"

ALL_CHECKS="A1 A2 A3 A4 P1 P2 P3 S1 S2 R1 R2 R3 R4 N1 N2 N3 H1 H2 W1 W2 W3 D1 G1 G2 G3 G4 UI1"

check_name() {
	case $1 in
	A1) echo "Provisioning on the emulator succeeds, and a second run changes nothing" ;;
	A2) echo "The server writes run data with Pod Identity credentials, and its Pods have no AWS_ACCESS_KEY_ID" ;;
	A3) echo "The server renews its Pod Identity credentials before they expire" ;;
	A4) echo "A service account without a Pod Identity association gets no credentials" ;;
	P1) echo "Both server replicas are ready" ;;
	P2) echo "The database migrations ran once" ;;
	P3) echo "The Kubernetes preflight and the NetworkPolicy probe passed, and one replica holds the leader lease" ;;
	S1) echo "The S3 driver passes the live tests against the emulator" ;;
	S2) echo "A task Pod opens a presigned URL that the server made" ;;
	R1) echo "A run with the claude CLI stand-in puts its transcript, artifact and snapshot in S3" ;;
	R2) echo "A follow-up after an idle timeout of 0 starts a new Pod that restores the snapshot and resumes" ;;
	R3) echo "A prompt attachment is uploaded, used in a message, and deleted by retention" ;;
	R4) echo "A run with the claude harness reads the rules files and answers" ;;
	N1) echo "The network tests of make e2e-k8s pass, and a restricted run shows a blocked host in its diagnostics" ;;
	N2) echo "A task Job that passes its deadline fails with a clear failure code" ;;
	N3) echo "A Role without jobs makes the preflight fail with a clear message" ;;
	H1) echo "When the leader replica stops during a turn, the turn finishes and the other replica takes the lease" ;;
	H2) echo "A rolling update of the chart during a turn does not stop the turn" ;;
	W1) echo "A webhook ping gets 200" ;;
	W2) echo "A delivery with a bad signature gets 401" ;;
	W3) echo "A delivery that is sent again starts no second turn" ;;
	D1) echo "A managed definition: apply, a new version, an invalid change refused, a restore" ;;
	G1) echo "An issue with the label factory:<alias> makes a pull request" ;;
	G2) echo "A review comment makes a follow-up commit and a reply in the review thread" ;;
	G3) echo "A merge closes the work item" ;;
	G4) echo "A pull request that changes the definition gets the openfactory/definition status" ;;
	UI1) echo "The dashboard pages load and show the runs of the checks" ;;
	esac
}

# check_timeout ID  The time limit of a check, in seconds.
check_timeout() {
	case $1 in
	A1) echo 600 ;;
	A3) echo 420 ;;
	R3)
		if [ "${LOCAL_R3_WAIT:-}" = 1 ]; then echo "${LOCAL_R3_WAIT_MAX:-10800}"; else echo 600; fi
		;;
	N1) echo 1500 ;;
	N2) echo 900 ;;
	N3 | S1) echo 480 ;;
	R4) echo 900 ;;
	H1 | H2) echo 600 ;;
	G1 | G2 | G3 | G4) echo 900 ;;
	*) echo 360 ;;
	esac
}

# check_needs ID  The setting that a check needs: chart, claude, github or nothing.
check_needs() {
	case $1 in
	A2 | A3 | A4 | N1 | H1 | H2) echo chart ;;
	R4) echo claude ;;
	G1 | G2 | G3 | G4) echo github ;;
	esac
}

# skip_reason ID  Print why the settings exclude a check. Print nothing when it can run.
skip_reason() {
	case $(check_needs "$1") in
	chart) [ "$LOCAL_SERVER_MODE" = chart ] || echo "needs LOCAL_SERVER_MODE=chart (it is $LOCAL_SERVER_MODE)" ;;
	claude) [ "$LOCAL_MODEL" = claude ] || echo "needs LOCAL_MODEL=claude (it is $LOCAL_MODEL)" ;;
	github)
		if [ "$LOCAL_FORGE" != github ]; then
			echo "needs LOCAL_FORGE=github (it is $LOCAL_FORGE)"
		elif [ -z "${LOCAL_GITHUB_REPO:-}" ]; then
			echo "LOCAL_GITHUB_REPO is not set in the environment of this script"
		elif [ -z "${GITHUB_TOKEN:-}" ]; then
			echo "GITHUB_TOKEN is not set in the environment of this script"
		fi
		;;
	esac
}

# ---------- select the checks ----------

WANTED=
if [ -n "${LOCAL_CHECKS:-}" ]; then
	for _id in $(printf '%s' "$LOCAL_CHECKS" | tr 'a-z,' 'A-Z '); do
		case " $ALL_CHECKS " in
		*" $_id "*) WANTED="$WANTED $_id" ;;
		*)
			echo "error: unknown check '$_id' in LOCAL_CHECKS. The checks are: $ALL_CHECKS" >&2
			exit 2
			;;
		esac
	done
fi
# The checks run in the order of the table, whatever the order in LOCAL_CHECKS.
SELECTED=
for _id in $ALL_CHECKS; do
	if [ -z "${LOCAL_CHECKS:-}" ]; then
		SELECTED="$SELECTED $_id"
	else
		case " $WANTED " in *" $_id "*) SELECTED="$SELECTED $_id" ;; esac
	fi
done

# ---------- logs and secrets ----------

STAMP=$(date +%Y%m%d-%H%M%S)
LOGDIR=$LOCAL_DIR/check-logs/$STAMP
mkdir -p "$LOGDIR" || {
	echo "error: cannot make $LOGDIR" >&2
	exit 2
}
chmod 700 "$LOCAL_DIR/check-logs" "$LOGDIR" 2>/dev/null
BASE=$(mktemp -d "${TMPDIR:-/tmp}/of-local-check.XXXXXX") || exit 2
STATE_DIR=$BASE/state
mkdir -p "$STATE_DIR"
MASK_WEBHOOK=
MASK_DB=
MASK_KEY=
[ ! -f "$LOCAL_DIR/webhook.secret" ] || MASK_WEBHOOK=$(tr -d '\n' <"$LOCAL_DIR/webhook.secret")
[ ! -f "$LOCAL_DIR/db.password" ] || MASK_DB=$(tr -d '\n' <"$LOCAL_DIR/db.password")
[ ! -f "$LOCAL_DIR/secret.key" ] || MASK_KEY=$(tr -d '\n' <"$LOCAL_DIR/secret.key")
MASK_AWS_ID=
MASK_AWS_SECRET=
if [ -f "$LOCAL_DIR/aws/credentials" ]; then
	MASK_AWS_ID=$(sed -n 's/^aws_access_key_id *= *//p' "$LOCAL_DIR/aws/credentials" | head -n 1)
	MASK_AWS_SECRET=$(sed -n 's/^aws_secret_access_key *= *//p' "$LOCAL_DIR/aws/credentials" | head -n 1)
fi
export MASK_WEBHOOK MASK_DB MASK_KEY MASK_AWS_ID MASK_AWS_SECRET

# shellcheck disable=SC2329 # invoked by the trap below
cleanup() {
	rm -rf "$BASE"
}
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT TERM

printf 'Logs: %s\n' "$LOGDIR"

# ---------- run one check ----------

# run_one ID  Run a check in a subshell with a time limit, and print its line.
run_one() {
	_ro_id=$1
	_ro_name=$(check_name "$_ro_id")
	_ro_why=$(skip_reason "$_ro_id")
	if [ -n "$_ro_why" ]; then
		printf 'SKIP %s %s: %s\n' "$_ro_id" "$_ro_name" "$_ro_why"
		printf 'SKIP %s %s: %s\n' "$_ro_id" "$_ro_name" "$_ro_why" >>"$LOGDIR/summary.txt"
		return 0
	fi
	_ro_limit=$(check_timeout "$_ro_id")
	_ro_result=$BASE/result.$_ro_id
	rm -f "$_ro_result" "$BASE/timeout.$_ro_id"
	(
		CHECK_ID=$_ro_id
		CHECK_NAME=$_ro_name
		CHECK_LOG=$LOGDIR/$_ro_id.log
		RESULT_FILE=$_ro_result
		WORK=$BASE/work-$_ro_id
		mkdir -p "$WORK"
		: >"$CHECK_LOG"
		chmod 600 "$CHECK_LOG"
		exec >>"$CHECK_LOG" 2>&1
		log "check $CHECK_ID: $CHECK_NAME (limit $_ro_limit s, mode $LOCAL_SERVER_MODE, model $LOCAL_MODEL, forge $LOCAL_FORGE)"
		"check_$CHECK_ID"
		fail "the check ended without a result"
	) &
	_ro_pid=$!
	(
		_ro_i=0
		while [ "$_ro_i" -lt "$_ro_limit" ]; do
			kill -0 "$_ro_pid" 2>/dev/null || exit 0
			sleep 1
			_ro_i=$((_ro_i + 1))
		done
		: >"$BASE/timeout.$_ro_id"
		kill -TERM "$_ro_pid" 2>/dev/null
		sleep 2
		kill -KILL "$_ro_pid" 2>/dev/null
	) >/dev/null 2>&1 &
	_ro_wd=$!
	wait "$_ro_pid"
	_ro_rc=$?
	wait "$_ro_wd" 2>/dev/null
	mask_file "$LOGDIR/$_ro_id.log"
	_ro_status=FAIL
	_ro_reason=
	if [ -s "$_ro_result" ]; then
		_ro_status=$(cut -f1 "$_ro_result")
		_ro_reason=$(cut -f2- "$_ro_result")
	elif [ -f "$BASE/timeout.$_ro_id" ]; then
		_ro_reason="the check did not end in $_ro_limit seconds"
	else
		_ro_reason="the check script stopped without a result (exit status $_ro_rc), see the log"
	fi
	case $_ro_status in
	PASS) _ro_line="PASS $_ro_id $_ro_name" ;;
	SKIP) _ro_line="SKIP $_ro_id $_ro_name: $_ro_reason" ;;
	*)
		_ro_status=FAIL
		_ro_line="FAIL $_ro_id $_ro_name: $_ro_reason"
		;;
	esac
	# The line is built from our own text and from reasons that the checks wrote
	# after they masked nothing: mask known secrets here too.
	printf '%s\n' "$_ro_line" >"$BASE/line.$_ro_id"
	mask_file "$BASE/line.$_ro_id"
	cat "$BASE/line.$_ro_id"
	cat "$BASE/line.$_ro_id" >>"$LOGDIR/summary.txt"
	[ "$_ro_status" != FAIL ] || : >"$BASE/failed"
}

for _id in $SELECTED; do
	run_one "$_id"
done

if [ -f "$BASE/failed" ]; then
	echo "RESULT: FAIL"
	echo "RESULT: FAIL" >>"$LOGDIR/summary.txt"
	exit 1
fi
echo "RESULT: PASS"
echo "RESULT: PASS" >>"$LOGDIR/summary.txt"
exit 0
