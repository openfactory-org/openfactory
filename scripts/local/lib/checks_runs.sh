# shellcheck shell=sh
# Checks R1 to R4 and N1 to N3. Source this file.

# duration_seconds TEXT  Seconds of a Go duration such as 90m, 2h or 1h30m.
duration_seconds() {
	printf '%s' "$1" | awk '{
		s = 0; str = $0
		while (match(str, /[0-9.]+(ms|h|m|s)/)) {
			v = substr(str, RSTART, RLENGTH); str = substr(str, RSTART + RLENGTH); n = v + 0
			if (v ~ /ms$/) s += n / 1000; else if (v ~ /h$/) s += n * 3600; else if (v ~ /m$/) s += n * 60; else s += n
		}
		print int(s)
	}'
}

# wait_snapshot RUN_ID SESSION MAX_SECONDS  Wait until a session of the run has saved
# the workspace part of its snapshot (GET /runs/{id}/sessions). The timeline
# endpoint has the same fact, but the sessions endpoint is the evidence here.
wait_snapshot() {
	_wt_t0=$(now)
	while :; do
		api_call GET "/runs/$1/sessions"
		if printf '%s' "$API_BODY" | jq -e --argjson s "$2" '[.items[]? | select(.number == $s and .snapshot.workspace == true)] | length > 0' >/dev/null 2>&1; then
			return 0
		fi
		[ $(($(now) - _wt_t0)) -lt "$3" ] || return 1
		sleep 3
	done
}

# timeline_has RUN_ID EVENT_TYPE SESSION  Log whether the timeline has an event. It never
# fails a check by itself: the sessions give the evidence, the timeline is a second look.
timeline_has() {
	api_call GET "/runs/$1/timeline?limit=500"
	case $API_CODE in
	2??)
		if printf '%s' "$API_BODY" | jq -e --arg t "$2" --argjson s "$3" '[.items[]? | select(.type == $t and .session == $s)] | length > 0' >/dev/null 2>&1; then
			evidence "the timeline has $2 for session $3"
		else
			log "the timeline has no $2 for session $3"
		fi
		;;
	*) log "GET /runs/$1/timeline answers $API_CODE" ;;
	esac
}

# wait_sandbox_gone RUN_ID MAX_SECONDS  Wait until the run has no live sandbox.
wait_sandbox_gone() {
	_ws_t0=$(now)
	while :; do
		run_get "$1"
		[ "$(printf '%s' "$RUN_JSON" | jq -r '.sandbox_running // false')" = false ] && return 0
		[ $(($(now) - _ws_t0)) -lt "$2" ] || return 1
		sleep 3
	done
}

# ---------- R1 ----------

check_R1() {
	need_cmd kubectl jq git aws
	aws_guard
	fx_check_factory git
	_r1_tok=$(rand_token)
	_r1_branch=of-local-r1-$_r1_tok
	_r1_art=r1-$_r1_tok.txt
	_r1_sh="D=repo; [ -d repo/.git ] || D=.; cd \$D && git checkout -q -b $_r1_branch && echo r1-$_r1_tok > r1.txt && git add r1.txt && git commit -q -m r1-$_r1_tok && git push -q origin $_r1_branch && echo PUSHED-$_r1_tok"
	_r1_prompt="R1 $_r1_tok. $(tool_directive write_file "$(jq -nc --arg p ".openfactory/artifacts/$_r1_art" --arg c "r1 $_r1_tok" '{path: $p, content: $c}')") $(tool_directive bash "$(jq -nc --arg c "$_r1_sh" '{command: $c}')")"
	run_dispatch "$FACTORY_UID" "$_r1_prompt" coder '{"idle_timeout_minutes":1}'
	_r1_run=$RUN_ID
	expect_reply "$_r1_run" 0 assistant 'DONE' 300 "the turn"
	expect_reply "$_r1_run" 0 tool_result "PUSHED-$_r1_tok" 30 "the push in the sandbox"
	# The branch is on the git server.
	_r1_ref=$(git_retry git ls-remote "$(git_url_host "$CHECK_REPO")" "refs/heads/$_r1_branch" 2>&1)
	[ -n "$_r1_ref" ] || fail "the branch $_r1_branch is not on the git server after the run pushed it"
	evidence "the agent cloned the repository from the git server and pushed $_r1_branch ($(printf '%s' "$_r1_ref" | cut -c1-8))"
	# The transcript of the run: the API serves the messages of the run.
	api_call GET "/runs/$_r1_run/transcript"
	[ "$(printf '%s\n' "$API_BODY" | grep -c .)" -ge 4 ] || fail "the transcript of the run has fewer than 4 messages"
	printf '%s\n' "$API_BODY" | grep -q "PUSHED-$_r1_tok" || fail "the transcript does not hold the tool result of the push"
	evidence "the transcript of the run has $(printf '%s\n' "$API_BODY" | grep -c .) messages, with the tool result of the push"
	# The session ends one minute after the turn (idle timeout 1), and it saves the snapshot.
	wait_sandbox_gone "$_r1_run" 240 || fail "the sandbox of the run did not end 240 seconds after the turn (idle_timeout_minutes 1)"
	wait_snapshot "$_r1_run" 1 120 || fail "session 1 of the run did not save the workspace part of its snapshot"
	evidence "session 1 ended and saved the workspace snapshot"
	timeline_has "$_r1_run" snapshot.saved 1
	# The artifact and the snapshot are objects in the bucket.
	_r1_ok=
	_r1_t0=$(now)
	while [ $(($(now) - _r1_t0)) -lt 90 ]; do
		_r1_keys=$(bucket_keys) || fail "aws s3 ls s3://$(out_json bucket) failed"
		if printf '%s\n' "$_r1_keys" | grep -q "runs/$_r1_run/artifacts/$_r1_art" &&
			printf '%s\n' "$_r1_keys" | grep -Eq "snapshots/[^/]+/$_r1_run/"; then
			_r1_ok=1
			break
		fi
		sleep 4
	done
	printf '%s\n' "$_r1_keys" | grep "$_r1_run" | sed 's/^/    key: /'
	[ -n "$_r1_ok" ] || fail "the bucket does not hold both runs/$_r1_run/artifacts/$_r1_art and snapshots/<team>/$_r1_run/ after 90 seconds"
	evidence "the bucket $(out_json bucket) holds the artifact $_r1_art and the snapshot of run $_r1_run"
	pass
}

# ---------- R2 ----------

# The workspace part of a snapshot holds the repositories of the run and the files that the
# run declares (OF_SNAPSHOT_DECLARATIONS). A loose file in the workspace is not in it. So
# this check does not look for a file. It looks for the facts of a restore: a new session in
# a new Pod, the snapshot of session 1 in the bucket, the event snapshot.restored of session 2,
# and the claude CLI that resumes its session from the harness state of the snapshot.
check_R2() {
	need_cmd kubectl jq aws
	aws_guard
	fx_check_factory
	_r2_tok=$(rand_token)
	_r2_cmd1="echo R2-TURN1-$_r2_tok"
	_r2_p1="R2 turn 1 $_r2_tok. $(tool_directive bash "$(jq -nc --arg c "$_r2_cmd1" '{command: $c}')")"
	run_dispatch "$FACTORY_UID" "$_r2_p1" "" '{"idle_timeout_minutes":0}'
	_r2_run=$RUN_ID
	cancel_later "$_r2_run"
	expect_reply "$_r2_run" 0 assistant 'DONE' 300 "turn 1"
	wait_sandbox_gone "$_r2_run" 180 || fail "the sandbox of the run did not end after turn 1 (idle_timeout_minutes 0)"
	wait_snapshot "$_r2_run" 1 120 || fail "session 1 did not save the workspace part of its snapshot"
	evidence "session 1 ended after the turn and saved the workspace snapshot"
	_r2_keys=$(bucket_keys) || fail "aws s3 ls failed"
	printf '%s\n' "$_r2_keys" | grep -Eq "snapshots/[^/]+/$_r2_run/1/workspace/manifest.json" ||
		fail "the bucket has no snapshots/<team>/$_r2_run/1/workspace/manifest.json after session 1"
	evidence "the bucket holds the snapshot of session 1 (workspace/manifest.json)"
	api_call GET "/runs/$_r2_run/sessions"
	printf '%s' "$API_BODY" | jq -e '[.items[]? | select(.number == 1 and .snapshot.harness_state == true)] | length > 0' >/dev/null ||
		fail "session 1 did not save the harness_state part of its snapshot (the session file of the claude CLI)"
	evidence "session 1 saved the session file of the claude CLI (harness_state)"
	run_get "$_r2_run"
	_r2_sid=$(printf '%s' "$RUN_JSON" | jq -r '.session_id // empty')
	[ -n "$_r2_sid" ] || fail "the run has no harness session id after turn 1"
	log "task Pods of session 1:"
	task_pods "$_r2_run" | sed 's/^/    /'
	_r2_after=$(last_seq "$_r2_run")
	# The follow-up starts a new session in a new Pod.
	_r2_p2="R2 turn 2 $_r2_tok. [[reply: R2-TURN2-$_r2_tok]]"
	api_ok POST "/runs/$_r2_run/messages" "$(jq -nc --arg m "$_r2_p2" '{message: $m}')"
	_r2_outcome=$(printf '%s' "$API_BODY" | jq -r '.outcome // empty')
	[ "$_r2_outcome" = new_session ] || fail "the follow-up outcome is '$_r2_outcome', not new_session"
	evidence "the follow-up message started a new session (outcome new_session)"
	expect_reply "$_r2_run" "$_r2_after" assistant "R2-TURN2-$_r2_tok" 300 "turn 2 in the new session"
	api_call GET "/runs/$_r2_run/sessions"
	printf '%s' "$API_BODY" | jq -c '.items[]? | {number, state, end_reason, snapshot}' | sed 's/^/    /'
	_r2_sessions=$(printf '%s' "$API_BODY" | jq '[.items[].number] | unique | length')
	[ "$_r2_sessions" -ge 2 ] || fail "the run has $_r2_sessions session(s), not 2"
	log "task Pods of the run now:"
	task_pods "$_r2_run" | sed 's/^/    /'
	_r2_ids=$(task_pods "$_r2_run" | awk '{print $2}' | sort -u | grep -c .)
	if [ "$_r2_ids" -ge 2 ]; then
		evidence "the cluster shows $_r2_ids task ids for the run (one Pod for each session)"
	fi
	# The claude CLI resumes: --resume finds the session file that the snapshot restored.
	# The CLI fails on a session that it does not find, so the same session id is the evidence.
	run_get "$_r2_run"
	_r2_sid2=$(printf '%s' "$RUN_JSON" | jq -r '.session_id // empty')
	[ "$_r2_sid2" = "$_r2_sid" ] || fail "the harness session changed from '$_r2_sid' to '$_r2_sid2': the claude CLI did not resume"
	evidence "the claude CLI of session 2 resumed the session $_r2_sid"
	# The new session restored the snapshot: the platform records it in the timeline.
	api_call GET "/runs/$_r2_run/timeline?limit=500"
	case $API_CODE in 2??) ;; *) fail "GET /runs/$_r2_run/timeline answers $API_CODE, so snapshot.restored of session 2 cannot be read" ;; esac
	printf '%s' "$API_BODY" | jq -r '.items[]? | "    \(.session // "-") \(.type)"'
	printf '%s' "$API_BODY" | jq -e '[.items[]? | select(.type == "snapshot.restored" and .session == 2)] | length > 0' >/dev/null ||
		fail "the timeline has no snapshot.restored event for session 2"
	evidence "the timeline has snapshot.restored for session 2"
	pass
}

# ---------- R3 ----------

check_R3() {
	need_cmd kubectl jq aws curl
	aws_guard
	fx_check_factory
	_r3_ret=$(server_env_value OF_OBJECT_STORE_RETENTION)
	[ -n "$_r3_ret" ] || fail "the servers have no OF_OBJECT_STORE_RETENTION, so retention cannot delete the attachment"
	_r3_ret_s=$(duration_seconds "$_r3_ret")
	[ "$_r3_ret_s" -gt 0 ] || fail "OF_OBJECT_STORE_RETENTION=$_r3_ret is not a duration"
	mkdir -p "$LOCAL_DIR/check-state"
	_r3_state=$LOCAL_DIR/check-state/r3-attachments
	touch "$_r3_state"
	_r3_tok=$(rand_token)
	_r3_name=r3-$_r3_tok.txt
	# 1. Upload.
	printf 'R3FILE=%s\n' "$_r3_tok" >"$WORK/r3.txt"
	api_upload "/attachments?name=$_r3_name" "$WORK/r3.txt" text/plain
	[ "$API_CODE" = 201 ] || fail "POST /attachments answers $API_CODE: $(printf '%s' "$API_BODY" | cut -c1-200)"
	_r3_id=$(printf '%s' "$API_BODY" | jq -r '.attachment_id // .id // empty')
	[ -n "$_r3_id" ] || fail "the upload answer has no attachment id"
	_r3_up=$(now)
	printf '%s %s %s\n' "$_r3_id" "$_r3_up" "$_r3_name" >>"$_r3_state"
	evidence "uploaded the attachment $_r3_id ($_r3_name)"
	# 2. Use it in a message to a run.
	run_dispatch "$FACTORY_UID" "R3 $_r3_tok. Wait for the file." "" ""
	_r3_run=$RUN_ID
	expect_reply "$_r3_run" 0 assistant 'READY' 240 "the first turn"
	_r3_after=$(last_seq "$_r3_run")
	_r3_msg="R3 read $_r3_tok. $(tool_directive bash "$(jq -nc '{command: "cat .openfactory/attachments/*/'"$_r3_name"'"}')")"
	api_ok POST "/runs/$_r3_run/messages" "$(jq -nc --arg m "$_r3_msg" --arg a "$_r3_id" '{message: $m, attachments: [$a]}')"
	expect_reply "$_r3_run" "$_r3_after" tool_result "R3FILE=$_r3_tok" 240 "the agent reads the attachment in its sandbox"
	evidence "the agent read the attachment from .openfactory/attachments/ in its task Pod (downloaded with a presigned URL)"
	# 3. The file is in the bucket.
	_r3_keys=$(bucket_keys) || fail "aws s3 ls failed"
	printf '%s\n' "$_r3_keys" | grep -q "attachments/.*$_r3_id/$_r3_name" || fail "the bucket has no object attachments/<team>/$_r3_id/$_r3_name"
	evidence "the bucket holds attachments/<team>/$_r3_id/$_r3_name"
	# 4. Retention deletes it. The leader prunes every OF_OBJECT_STORE_RETENTION_INTERVAL
	# (default 1h). The deadline is the retention, one sweep interval and 5 minutes.
	_r3_every=$(server_env_value OF_OBJECT_STORE_RETENTION_INTERVAL)
	_r3_every_s=$(duration_seconds "${_r3_every:-1h}")
	[ "$_r3_every_s" -gt 0 ] || _r3_every_s=3600
	_r3_slack=$((_r3_every_s + 300))
	_r3_verified=0
	while read -r _r3_oid _r3_ots _r3_oname; do
		[ -n "$_r3_oid" ] || continue
		[ "$_r3_oid" != "$_r3_id" ] || continue
		_r3_age=$(($(now) - _r3_ots))
		[ "$_r3_age" -ge $((_r3_ret_s + _r3_slack)) ] || continue
		if printf '%s\n' "$_r3_keys" | grep -q "$_r3_oid"; then
			fail "retention is $_r3_ret, but the attachment $_r3_oid of an earlier check ($((_r3_age / 60)) minutes old) is still in the bucket"
		fi
		_r3_verified=$((_r3_verified + 1))
		log "the attachment $_r3_oid of an earlier check ($((_r3_age / 60)) minutes old) is gone: retention deleted it"
	done <"$_r3_state"
	if [ "$_r3_verified" -gt 0 ]; then
		evidence "retention ($_r3_ret) deleted $_r3_verified attachment(s) of earlier checks"
		pass
	fi
	# Wait when asked, or when the deadline is short (at most 15 minutes).
	if [ "${LOCAL_R3_WAIT:-}" = 1 ] || [ $((_r3_ret_s + _r3_slack)) -le 900 ]; then
		log "wait for the retention sweep (retention $_r3_ret, sweep every ${_r3_every_s}s)"
		_r3_limit=$((_r3_ret_s + _r3_slack))
		while [ $(($(now) - _r3_up)) -lt "$_r3_limit" ]; do
			_r3_keys=$(bucket_keys) || fail "aws s3 ls failed"
			if ! printf '%s\n' "$_r3_keys" | grep -q "$_r3_id"; then
				evidence "retention deleted the attachment $_r3_id $(($(now) - _r3_up)) seconds after the upload"
				pass
			fi
			sleep 60
		done
		fail "the attachment $_r3_id is still in the bucket $((_r3_limit / 60)) minutes after the upload (retention $_r3_ret)"
	fi
	fail "retention is not observed yet: the attachment $_r3_id was uploaded and used, and the leader deletes it after $_r3_ret, in a sweep every ${_r3_every_s}s (at the latest about $(((_r3_ret_s + _r3_slack) / 60)) minutes after the upload). Run R3 again later, or set LOCAL_R3_WAIT=1"
}

# ---------- R4 ----------

check_R4() {
	need_cmd kubectl jq git
	api_call GET /secrets/CLAUDE_OAUTH_TOKEN
	[ "$API_CODE" = 200 ] || fail "the team secret CLAUDE_OAUTH_TOKEN does not exist (openfactory secret set CLAUDE_OAUTH_TOKEN --type anthropic_oauth_token)"
	fx_check_factory git
	_r4_tok=$(rand_token)
	_r4_ov='{"idle_timeout_minutes":0,"harness":{"type":"claude","model":"claude-sonnet-5-5","auth":{"source":"managedSecret","secretName":"CLAUDE_OAUTH_TOKEN"}}}'
	_r4_prompt="This is an automated check. Read the rule files of the repository. Then reply with exactly one line: PONG $_r4_tok"
	run_dispatch "$FACTORY_UID" "$_r4_prompt" coder "$_r4_ov"
	_r4_run=$RUN_ID
	expect_reply "$_r4_run" 0 assistant "PONG $_r4_tok" 600 "the claude harness answers"
	run_get "$_r4_run"
	_r4_h=$(printf '%s' "$RUN_JSON" | jq -r '.config.harness.type // empty')
	[ "$_r4_h" = claude ] || fail "the run used the harness '$_r4_h', not claude"
	run_messages "$_r4_run" 0 | jq -r 'select(.role == "assistant") | .content | tostring' | grep -q "$RULE_MARKER" ||
		fail "no reply holds $RULE_MARKER, the marker that CLAUDE.md of the test repository asks for: the CLI did not read the rule file"
	evidence "the claude harness answered PONG $_r4_tok and added $RULE_MARKER from the rule file CLAUDE.md of the repository"
	pass
}

# ---------- N1 ----------

# server_image  The image of the server Deployment of the stack (chart mode).
server_image() {
	kc -n of-local get deploy openfactory -o jsonpath='{.spec.template.spec.containers[?(@.name=="server")].image}' 2>/dev/null
}

check_N1() {
	need_cmd go kubectl jq openssl
	# Part 1: the network tests of make e2e-k8s (TestK8sNet*) on the cluster of
	# the stack. The proxy image is the server image (openfactory and iptables).
	# The test Pods pull curl and nginx, so the pull policy is IfNotPresent.
	_n1_proxy_image=$(server_image)
	[ -n "$_n1_proxy_image" ] || fail "cannot find the image of the server Deployment openfactory in of-local"
	E2E_PROXY_IMAGE=$_n1_proxy_image E2E_PULL_POLICY=IfNotPresent E2E_TIMEOUT=18m \
		e2e_k8s_test '^TestK8sNet' || fail "go test -tags e2e_k8s -run ^TestK8sNet failed"
	for _n1_t in TestK8sNetEnforcementProbe TestK8sNetEgressProxy TestK8sNetEgressLock TestK8sNetPolicyBlocksDirectEgress TestK8sNetPolicyWaitTimeout; do
		grep -q -- "--- PASS: $_n1_t " "$WORK/e2e.out" || grep -q -- "--- SKIP: $_n1_t " "$WORK/e2e.out" ||
			fail "$_n1_t did not run"
	done
	if grep -q -- '--- SKIP: TestK8sNetPolicyBlocksDirectEgress ' "$WORK/e2e.out"; then
		case $KUBE_CONTEXT in
		kind-*) fail "TestK8sNetPolicyBlocksDirectEgress was skipped: the kind cluster does not enforce NetworkPolicy (is Calico running?)" ;;
		esac
		evidence "TestK8sNet*: pass; the cluster does not enforce NetworkPolicy, so TestK8sNetPolicyBlocksDirectEgress skipped"
	else
		evidence "TestK8sNet*: pass, with NetworkPolicy enforcement"
	fi
	# Part 2: a restricted run through the server of the stack.
	fx_factory "$HERE/factories/net" of-local-net
	_n1_tok=$(rand_token)
	_n1_cmd="echo PROXY=\${HTTP_PROXY:-none}; curl -sS -m 20 -o /tmp/n1.out http://blocked.of-local.test/; echo CURL-EXIT=\$?; cat /tmp/n1.out; echo N1-END-$_n1_tok"
	_n1_prompt="N1 $_n1_tok. $(tool_directive bash "$(jq -nc --arg c "$_n1_cmd" '{command: $c}')")"
	run_dispatch "$FACTORY_UID" "$_n1_prompt" "" '{"idle_timeout_minutes":0}'
	_n1_run=$RUN_ID
	cancel_later "$_n1_run"
	# While the task starts, look at its init container egress-wait.
	_n1_wait=
	_n1_t0=$(now)
	while [ $(($(now) - _n1_t0)) -lt 90 ]; do
		_n1_wait=$(kc -n "$TASK_NAMESPACE" get pods -o json 2>/dev/null | jq -r --arg id "$_n1_run." '.items[]? |
			select((.metadata.annotations["openfactory.dev/sandbox-id"] // "") | startswith($id)) |
			.status.initContainerStatuses[]? | select(.name == "egress-wait") | .state.terminated.exitCode // empty' | head -n 1)
		[ -z "$_n1_wait" ] || break
		run_get "$_n1_run"
		case $RUN_STATE in SUCCEEDED | FAILED | ERROR | CANCELLED) break ;; esac
		sleep 1
	done
	if [ -n "$_n1_wait" ]; then
		[ "$_n1_wait" = 0 ] || fail "the init container egress-wait ended with exit code $_n1_wait (75 is network_policy_timeout)"
		evidence "the init container egress-wait ended with exit code 0 (the NetworkPolicy was active before the task code)"
	else
		log "the task Pod was not seen while it started (it can be fast); the run state decides"
	fi
	expect_reply "$_n1_run" 0 tool_result "N1-END-$_n1_tok" 300 "the command in the restricted task"
	_n1_proxy=$(run_messages "$_n1_run" 0 | jq -r 'select(.role == "tool_result") | .content | tostring' | grep -a '^PROXY=' | head -n 1)
	log "proxy variable in the task: ${_n1_proxy:-none seen}"
	case $_n1_proxy in
	"" | PROXY=none) fail "the restricted task has no HTTP_PROXY: the egress proxy is not set up (the server needs OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE)" ;;
	esac
	expect_reply "$_n1_run" 0 tool_result "network policy blocked access to blocked\\.of-local\\.test" 30 "the blocked call"
	expect_reply "$_n1_run" 0 assistant 'DONE' 60 "the turn"
	run_get "$_n1_run"
	case $(printf '%s' "$RUN_JSON" | jq -r '.failure_code // empty') in
	"") ;;
	*) fail "the restricted run has the failure code $(printf '%s' "$RUN_JSON" | jq -r '.failure_code')" ;;
	esac
	evidence "the restricted run started and the proxy answered for the blocked host: Open Factory network policy blocked access"
	# The blocked host is a diagnostic of the run.
	_n1_diag=
	_n1_t0=$(now)
	while [ $(($(now) - _n1_t0)) -lt 90 ]; do
		api_call GET "/runs/$_n1_run/diagnostics"
		_n1_diag=$(printf '%s' "$API_BODY" | jq -r '.items[]? | select(.code == "network/egress_blocked" and (.data.blocked_host // "") == "blocked.of-local.test") | "\(.code) host=\(.data.blocked_host) count=\(.data.count)"' | head -n 1)
		[ -z "$_n1_diag" ] || break
		sleep 3
	done
	[ -n "$_n1_diag" ] || {
		printf '%s' "$API_BODY" | jq -c '.items[]? | {code, message}' | head -n 10
		fail "the diagnostics of the run have no network/egress_blocked entry for blocked.of-local.test (does the stack set OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE?)"
	}
	evidence "diagnostic: $_n1_diag"
	pass
}

# ---------- N2 and N3 ----------

# e2e_k8s_test TEST_REGEX  Run the e2e_k8s Go tests of make e2e-k8s on the
# cluster of the stack. E2E_PROXY_IMAGE, E2E_PULL_POLICY (default Never),
# E2E_EGRESS_WAIT_ADDR (target of the egress wait, default the kubelet) and
# E2E_TIMEOUT (default 12m) change the settings of the tests.
e2e_k8s_test() {
	need_cmd go kubectl
	_e2_ri=$(runner_image)
	_e2_ca=$WORK/kube-ca.crt
	kc config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' 2>/dev/null | openssl base64 -d -A >"$_e2_ca" 2>/dev/null
	[ -s "$_e2_ca" ] || {
		rm -f "$_e2_ca"
		_e2_ca=
	}
	(
		cd "$REPO" || exit 1
		OF_E2E_KUBE_CONTEXT=$KUBE_CONTEXT OF_E2E_RUNNER_IMAGE=$_e2_ri \
			OF_E2E_PULL_POLICY=${E2E_PULL_POLICY:-Never} OF_E2E_PROXY_IMAGE=${E2E_PROXY_IMAGE:-} \
			OF_E2E_EGRESS_WAIT_ADDR=${E2E_EGRESS_WAIT_ADDR:-} \
			OF_E2E_CA_FILE=${_e2_ca:-} \
			go test -tags e2e_k8s -run "$1" -count=1 -v -timeout "${E2E_TIMEOUT:-12m}" ./internal/sandbox/k8stask
	) >"$WORK/e2e.out" 2>&1
	_e2_rc=$?
	tail -n 60 "$WORK/e2e.out"
	return "$_e2_rc"
}

check_N2() {
	need_cmd go kubectl openssl
	[ -n "$(runner_image)" ] || fail "cannot find the runner image of the stack (OF_RUNNER_IMAGE)"
	e2e_k8s_test '^TestK8sDeadline$' || fail "go test -tags e2e_k8s -run TestK8sDeadline failed"
	grep -q -- '--- PASS: TestK8sDeadline' "$WORK/e2e.out" || fail "TestK8sDeadline did not pass (it was skipped?)"
	grep -q 'failure code of the run: ' "$WORK/e2e.out" || fail "the test did not report the failure code"
	evidence "TestK8sDeadline: $(grep 'failure code of the run: ' "$WORK/e2e.out" | head -n 1 | sed 's/^ *[a-z0-9_.]*:[0-9]*: *//')"
	pass
}

check_N3() {
	need_cmd go kubectl openssl
	e2e_k8s_test '^TestK8sRoleWithoutJobs$' || fail "go test -tags e2e_k8s -run TestK8sRoleWithoutJobs failed"
	grep -q -- '--- PASS: TestK8sRoleWithoutJobs$' "$WORK/e2e.out" || grep -q -- '--- PASS: TestK8sRoleWithoutJobs ' "$WORK/e2e.out" || fail "TestK8sRoleWithoutJobs did not pass"
	grep -q -- '--- PASS: TestK8sRoleWithoutJobs/Preflight' "$WORK/e2e.out" || fail "the subtest Preflight did not pass"
	if grep -q -- '--- SKIP: TestK8sRoleWithoutJobs/ServerStartup' "$WORK/e2e.out"; then
		fail "the subtest ServerStartup was skipped (no CA file for the cluster)"
	fi
	grep -q -- '--- PASS: TestK8sRoleWithoutJobs/ServerStartup' "$WORK/e2e.out" || fail "the subtest ServerStartup did not pass"
	evidence "a Role without jobs: the preflight and the server startup stop with an error that names the jobs rule"
	pass
}
