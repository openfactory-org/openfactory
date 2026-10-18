#!/bin/sh
# Real-world checks that need a real server, a real forge, a real cloud account
# or the real harness CLIs. CI cannot run them. Run them on your own machine.
# The steps and the expected results are in docs/self-hosting/local-testing.md#real-world-checks.
#
#   sh scripts/checks/real-world.sh setup
#   sh scripts/checks/real-world.sh forge-refresh     # a push 2 hours after the sandbox started
#   sh scripts/checks/real-world.sh cloud-refresh     # a cloud read at the end of a 90-minute turn
#   sh scripts/checks/real-world.sh harness-turn [claude] [codex]   # optional
#
# The script starts one run through the REST API, waits for it, and checks the
# result. It prints PASS or FAIL for each check, and the last line is
# "RESULT: PASS" or "RESULT: FAIL". Exit status: 0 pass, 1 fail, 2 wrong setup.
# It needs curl and jq. It never prints a secret, and it holds no account.
#
# Settings that all commands need (environment variables):
#   OF_SERVER          URL of the server. Default: http://localhost:8080
#   OF_API_KEY         API key of an editor or admin.
#   OF_CHECK_FACTORY   Factory UID, name or alias. Its definition must allow the check
#                      (see the page). It should have exactly one repository.
#   OF_CHECK_AGENT     Agent that starts the run. Default: the foreman. Use an agent that
#                      runs shell commands and can push (for example implement).
#   OF_CHECK_WAIT_SECONDS   Length of the wait. forge-refresh: 7200. cloud-refresh: 5400.
#                      A shorter value is a rehearsal. It does not prove the refresh.
#   OF_CHECK_DRY_RUN=1 Print the request and stop. Nothing starts.
#   OF_CHECK_RUN_ID    Watch a run that already started, in place of a new run.
#
# forge-refresh also needs:
#   OF_CHECK_REPO_URL  URL of the repository of the factory, as your git can read it.
#                      Your own git credentials read the branch back with git ls-remote.
#   OF_CHECK_BRANCH    Only with OF_CHECK_RUN_ID: the branch name of that run.
#
# cloud-refresh also needs:
#   OF_CHECK_CLOUD     aws or gcp. The factory must declare that cloud in cloudProviders.
#   OF_CHECK_BUCKET    Name of a bucket that the cloud role can read.
#   OF_CHECK_OBJECT    Key of an object in that bucket.
#   OF_CHECK_EXPECTED  The exact content of that object (one line, a random value).
#   OF_CHECK_CLOUD_COMMAND  Optional. The command that prints the object. Default for aws:
#                      aws s3 cp s3://BUCKET/OBJECT -   Default for gcp: gcloud storage cat gs://BUCKET/OBJECT
#
# harness-turn options:
#   OF_CHECK_CLAUDE_SECRET, OF_CHECK_CODEX_SECRET   Optional. The name of a managed secret that
#                      holds the API key. If you set neither a secret nor a model, the script
#                      sends no harness override. The agent then keeps its own harness, model and
#                      auth, and it must already use the harness that you test.
#   OF_CHECK_CLAUDE_MODEL, OF_CHECK_CODEX_MODEL     Optional model names. If you set a secret or a
#                      model, the run uses a harness of that type with only these settings.
#   OF_CHECK_RULE_MARKER  Optional. A word that a rule file of the repository tells the agent to
#                      add to every reply. The reply must then contain it.
#   OF_CHECK_RESUME=1  Also check the resume in a new sandbox.
#   OF_CHECK_TURN_TIMEOUT   Seconds to wait for one turn. Default: 900.

set -eu

OF_SERVER=${OF_SERVER:-http://localhost:8080}
POLL=${OF_CHECK_POLL:-30}

PASSES=0
FAILS=0

log() { printf '==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
usage_error() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

pass() {
	PASSES=$((PASSES + 1))
	printf '  PASS  %s\n' "$1"
}

fail() {
	FAILS=$((FAILS + 1))
	printf '  FAIL  %s\n' "$1"
	if [ -n "${2:-}" ]; then
		printf '%s\n' "$2" | sed 's/^/        /'
	fi
}

finish() {
	printf '\n%s checks passed, %s failed\n' "$PASSES" "$FAILS"
	if [ "$FAILS" -gt 0 ]; then
		echo "RESULT: FAIL"
		exit 1
	fi
	echo "RESULT: PASS"
	exit 0
}

need_tools() {
	for t in curl jq; do
		command -v "$t" >/dev/null 2>&1 || usage_error "$t is not on the PATH"
	done
}

# need_env NAME... exits with status 2 when a variable is empty.
need_env() {
	for v in "$@"; do
		eval "val=\${$v:-}"
		[ -n "$val" ] || usage_error "set $v (see the top of this script, or --help)"
	done
}

# api METHOD PATH [BODY]  Call the REST API. It prints the body of the answer.
api() {
	if [ -n "${3:-}" ]; then
		curl -fsS -X "$1" -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
			-d "$3" "${OF_SERVER%/}/api/v1$2"
	else
		curl -fsS -X "$1" -H "Authorization: Bearer $OF_API_KEY" "${OF_SERVER%/}/api/v1$2"
	fi
}

now() { date +%s; }

# resolve_factory prints the UID of OF_CHECK_FACTORY.
resolve_factory() {
	case $OF_CHECK_FACTORY in
	fac_*)
		printf '%s\n' "$OF_CHECK_FACTORY"
		return
		;;
	esac
	q=$(printf '%s' "$OF_CHECK_FACTORY" | jq -sRr @uri)
	uid=$(api GET "/factories?search=$q" |
		jq -r --arg n "$OF_CHECK_FACTORY" '[.items[]? | select(.name == $n or .alias == $n or .uid == $n or .id == $n)][0] | (.uid // .id) // empty')
	[ -n "$uid" ] || usage_error "no factory has the name or alias \"$OF_CHECK_FACTORY\""
	printf '%s\n' "$uid"
}

# start_run PROMPT TITLE CHECK [OVERRIDES_JSON]  Start a run of the factory. It sets RUN_ID.
start_run() {
	body=$(jq -n --arg prompt "$1" --arg title "$2" --arg check "$3" \
		--arg agent "${OF_CHECK_AGENT:-}" --argjson overrides "${4:-null}" \
		'{prompt: $prompt, title: $title, metadata: {check: $check}}
		 + (if $agent != "" then {agent: $agent} else {} end)
		 + (if $overrides != null then {overrides: $overrides} else {} end)')
	if [ "${OF_CHECK_DRY_RUN:-}" = 1 ]; then
		log "dry run: this request would start the run"
		printf 'POST %s/api/v1/factories/<uid of %s>/runs\n' "${OF_SERVER%/}" "$OF_CHECK_FACTORY"
		printf '%s\n' "$body" | jq .
		exit 0
	fi
	uid=$(resolve_factory)
	resp=$(api POST "/factories/$uid/runs" "$body") || usage_error "the server refused the run"
	RUN_ID=$(printf '%s\n' "$resp" | jq -r '.run_id // empty')
	[ -n "$RUN_ID" ] || usage_error "the answer has no run_id: $resp"
	log "started run $RUN_ID"
	printf '%s\n' "$resp" | jq -r '.url // empty | "    " + .'
}

# wait_run RUN_ID MAX_SECONDS  Wait until the run is terminal, or blocked, or
# the time ends. It sets RUN_STATE (a run state, or TIMEOUT) and RUN_JSON.
wait_run() {
	id=$1
	max=$2
	t0=$(now)
	last=0
	first=1
	while :; do
		RUN_JSON=$(api GET "/runs/$id") || RUN_JSON=""
		RUN_STATE=$(printf '%s\n' "$RUN_JSON" | jq -r '.state // empty' 2>/dev/null || true)
		case $RUN_STATE in
		SUCCEEDED | FAILED | ERROR | CANCELLED | BLOCKED) return 0 ;;
		esac
		el=$(($(now) - t0))
		if [ "$el" -ge "$max" ]; then
			RUN_STATE=TIMEOUT
			return 0
		fi
		if [ "$first" = 1 ] || [ $((el - last)) -ge 600 ]; then
			note "waiting: run $id is ${RUN_STATE:-unknown}, $((el / 60)) minutes passed"
			last=$el
			first=0
		fi
		sleep "$POLL"
	done
}

# transcript RUN_ID ROLES AFTER  Print the messages of the run, one JSON object per line.
transcript() {
	run=$1
	roles=$2
	after=${3:-0}
	while :; do
		q="limit=1000&after=$after"
		[ -z "$roles" ] || q="$q&role=$roles"
		page=$(api GET "/runs/$run/messages?$q") || return 1
		printf '%s\n' "$page" | jq -c '(.messages // .items // [])[]'
		more=$(printf '%s\n' "$page" | jq -r '.has_more // false')
		[ "$more" = true ] || break
		after=$(printf '%s\n' "$page" | jq -r '.next_after')
	done
}

# transcript_text RUN_ID ROLES AFTER  The same, as plain text (all strings of each message).
transcript_text() {
	transcript "$@" | jq -r '[.. | strings] | join(" ")'
}

# report_run_state  Check that the run ended well. It needs RUN_STATE and RUN_JSON.
report_run_state() {
	if [ "$RUN_STATE" = SUCCEEDED ]; then
		pass "the run finished with the state SUCCEEDED"
	else
		msg=$(printf '%s\n' "$RUN_JSON" | jq -r '(.status_message.message // .error // "") + " " + (.status_message.failure_code // "")' 2>/dev/null || true)
		fail "the run ended with the state ${RUN_STATE:-unknown}, not SUCCEEDED" "$msg"
	fi
}

# max_elapsed TEXT  Print the highest number after OF_CHECK_ELAPSED= in the text.
max_elapsed() {
	printf '%s\n' "$1" | grep -o 'OF_CHECK_ELAPSED=[0-9][0-9]*' | sed 's/.*=//' | sort -n | tail -n 1
}

# wait_steps SECONDS  The shared steps of a long wait in the sandbox.
wait_steps() {
	cat <<PROMPT
1. Run this command to record the start time: date +%s > /tmp/of-check-start
2. Wait. Run "sleep 240" again and again. Each sleep command must take less than 5 minutes. After each sleep, run this command: echo \$(( \$(date +%s) - \$(cat /tmp/of-check-start) ))
   Stop when the printed number is at least $1.
3. Run this command and keep its output: echo "OF_CHECK_ELAPSED=\$(( \$(date +%s) - \$(cat /tmp/of-check-start) ))"
PROMPT
}

# ---------- setup ----------

cmd_setup() {
	need_tools
	need_env OF_API_KEY OF_CHECK_FACTORY
	log "check the setup"
	if me=$(api GET /me 2>&1); then
		pass "the server answers and accepts the API key ($(printf '%s\n' "$me" | jq -r '.email // .name // "ok"' 2>/dev/null))"
	else
		fail "the server $OF_SERVER does not accept the API key" "$me"
		finish
	fi
	if uid=$(resolve_factory 2>&1); then
		pass "the factory exists ($uid)"
	else
		fail "the factory $OF_CHECK_FACTORY was not found" "$uid"
	fi
	finish
}

# ---------- forge-refresh (forge token) ----------

cmd_forge_refresh() {
	need_tools
	need_env OF_API_KEY OF_CHECK_FACTORY OF_CHECK_REPO_URL
	wait_s=${OF_CHECK_WAIT_SECONDS:-7200}
	if [ -n "${OF_CHECK_RUN_ID:-}" ]; then
		need_env OF_CHECK_BRANCH
		RUN_ID=$OF_CHECK_RUN_ID
		branch=$OF_CHECK_BRANCH
		log "watch the run $RUN_ID (branch $branch)"
	else
		branch="of-check-$(now)"
		prompt="This is an automated credential check. Follow the steps in order. Do nothing else. Do not ask questions.
$(wait_steps "$wait_s")
4. Create the branch $branch from the current HEAD: git checkout -b $branch
5. Create the file of-check/$branch.txt with one line: credential refresh check. Run git add on it. Then run: git commit -m \"credential refresh check\"
6. Run: git push origin $branch
   Do not change the remote URL. Do not change any git credential setting. Do not print any token.
7. Reply with the word DONE and the output of: git rev-parse HEAD"
		started=$(now)
		log "forge token refresh: the agent waits $wait_s seconds, then pushes the branch $branch"
		if [ "$wait_s" -lt 3700 ]; then
			note "NOTE: a wait below 3700 seconds is a rehearsal. A forge App token lives 1 hour, so it does not prove the refresh."
		fi
		start_run "$prompt" "Credential check: forge push after $wait_s seconds" forge-refresh
	fi
	t0=${started:-$(now)}
	wait_run "$RUN_ID" $((wait_s + ${OF_CHECK_TIMEOUT_EXTRA:-3600}))

	report_run_state
	elapsed_wall=$(($(now) - t0))
	text=$(transcript_text "$RUN_ID" tool_result,assistant 0 2>/dev/null || true)
	reported=$(max_elapsed "$text")
	if [ -z "$reported" ]; then
		fail "the transcript has no OF_CHECK_ELAPSED line, so the wait is not proven"
	elif [ "$reported" -ge "$wait_s" ]; then
		pass "the agent pushed $reported seconds after it started the wait (needed: $wait_s)"
	else
		fail "the agent waited only $reported seconds (needed: $wait_s)"
	fi
	if refs=$(git ls-remote --exit-code "$OF_CHECK_REPO_URL" "refs/heads/$branch" 2>&1); then
		pass "the branch $branch exists on the remote ($(printf '%s' "$refs" | cut -c1-12))"
	else
		fail "the branch $branch is not on the remote. The push after the wait did not work" "$refs"
	fi
	note "wall time of this script: $elapsed_wall seconds. Delete the branch when you are done: git push $OF_CHECK_REPO_URL --delete $branch"
	finish
}

# ---------- cloud-refresh (cloud identity token) ----------

cmd_cloud_refresh() {
	need_tools
	need_env OF_API_KEY OF_CHECK_FACTORY OF_CHECK_CLOUD OF_CHECK_BUCKET OF_CHECK_OBJECT OF_CHECK_EXPECTED
	wait_s=${OF_CHECK_WAIT_SECONDS:-5400}
	case $OF_CHECK_CLOUD in
	aws) default_cmd="aws s3 cp s3://$OF_CHECK_BUCKET/$OF_CHECK_OBJECT -" ;;
	gcp) default_cmd="gcloud storage cat gs://$OF_CHECK_BUCKET/$OF_CHECK_OBJECT" ;;
	*) usage_error "OF_CHECK_CLOUD must be aws or gcp" ;;
	esac
	cloud_cmd=${OF_CHECK_CLOUD_COMMAND:-$default_cmd}
	if [ -n "${OF_CHECK_RUN_ID:-}" ]; then
		RUN_ID=$OF_CHECK_RUN_ID
		log "watch the run $RUN_ID"
	else
		prompt="This is an automated credential check. Follow the steps in order. Do nothing else. Do not ask questions.
Do not run any aws, gcloud or other cloud command before step 4.
$(wait_steps "$wait_s")
4. Run this shell command: printf 'OF_CHECK_RESULT=%s\\n' \"\$($cloud_cmd)\"
   Run the command as it is. Do not change any credential setting or environment variable.
5. Reply with the word DONE and the output line of step 4."
		log "cloud identity refresh ($OF_CHECK_CLOUD): the agent waits $wait_s seconds, then reads the object"
		if [ "$wait_s" -lt 3700 ]; then
			note "NOTE: a wait below 3700 seconds is a rehearsal. A cloud identity token lives 1 hour, so it does not prove the refresh."
		fi
		start_run "$prompt" "Credential check: cloud read after $wait_s seconds" cloud-refresh
	fi
	wait_run "$RUN_ID" $((wait_s + ${OF_CHECK_TIMEOUT_EXTRA:-3600}))

	report_run_state
	text=$(transcript_text "$RUN_ID" tool_result,assistant 0 2>/dev/null || true)
	reported=$(max_elapsed "$text")
	if [ -z "$reported" ]; then
		fail "the transcript has no OF_CHECK_ELAPSED line, so the wait is not proven"
	elif [ "$reported" -ge "$wait_s" ]; then
		pass "the agent read the object $reported seconds after it started the wait (needed: $wait_s)"
	else
		fail "the agent waited only $reported seconds (needed: $wait_s)"
	fi
	if printf '%s\n' "$text" | grep -qF "OF_CHECK_RESULT=$OF_CHECK_EXPECTED"; then
		pass "the object read from the bucket has the expected content"
	else
		fail "the transcript does not show OF_CHECK_RESULT=<expected content>. The cloud read at the end did not work" \
			"$(printf '%s\n' "$text" | grep -o 'OF_CHECK_RESULT=.\{0,80\}' | head -n 3)"
	fi
	finish
}

# ---------- harness-turn (optional) ----------

# harness_check NAME  One turn with the real CLI of the harness.
harness_check() {
	h=$1
	upper=$(printf '%s' "$h" | tr '[:lower:]' '[:upper:]')
	secret='' model=''
	eval "secret=\${OF_CHECK_${upper}_SECRET:-}"
	eval "model=\${OF_CHECK_${upper}_MODEL:-}"
	word="OFWORD$(now)"
	log "one turn with the real $h CLI"
	# A harness override with a type replaces the whole harness of the agent, with its
	# auth and model. So the script sends one only when you give a secret or a model.
	# Without them, the agent keeps its own harness and auth, and the script checks
	# that the harness of the run is the one you asked for.
	overrides=$(jq -n --arg t "$h" --arg s "$secret" --arg m "$model" --arg resume "${OF_CHECK_RESUME:-}" \
		'(if $s != "" or $m != "" then
			{harness: ({type: $t}
				+ (if $s != "" then {auth: {source: "managedSecret", secretName: $s}} else {} end)
				+ (if $m != "" then {model: $m} else {} end))}
		  else {} end)
		 + (if $resume == "1" then {idle_timeout_minutes: 1} else {} end)')
	prompt="This is an automated check of the $h harness. The code word is $word. Reply with exactly one line: PONG $word"
	start_run "$prompt" "Harness check: $h" "harness-$h" "$overrides"
	wait_run "$RUN_ID" "${OF_CHECK_TURN_TIMEOUT:-900}"
	report_run_state
	used=$(printf '%s\n' "$RUN_JSON" | jq -r '.config.harness.type // empty' 2>/dev/null || true)
	if [ "$used" = "$h" ]; then
		pass "the run used the $h harness"
	else
		fail "the run used the harness \"${used:-unknown}\", not $h. Let the agent use the $h harness, or set OF_CHECK_${upper}_SECRET"
	fi
	text=$(transcript_text "$RUN_ID" assistant 0 2>/dev/null || true)
	if printf '%s\n' "$text" | grep -qF "PONG $word"; then
		pass "$h answered the prompt (the real CLI ran in the sandbox)"
	else
		fail "$h did not answer with PONG $word" "$(printf '%s\n' "$text" | tail -n 3 | cut -c1-200)"
	fi
	if [ -n "${OF_CHECK_RULE_MARKER:-}" ]; then
		if printf '%s\n' "$text" | grep -qF "$OF_CHECK_RULE_MARKER"; then
			pass "$h followed the rule file of the repository (the reply has $OF_CHECK_RULE_MARKER)"
		else
			fail "the reply of $h does not have $OF_CHECK_RULE_MARKER. The CLI did not read the rule file"
		fi
	fi
	if [ "${OF_CHECK_RESUME:-}" = 1 ]; then
		if [ "$RUN_STATE" != SUCCEEDED ]; then
			fail "the resume was not checked, because the first turn did not succeed"
			return
		fi
		last=$(transcript "$RUN_ID" "" 0 | jq -s 'map(.seq) | max // 0')
		log "wait for the sandbox of $RUN_ID to end (idle timeout 1 minute)"
		t0=$(now)
		while :; do
			running=$(api GET "/runs/$RUN_ID" | jq -r '.sandbox_running // false')
			[ "$running" = false ] && break
			if [ $(($(now) - t0)) -ge "${OF_CHECK_RESUME_WAIT:-600}" ]; then
				break
			fi
			sleep "$POLL"
		done
		if [ "$running" != false ]; then
			fail "the sandbox of the run did not end, so the resume cannot be checked"
			return
		fi
		resp=$(api POST "/runs/$RUN_ID/messages" \
			"$(jq -n '{message: "What was the code word in my first message? Reply with exactly one line: CODE=<word>"}')") ||
			{
				fail "the server refused the follow-up message"
				return
			}
		outcome=$(printf '%s\n' "$resp" | jq -r '.outcome // empty')
		if [ "$outcome" = new_session ]; then
			pass "the follow-up started a new session (outcome new_session)"
		else
			fail "the follow-up did not start a new session (outcome: ${outcome:-none})"
		fi
		t0=$(now)
		reply=""
		while [ $(($(now) - t0)) -lt "${OF_CHECK_TURN_TIMEOUT:-900}" ]; do
			reply=$(transcript_text "$RUN_ID" assistant "$last" 2>/dev/null || true)
			printf '%s\n' "$reply" | grep -q 'CODE=' && break
			sleep "$POLL"
		done
		if printf '%s\n' "$reply" | grep -qF "CODE=$word"; then
			pass "$h remembered the code word in the new sandbox (the conversation was restored)"
		else
			fail "the reply after the resume does not have CODE=$word" "$(printf '%s\n' "$reply" | tail -n 3 | cut -c1-200)"
		fi
	fi
}

cmd_harness_turn() {
	need_tools
	need_env OF_API_KEY OF_CHECK_FACTORY
	[ $# -gt 0 ] || set -- claude codex
	for h in "$@"; do
		case $h in claude | codex) ;; *) usage_error "unknown harness: $h (use claude or codex)" ;; esac
	done
	for h in "$@"; do
		harness_check "$h"
	done
	finish
}

# ---------- main ----------

case ${1:-} in
setup)
	cmd_setup
	;;
forge-refresh)
	cmd_forge_refresh
	;;
cloud-refresh)
	cmd_cloud_refresh
	;;
harness-turn)
	shift
	cmd_harness_turn "$@"
	;;
-h | --help | help | "")
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
	;;
*)
	usage_error "unknown command: $1 (use setup, forge-refresh, cloud-refresh or harness-turn)"
	;;
esac
