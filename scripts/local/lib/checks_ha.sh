# shellcheck shell=sh
# Checks H1 and H2. Source this file.

# leader_row  Print "<replica> <seconds left>" of the control-plane lease.
leader_row() {
	_lr=$(psql_query "SELECT split_part(holder, '/', 1) || ' ' || round(expires_ns / 1e9 - extract(epoch from now()))::int FROM leader_leases WHERE name = 'control-plane'" 2>/dev/null) || return 1
	[ -n "$_lr" ] || return 1
	printf '%s\n' "$_lr"
}

# h_long_turn PROMPT_WORD SECONDS  Start a run that runs a shell command for a while. It sets RUN_ID and H_TOK.
h_long_turn() {
	fx_check_factory
	H_TOK=$(rand_token)
	_hl_cmd="sleep $2; echo $1-DONE-$H_TOK"
	_hl_prompt="$1 $H_TOK. $(tool_directive bash "$(jq -nc --arg c "$_hl_cmd" '{command: $c, timeout_seconds: 300}')")"
	run_dispatch "$FACTORY_UID" "$_hl_prompt" "" ""
	# The turn is in progress when the first tool call is stored.
	expect_reply "$RUN_ID" 0 tool_call '.*' 240 "the turn starts its tool call"
}

# host_pid_of_replica REPLICA  The process id of the host server with this replica id.
host_pid_of_replica() {
	for _hp_port in 18081 18082; do
		_hp_pid=$(lsof -nP -iTCP:"$_hp_port" -sTCP:LISTEN -t 2>/dev/null | head -n 1)
		[ -n "$_hp_pid" ] || continue
		_hp_info=$(ps eww -p "$_hp_pid" 2>/dev/null)
		case $_hp_info in
		*"OF_REPLICA_ID=$1 "* | *"OF_REPLICA_ID=$1" | *"--replica-id $1"* | *"--replica-id=$1"*)
			printf '%s\n' "$_hp_pid"
			return 0
			;;
		esac
	done
	return 1
}

# h_run_alive RUN_ID  Return 0 when the run did not fail, its sandbox runs,
# and (chart mode) a task Pod of the run is Running. It sets H_ALIVE_WHY.
h_run_alive() {
	run_get "$1"
	case $RUN_STATE in
	"")
		H_ALIVE_WHY="cannot read the run $1"
		return 1
		;;
	FAILED | ERROR | CANCELLED)
		H_ALIVE_WHY="the run is $RUN_STATE ($(run_why "$1"))"
		return 1
		;;
	esac
	if [ "$(printf '%s' "$RUN_JSON" | jq -r '.sandbox_running // false')" != true ]; then
		H_ALIVE_WHY="the run has no running sandbox (state $RUN_STATE): the runner stopped"
		return 1
	fi
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		_ra_pods=$(task_pods "$1")
		if ! printf '%s\n' "$_ra_pods" | awk '$3 == "Running" { found = 1 } END { exit !found }'; then
			H_ALIVE_WHY="no task Pod of the run is Running: $(printf '%s' "$_ra_pods" | tr '\n' ';')"
			return 1
		fi
	fi
	return 0
}

# h_hold_alive RUN_ID SECONDS  Fail the check when h_run_alive is false at any
# time in the next SECONDS. A runner that dies after the turn shows here.
h_hold_alive() {
	_ha_t0=$(now)
	while :; do
		h_run_alive "$1" || fail "after the turn: $H_ALIVE_WHY"
		[ $(($(now) - _ha_t0)) -lt "$2" ] || return 0
		sleep 3
	done
}

h1_takeover() {
	_h1_row=$(leader_row) || return 1
	[ "${_h1_row%% *}" != "$H1_OLD" ] && [ "${_h1_row##* }" -ge 0 ] 2>/dev/null
}

check_H1() {
	need_cmd jq curl kubectl
	need_file "$LOCAL_DIR/db.password"
	_h1_row=$(leader_row) || fail "cannot read the leader lease in PostgreSQL"
	H1_OLD=${_h1_row%% *}
	log "leader before: $H1_OLD (lease valid for ${_h1_row##* } s)"
	h_long_turn H1 45
	_h1_run=$RUN_ID
	_h1_row=$(leader_row) || fail "cannot read the leader lease in PostgreSQL"
	H1_OLD=${_h1_row%% *}
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		server_pods | grep -qx "$H1_OLD" || fail "the lease holder $H1_OLD is not a server Pod"
		log "delete the leader Pod $H1_OLD without a grace period, during the turn"
		kc -n of-local delete pod "$H1_OLD" --grace-period=0 --force --wait=false || fail "cannot delete the Pod $H1_OLD"
	else
		need_cmd lsof ps
		_h1_pid=$(host_pid_of_replica "$H1_OLD") || not_implemented "cannot map the lease holder '$H1_OLD' to a host server process (port 18081 or 18082): the processes show no OF_REPLICA_ID or --replica-id with that value"
		log "kill the host server process $_h1_pid (replica $H1_OLD) during the turn"
		kill -KILL "$_h1_pid" || fail "cannot kill the process $_h1_pid"
		log "the killed host server is not started again by this check. Run make local-up to start it."
	fi
	evidence "the leader $H1_OLD stopped during the turn of run $_h1_run"
	# The turn finishes, with the result of the command.
	expect_reply "$_h1_run" 0 tool_result "H1-DONE-$H_TOK" 300 "the long command ends"
	expect_reply "$_h1_run" 0 assistant 'DONE' 120 "the turn ends"
	evidence "the turn finished after the leader stopped (a replica holds no turn)"
	# The runner lives on after the turn: the session waits for the next message.
	h_hold_alive "$_h1_run" 30
	evidence "30 s after the turn, run $_h1_run did not fail and its sandbox still runs"
	# The other replica takes the lease.
	wait_until 120 3 h1_takeover || fail "the other replica did not take the leader lease in 120 seconds (still $(leader_row 2>/dev/null))"
	_h1_row=$(leader_row)
	evidence "replica ${_h1_row%% *} holds the lease now (was $H1_OLD), valid for ${_h1_row##* } s"
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		# shellcheck disable=SC2329 # invoked by wait_until on the next line
		h1_ready() { [ "$(kc -n of-local get deploy openfactory -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "$(kc -n of-local get deploy openfactory -o jsonpath='{.spec.replicas}' 2>/dev/null)" ]; }
		wait_until 240 4 h1_ready || fail "the Deployment did not return to its full replica count in 240 seconds"
		evidence "the Deployment started a new Pod, and all replicas are ready again"
	fi
	h_run_alive "$_h1_run" || fail "at the end: $H_ALIVE_WHY"
	evidence "at the end, run $_h1_run did not fail and its sandbox still runs"
	pass
}

check_H2() {
	need_chart
	need_cmd kubectl jq curl
	h_long_turn H2 60
	_h2_run=$RUN_ID
	_h2_old=$(server_pods | sort | tr '\n' ' ')
	log "server Pods before: $_h2_old"
	kc -n of-local rollout restart deploy/openfactory || fail "kubectl rollout restart failed"
	_h2_t0=$(now)
	_h2_streak=0
	_h2_max=0
	_h2_done=
	while [ $(($(now) - _h2_t0)) -lt 360 ]; do
		_h2_code=$(http_code "${OF_SERVER%/}/readyz")
		if [ "$_h2_code" = 200 ]; then
			_h2_streak=0
		else
			_h2_streak=$((_h2_streak + 1))
			[ "$_h2_streak" -le "$_h2_max" ] || _h2_max=$_h2_streak
		fi
		if kc -n of-local rollout status deploy/openfactory --timeout=2s >/dev/null 2>&1; then
			_h2_done=1
			break
		fi
		sleep 1
	done
	[ -n "$_h2_done" ] || fail "the rollout did not finish in 360 seconds"
	log "rollout finished after $(($(now) - _h2_t0)) s. Longest run of failed /readyz probes: $_h2_max"
	_h2_new=$(server_pods | sort | tr '\n' ' ')
	log "server Pods after: $_h2_new"
	for _h2_p in $_h2_old; do
		printf '%s' "$_h2_new" | grep -q "$_h2_p" && fail "the Pod $_h2_p still exists after the rollout"
	done
	[ "$_h2_max" -le 8 ] || fail "$OF_SERVER/readyz failed $_h2_max times in a row during the rolling update (the chart should keep one replica ready)"
	evidence "every Pod was replaced by the rolling update; longest gap of /readyz: $_h2_max probe(s)"
	expect_reply "$_h2_run" 0 tool_result "H2-DONE-$H_TOK" 300 "the long command ends"
	expect_reply "$_h2_run" 0 assistant 'DONE' 120 "the turn ends"
	evidence "the turn of run $_h2_run finished although both server Pods were replaced during it"
	_h2_row=$(leader_row) || fail "cannot read the leader lease after the rollout"
	[ "${_h2_row##* }" -ge 0 ] || fail "the lease of ${_h2_row%% *} is expired after the rollout"
	evidence "replica ${_h2_row%% *} holds the lease after the rollout"
	pass
}
