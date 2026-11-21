# shellcheck shell=sh
# Checks P1 to P3, D1 and UI1. Source this file.

# ---------- P1 ----------

check_P1() {
	need_cmd curl jq
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		need_cmd kubectl
		kc -n of-local get deploy openfactory -o json >"$WORK/p1-deploy.json" || fail "kubectl cannot read deploy/openfactory in of-local"
		_p1_want=$(jq -r '.spec.replicas // 0' "$WORK/p1-deploy.json")
		_p1_ready=$(jq -r '.status.readyReplicas // 0' "$WORK/p1-deploy.json")
		log "deploy/openfactory: $_p1_ready of $_p1_want replicas ready"
		[ "$_p1_want" -ge 2 ] || fail "the Deployment has $_p1_want replica(s); the stack needs 2"
		[ "$_p1_ready" -eq "$_p1_want" ] || fail "$_p1_ready of $_p1_want server replicas are ready"
		kc -n of-local get pods -l app.kubernetes.io/instance=openfactory,app.kubernetes.io/component=server -o json |
			jq -r '.items[] | "\(.metadata.name) phase=\(.status.phase) containers_ready=\([.status.containerStatuses[]?.ready] | all)"' | tee "$WORK/p1-pods.txt"
		[ "$(grep -c 'phase=Running containers_ready=true' "$WORK/p1-pods.txt")" -ge 2 ] || fail "fewer than 2 server Pods run with all containers ready"
		if [ "$LOCAL_TARGET" = eks ]; then
			evidence "2 server Pods are Running and ready (EKS: the Pod Identity agent is a node add-on, not a sidecar)"
			pass
		fi
		_p1_side=$(kc -n of-local get pods -l app.kubernetes.io/instance=openfactory,app.kubernetes.io/component=server -o json |
			jq -r '[.items[] | select([.spec.containers[].name] | index("pod-identity-agent"))] | length')
		[ "$_p1_side" -ge 2 ] || fail "only $_p1_side server Pod(s) have the container pod-identity-agent"
		evidence "2 server Pods are Running and ready, each with the pod-identity-agent sidecar"
	else
		for _p1_port in 18081 18082; do
			_p1_code=$(http_code "http://127.0.0.1:$_p1_port/readyz")
			log "http://127.0.0.1:$_p1_port/readyz -> $_p1_code"
			[ "$_p1_code" = 200 ] || fail "the host server on port $_p1_port answers $_p1_code on /readyz"
		done
		evidence "both host servers (18081 and 18082) answer 200 on /readyz"
	fi
	_p1_code=$(http_code "${OF_SERVER%/}/readyz")
	[ "$_p1_code" = 200 ] || fail "$OF_SERVER/readyz answers $_p1_code"
	evidence "$OF_SERVER/readyz answers 200"
	pass
}

# ---------- P2 ----------

check_P2() {
	need_cmd kubectl jq
	need_file "$LOCAL_DIR/db.password"
	_p2_want=$(for _p2_f in "$REPO"/internal/store/postgres/migrations/*.sql; do
		[ -e "$_p2_f" ] && basename "$_p2_f" .sql
	done | sort)
	[ -n "$_p2_want" ] || fail "the repository has no PostgreSQL migrations"
	_p2_got=$(psql_query "SELECT lpad(version::text, 4, '0') || '_' || name FROM schema_migrations ORDER BY version") ||
		fail "cannot query schema_migrations in PostgreSQL through kubectl exec into the database Pod"
	log "migrations in the database:"
	printf '%s\n' "$_p2_got"
	[ "$_p2_got" = "$_p2_want" ] || {
		log "migrations in the repository:"
		printf '%s\n' "$_p2_want"
		fail "schema_migrations differs from internal/store/postgres/migrations ($(printf '%s\n' "$_p2_got" | grep -c .) rows, $(printf '%s\n' "$_p2_want" | grep -c .) files)"
	}
	_p2_dup=$(psql_query "SELECT count(*) - count(DISTINCT version) FROM schema_migrations")
	[ "$_p2_dup" = 0 ] || fail "schema_migrations has duplicate versions"
	# No replica reports a failed migration.
	server_logs >"$WORK/p2-logs.txt" 2>&1
	if grep -i 'migration' "$WORK/p2-logs.txt" | grep -i -E 'error|fail' | head -n 3 | grep .; then
		fail "a server log reports a migration problem"
	fi
	_p2_starts=$(grep -c 'server started' "$WORK/p2-logs.txt")
	evidence "$(printf '%s\n' "$_p2_got" | grep -c .) migrations, each version once, equal to the files of the repository. Server starts in the logs: $_p2_starts"
	pass
}

# ---------- P3 ----------

check_P3() {
	need_cmd kubectl jq
	need_file "$LOCAL_DIR/db.password"
	server_logs >"$WORK/p3-logs.txt" 2>&1
	[ -s "$WORK/p3-logs.txt" ] || fail "no server logs to read"
	# Each replica: the server started (a failed preflight stops the start), the
	# NetworkPolicy probe found enforcement, and nothing says otherwise.
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		_p3_names=$(server_pods)
	else
		_p3_names=$(sed -n 's/^\[\([^]]*\)\].*/\1/p' "$WORK/p3-logs.txt" | sort -u)
	fi
	[ -n "$_p3_names" ] || fail "no server replica found"
	for _p3_r in $_p3_names; do
		# Only the current process: a host log is appended at each start, so keep
		# the lines from the last 'server started' on.
		grep -F "[$_p3_r]" "$WORK/p3-logs.txt" |
			awk '/server started/ { n = 0; delete buf } { buf[++n] = $0 } END { for (i = 1; i <= n; i++) print buf[i] }' >"$WORK/p3-one.txt"
		grep -q 'server started' "$WORK/p3-one.txt" || fail "the log of $_p3_r has no 'server started' (the startup preflight did not finish)"
		grep -q 'the cluster enforces NetworkPolicy' "$WORK/p3-one.txt" || fail "the log of $_p3_r has no 'the cluster enforces NetworkPolicy' (the NetworkPolicy probe did not pass)"
		if grep -q -e 'the cluster does NOT enforce NetworkPolicy' -e 'the backend cannot manage NetworkPolicies' -e 'sandbox backend preflight' "$WORK/p3-one.txt"; then
			grep -e 'does NOT enforce' -e 'cannot manage NetworkPolicies' -e 'sandbox backend preflight' "$WORK/p3-one.txt" | head -n 3
			fail "the log of $_p3_r reports a failed preflight or probe"
		fi
		evidence "$_p3_r: server started, and the cluster enforces NetworkPolicy (probe)"
		grep -q 'preflight job completed' "$WORK/p3-one.txt" && evidence "$_p3_r: the preflight Job completed"
	done
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		_p3_can=$(kc -n "$TASK_NAMESPACE" auth can-i create networkpolicies --as=system:serviceaccount:of-local:openfactory 2>&1)
		[ "$_p3_can" = yes ] || fail "the server service account cannot create networkpolicies in $TASK_NAMESPACE ($_p3_can)"
		_p3_can=$(kc -n "$TASK_NAMESPACE" auth can-i create jobs.batch --as=system:serviceaccount:of-local:openfactory 2>&1)
		[ "$_p3_can" = yes ] || fail "the server service account cannot create jobs in $TASK_NAMESPACE ($_p3_can)"
		evidence "the server service account can create jobs and networkpolicies in $TASK_NAMESPACE"
	fi
	# The leader lease is one row, held by a replica of the stack, not expired.
	_p3_rows=$(psql_query "SELECT holder || ' ' || round(expires_ns / 1e9 - extract(epoch from now()))::int FROM leader_leases WHERE name = 'control-plane'") ||
		fail "cannot read leader_leases"
	[ "$(printf '%s\n' "$_p3_rows" | grep -c .)" -eq 1 ] || fail "leader_leases has $(printf '%s\n' "$_p3_rows" | grep -c .) row(s) for control-plane, not 1"
	_p3_holder=${_p3_rows%% *}
	_p3_left=${_p3_rows##* }
	_p3_replica=${_p3_holder%%/*}
	log "lease control-plane: holder replica $_p3_replica, expires in $_p3_left s"
	[ "$_p3_left" -ge 0 ] 2>/dev/null || fail "the lease of $_p3_replica expired $_p3_left seconds ago"
	if [ "$LOCAL_SERVER_MODE" = chart ]; then
		printf '%s\n' "$_p3_names" | grep -qx "$_p3_replica" || fail "the lease holder $_p3_replica is not one of the server Pods: $(printf '%s' "$_p3_names" | tr '\n' ' ')"
	else
		grep -F -q -e "replica=$_p3_replica" -e "\"replica\":\"$_p3_replica\"" "$WORK/p3-logs.txt" ||
			fail "the lease holder $_p3_replica is not the replica of any host server log ('server started' line)"
	fi
	evidence "the leader lease of control-plane is held by $_p3_replica and is not expired"
	pass
}

# ---------- D1 ----------

# d1_active  Print the id and the content of the marker line of the active definition.
d1_active() {
	api_call GET "/factories/$1/definition"
	printf '%s' "$API_BODY" | jq -r '"\(.version.id // "-") \((.files["agents/foreman/agent.md"] // "") | capture("D1-MARKER: (?<m>[a-z]+)").m // "none")"'
}

check_D1() {
	ensure_cli
	_d1_dir=$WORK/d1
	rm -rf "$_d1_dir"
	mkdir -p "$_d1_dir"
	render_tree "$HERE/factories/def" "$_d1_dir/v1"
	render_tree "$HERE/factories/def" "$_d1_dir/v2"
	sed 's/D1-MARKER: one/D1-MARKER: two/' "$_d1_dir/v2/agents/foreman/agent.md" >"$_d1_dir/v2/agent.tmp" && mv "$_d1_dir/v2/agent.tmp" "$_d1_dir/v2/agents/foreman/agent.md"
	render_tree "$_d1_dir/v2" "$_d1_dir/bad"
	printf 'noSuchKey: true\n' >>"$_d1_dir/bad/factory.yaml"
	# Local validation, as the CLI does it.
	"$OFBIN" validate "$_d1_dir/v1" >/dev/null 2>&1 || fail "openfactory validate refuses the tree v1"
	"$OFBIN" validate "$_d1_dir/v2" >/dev/null 2>&1 || fail "openfactory validate refuses the tree v2"
	if "$OFBIN" validate "$_d1_dir/bad" >/dev/null 2>&1; then
		fail "openfactory validate accepts the invalid tree"
	fi
	_d1_uid=$(factory_uid of-local-def)
	# 1. Apply v1 (the factory is created on the first run).
	if [ -n "$_d1_uid" ]; then
		_d1_now=$(d1_active "$_d1_uid")
		log "active before: $_d1_now"
		case $_d1_now in
		*" one") log "the active version already has the content of v1" ;;
		*) run_logged "$OFBIN" factory apply "$_d1_dir/v1" --factory "$_d1_uid" -m "D1 v1" || fail "apply of v1 failed" ;;
		esac
	else
		run_logged "$OFBIN" factory apply "$_d1_dir/v1" -m "D1 v1" || fail "creating the factory from v1 failed"
		_d1_uid=$(factory_uid of-local-def)
	fi
	[ -n "$_d1_uid" ] || fail "the factory of-local-def does not exist after the apply"
	_d1_a=$(d1_active "$_d1_uid")
	_d1_v1=${_d1_a% *}
	[ "${_d1_a#* }" = one ] || fail "after apply v1 the active definition has the marker '${_d1_a#* }', not 'one'"
	evidence "v1 is active: version $_d1_v1"
	# 2. Apply v2: a new version.
	run_logged "$OFBIN" factory apply "$_d1_dir/v2" --factory "$_d1_uid" -m "D1 v2" || fail "apply of v2 failed"
	_d1_a=$(d1_active "$_d1_uid")
	_d1_v2=${_d1_a% *}
	[ "${_d1_a#* }" = two ] || fail "after apply v2 the active definition has the marker '${_d1_a#* }', not 'two'"
	[ "$_d1_v2" != "$_d1_v1" ] || fail "apply v2 did not make a new version (still $_d1_v1)"
	evidence "v2 is a new active version: $_d1_v2 (v1 was $_d1_v1)"
	api_ok GET "/factories/$_d1_uid/definition/versions?limit=200"
	_d1_count=$(printf '%s' "$API_BODY" | jq '.items | length')
	printf '%s' "$API_BODY" | jq -e --arg a "$_d1_v2" '[.items[] | select(.id == $a and .state == "active")] | length == 1' >/dev/null || fail "the version list does not show $_d1_v2 as active"
	# 3. An invalid change is refused, and the active version stays.
	_d1_body=$(files_json "$_d1_dir/bad" | jq -c --arg b "$_d1_v2" '{files: ., message: "D1 invalid change", base_version_id: $b}')
	api_call PUT "/factories/$_d1_uid/definition" "$_d1_body"
	case $API_CODE in
	4??) log "the API refused the invalid change with $API_CODE: $(printf '%s' "$API_BODY" | cut -c1-200)" ;;
	*) fail "the API answered $API_CODE to an invalid definition, not a 4xx: $(printf '%s' "$API_BODY" | cut -c1-200)" ;;
	esac
	printf '%s' "$API_BODY" | grep -q -i -E 'noSuchKey|unknown|error' || fail "the refusal does not name the error: $(printf '%s' "$API_BODY" | cut -c1-200)"
	if "$OFBIN" factory apply "$_d1_dir/bad" --factory "$_d1_uid" >/dev/null 2>&1; then
		fail "openfactory factory apply accepted the invalid tree"
	fi
	_d1_a=$(d1_active "$_d1_uid")
	[ "${_d1_a% *}" = "$_d1_v2" ] || fail "the invalid change replaced the active version ($_d1_a)"
	api_ok GET "/factories/$_d1_uid/definition/versions?limit=200"
	[ "$(printf '%s' "$API_BODY" | jq '[.items[] | select(.state == "active")] | length')" -eq 1 ] || fail "after the invalid change there is not exactly one active version"
	evidence "the invalid change was refused ($API_CODE earlier) and version $_d1_v2 stays active"
	# 4. Restore v1.
	api_ok POST "/factories/$_d1_uid/definition/versions/$_d1_v1/restore" '{}'
	_d1_a=$(d1_active "$_d1_uid")
	_d1_v3=${_d1_a% *}
	[ "${_d1_a#* }" = one ] || fail "after the restore the active definition has the marker '${_d1_a#* }', not 'one'"
	{ [ "$_d1_v3" != "$_d1_v1" ] && [ "$_d1_v3" != "$_d1_v2" ]; } || fail "the restore did not make a new version (active $_d1_v3)"
	api_ok GET "/factories/$_d1_uid/definition/versions?limit=200"
	[ "$(printf '%s' "$API_BODY" | jq '.items | length')" -gt "$_d1_count" ] || fail "the version list did not grow after the restore"
	evidence "restore of $_d1_v1 made the new active version $_d1_v3 with the content of v1"
	pass
}

# ---------- UI1 ----------

check_UI1() {
	need_cmd curl jq
	_ui_base=${OF_SERVER%/}
	_ui_jar=$WORK/ui.jar
	# 1. The app shell: the SPA index on the routes of the dashboard, and its assets.
	for _ui_route in /login / /factories /runs /inbox /environments /workers; do
		_ui_code=$(curl -sS -m 20 -o "$WORK/ui-index.html" -w '%{http_code} %{content_type}' "$_ui_base$_ui_route" 2>>"$CHECK_LOG")
		log "GET $_ui_route -> $_ui_code"
		case $_ui_code in
		"200 text/html"*) ;;
		*) fail "GET $_ui_route answers '$_ui_code', not 200 text/html" ;;
		esac
		grep -q 'id="root"' "$WORK/ui-index.html" || fail "GET $_ui_route is not the app shell (no div#root)"
	done
	_ui_assets=$(grep -oE '(src|href)="/assets/[^"]*"' "$WORK/ui-index.html" | sed -E 's/^[a-z]+="//; s/"$//' | sort -u)
	[ -n "$_ui_assets" ] || fail "the app shell references no /assets/ file (is the dashboard built into the server image?)"
	for _ui_a in $_ui_assets; do
		_ui_code=$(curl -sS -m 20 -o /dev/null -w '%{http_code} %{content_type}' "$_ui_base$_ui_a" 2>>"$CHECK_LOG")
		log "GET $_ui_a -> $_ui_code"
		case $_ui_code in
		"200 "*javascript* | "200 "*css*) ;;
		*) fail "the asset $_ui_a answers '$_ui_code'" ;;
		esac
	done
	evidence "the app shell loads on 7 routes and its $(printf '%s\n' "$_ui_assets" | grep -c .) asset(s) load"
	# 2. Sign in as the dashboard does (API key session), then read what the pages read.
	_ui_login=$(printf '{"api_key":"%s"}' "$OF_API_KEY" | curl -sS -m 20 -c "$_ui_jar" -H 'Content-Type: application/json' -H 'X-OF-CSRF: 1' \
		--data-binary @- -o /dev/null -w '%{http_code}' "$_ui_base/auth/session" 2>>"$CHECK_LOG")
	[ "$_ui_login" = 200 ] || fail "POST /auth/session answers $_ui_login, not 200"
	grep -q of_session "$_ui_jar" || fail "POST /auth/session set no of_session cookie"
	evidence "the API key session login answers 200 and sets the of_session cookie"
	_ui_get() {
		curl -sS -m 30 -b "$_ui_jar" -o "$WORK/ui-api.json" -w '%{http_code}' "$_ui_base/api/v1$1" 2>>"$CHECK_LOG"
	}
	[ "$(_ui_get /me)" = 200 ] || fail "GET /api/v1/me with the session cookie does not answer 200"
	# The factory and the runs of the checks.
	fx_artifact_run
	[ "$(_ui_get "/factories?search=$CHECK_FACTORY_ALIAS")" = 200 ] || fail "GET /api/v1/factories does not answer 200"
	_ui_uid=$(jq -r --arg a "$CHECK_FACTORY_ALIAS" '[.items[] | select(.alias == $a)][0] | (.uid // .id) // empty' "$WORK/ui-api.json")
	[ -n "$_ui_uid" ] || fail "the factory list has no factory $CHECK_FACTORY_ALIAS"
	for _ui_p in "/factories/$_ui_uid" "/factories/$_ui_uid/agents" "/factories/$_ui_uid/work-items" "/factories/$_ui_uid/metrics" \
		"/factories/$_ui_uid/definition" "/factories/$_ui_uid/definition/versions" "/inbox" "/environments" "/workers" "/version"; do
		_ui_code=$(_ui_get "$_ui_p")
		log "GET /api/v1$_ui_p -> $_ui_code"
		case $_ui_code in 2??) ;; *) fail "GET /api/v1$_ui_p answers $_ui_code (a dashboard page reads it)" ;; esac
	done
	[ "$(_ui_get "/runs?factory_id=$_ui_uid&limit=100")" = 200 ] || fail "GET /api/v1/runs does not answer 200"
	_ui_mine=$(sort -u "$STATE_DIR/runs" 2>/dev/null)
	_ui_found=0
	_ui_total=0
	for _ui_r in $_ui_mine; do
		_ui_total=$((_ui_total + 1))
		if jq -e --arg id "$_ui_r" '[.items[] | select(.run_id == $id or .id == $id)] | length > 0' "$WORK/ui-api.json" >/dev/null; then
			_ui_found=$((_ui_found + 1))
		fi
	done
	log "runs of the checks in the run list: $_ui_found of $_ui_total (the list shows the newest 100)"
	[ "$_ui_total" -gt 0 ] && [ "$_ui_found" -gt 0 ] || fail "the run list of the dashboard shows none of the $_ui_total run(s) that the checks started"
	_ui_run=$ARTIFACT_RUN
	for _ui_p in "/runs/$_ui_run" "/runs/$_ui_run/messages" "/runs/$_ui_run/timeline" "/runs/$_ui_run/artifacts"; do
		_ui_code=$(_ui_get "$_ui_p")
		log "GET /api/v1$_ui_p -> $_ui_code"
		case $_ui_code in 2??) ;; *) fail "GET /api/v1$_ui_p answers $_ui_code (the run page reads it)" ;; esac
	done
	_ui_code=$(curl -sS -m 20 -o /dev/null -w '%{http_code}' "$_ui_base/runs/$_ui_run" 2>>"$CHECK_LOG")
	[ "$_ui_code" = 200 ] || fail "GET /runs/$_ui_run answers $_ui_code"
	evidence "the pages read $_ui_found of $_ui_total run(s) of the checks, and the run page of $_ui_run loads"
	pass
}
