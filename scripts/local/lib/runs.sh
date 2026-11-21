# shellcheck shell=sh
# Fixtures and run helpers of scripts/local/check.sh. Source this file.
# The test factories are in scripts/local/factories/. The setup is idempotent:
# it creates what is missing, and it applies a definition only when it differs.

GIT_NAMESPACE=of-local-infra
TASK_NAMESPACE=of-local-sandboxes
CHECK_REPO=of-local-check
CHECK_FACTORY_ALIAS=of-local-check
RULE_MARKER=OF-RULE-7421

# git_url_host / git_url_cluster  The two URLs of the same repository.
git_url_host() { printf 'git://git.%s:%s/%s.git\n' "$LOCAL_DOMAIN" "${LOCAL_GIT_PORT:-9418}" "$1"; }
git_url_cluster() { printf 'git://git.%s.svc:9418/%s.git\n' "$GIT_NAMESPACE" "$1"; }

# ---------- tools ----------

# ensure_cli  Build bin/openfactory when it is missing or older than the sources.
ensure_cli() {
	need_cmd curl jq
	OFBIN="$REPO/bin/openfactory"
	_ec_stale=
	if [ -x "$OFBIN" ]; then
		_ec_stale=$(find "$REPO/cmd" "$REPO/internal" "$REPO/pkg" -name '*.go' -newer "$OFBIN" 2>/dev/null | head -n 1)
	fi
	if [ ! -x "$OFBIN" ] || [ -n "$_ec_stale" ]; then
		need_cmd make go
		log "build bin/openfactory (make build)"
		(cd "$REPO" && make build) || fail "make build failed"
	fi
}

# ---------- git repository ----------

# fx_git_repo NAME  Make the bare repository on the git server and its first commit.
fx_git_repo() {
	_gr=$1
	need_cmd git kubectl
	kc -n "$GIT_NAMESPACE" exec deploy/git -c git -- sh -c \
		"test -d /srv/git/$_gr.git || git init -q --bare --shared=all /srv/git/$_gr.git; git -C /srv/git/$_gr.git symbolic-ref HEAD refs/heads/main; touch /srv/git/$_gr.git/git-daemon-export-ok; chmod -R a+rwX /srv/git/$_gr.git" ||
		fail "cannot make /srv/git/$_gr.git on the git server with kubectl exec deploy/git -c git"
	_gr_url=$(git_url_host "$_gr")
	_gr_dir=$WORK/repo-$_gr
	rm -rf "$_gr_dir"
	# The first lookup of a host name can fail on OrbStack: ask three times before the repository counts as new.
	_gr_have=
	for _gr_try in 1 2 3; do
		if git_retry git ls-remote --exit-code "$_gr_url" refs/heads/main >/dev/null 2>&1; then
			_gr_have=1
			break
		fi
		sleep 2
	done
	if [ -n "$_gr_have" ]; then
		git clone -q --depth 1 "$_gr_url" "$_gr_dir" || fail "cannot clone $_gr_url"
	else
		if ! { mkdir -p "$_gr_dir" && git -C "$_gr_dir" init -q -b main && git -C "$_gr_dir" remote add origin "$_gr_url"; }; then
			fail "cannot make a work directory for $_gr_url"
		fi
	fi
	_gr_changed=
	if [ ! -f "$_gr_dir/README.md" ]; then
		printf '# %s\n\nTest repository of the local stack checks.\n' "$_gr" >"$_gr_dir/README.md"
		_gr_changed=1
	fi
	if ! grep -q "$RULE_MARKER" "$_gr_dir/CLAUDE.md" 2>/dev/null; then
		printf 'Rules of this repository.\n\nEnd every reply with the exact marker %s on its own line.\n' "$RULE_MARKER" >"$_gr_dir/CLAUDE.md"
		_gr_changed=1
	fi
	if [ -n "$_gr_changed" ]; then
		if ! { git -C "$_gr_dir" add -A &&
			git -C "$_gr_dir" -c user.name=of-local-check -c user.email=of-local-check@of-local.test commit -q -m "Test repository files" &&
			git_retry git -C "$_gr_dir" push -q origin HEAD:refs/heads/main; }; then
			fail "cannot push the first commit to $_gr_url"
		fi
		log "pushed the first commit of $_gr_url"
	fi
	git_retry git ls-remote --exit-code "$_gr_url" refs/heads/main >/dev/null 2>&1 || fail "the branch main is missing in $_gr_url after the setup"
}

# ---------- factories ----------

# render_tree SRC DST  Copy a factory tree and fill in the placeholders.
render_tree() {
	rm -rf "$2"
	mkdir -p "$2"
	cp -R "$1/." "$2/"
	_rt_repo=${LOCAL_GITHUB_REPO:-owner/repo}
	_rt_owner=${_rt_repo%%/*}
	_rt_name=${_rt_repo#*/}
	find "$2" -type f | while IFS= read -r _rt_f; do
		sed -e "s|__SANDBOX_IMAGE__|${SANDBOX_IMAGE:-ghcr.io/openfactory-org/sandbox:local}|g" \
			-e "s|__GIT_CLONE_URL__|${GIT_CLONE_URL:-git://git.example.test/repo.git}|g" \
			-e "s|__LOCAL_DOMAIN__|$LOCAL_DOMAIN|g" \
			-e "s|__GITHUB_OWNER__|${_rt_owner:-owner}|g" \
			-e "s|__GITHUB_NAME__|${_rt_name:-repo}|g" "$_rt_f" >"$_rt_f.tmp" && mv "$_rt_f.tmp" "$_rt_f"
	done
}

# files_json DIR  The files of a definition tree as a JSON object (path to content).
files_json() {
	_fj=$(mktemp "$WORK/files.XXXXXX")
	printf '{}' >"$_fj"
	(cd "$1" && find . -type f | sed 's|^\./||' | sort) | while IFS= read -r _fj_f; do
		jq --arg p "$_fj_f" --rawfile c "$1/$_fj_f" '. + {($p): $c}' "$_fj" >"$_fj.next" && mv "$_fj.next" "$_fj"
	done
	cat "$_fj"
}

# factory_uid ALIAS  Print the uid of a factory, or nothing.
factory_uid() {
	api_call GET "/factories?search=$(urlenc "$1")"
	printf '%s' "$API_BODY" | jq -r --arg a "$1" '[.items[]? | select(.alias == $a or .name == $a)][0] | (.uid // .id) // empty'
}

# fx_factory SRC_DIR ALIAS  Make or update a managed factory from a tree. It sets FACTORY_UID.
fx_factory() {
	_ff_src=$1
	_ff_alias=$2
	ensure_cli
	_ff_dir=$WORK/tree-$_ff_alias
	render_tree "$_ff_src" "$_ff_dir"
	FACTORY_UID=$(factory_uid "$_ff_alias")
	if [ -n "$FACTORY_UID" ]; then
		api_ok GET "/factories/$FACTORY_UID/definition"
		_ff_have=$(printf '%s' "$API_BODY" | jq -S '.files // {}')
		_ff_want=$(files_json "$_ff_dir" | jq -S .)
		if [ "$_ff_have" = "$_ff_want" ]; then
			log "factory $_ff_alias ($FACTORY_UID) is up to date"
			return 0
		fi
	fi
	log "apply the definition of $_ff_alias"
	if [ -n "$FACTORY_UID" ]; then
		run_logged "$OFBIN" factory apply "$_ff_dir" --factory "$FACTORY_UID" -m "local stack checks" ||
			fail "openfactory factory apply failed for $_ff_alias"
	else
		run_logged "$OFBIN" factory apply "$_ff_dir" -m "local stack checks" ||
			fail "openfactory factory apply failed for $_ff_alias"
	fi
	FACTORY_UID=$(factory_uid "$_ff_alias")
	[ -n "$FACTORY_UID" ] || fail "the factory $_ff_alias does not exist after the apply"
}

# fx_check_factory [git]  The main test factory. It sets FACTORY_UID.
# With the argument git it also makes sure that the test repository exists on
# the git server, for the checks that clone it (R1, R4). The agent coder clones
# it with the setup command of its runner (the setup commands of an
# environment are not applied by the runner plan, so the checks do not use one).
fx_check_factory() {
	[ "${1:-}" != git ] || fx_git_repo "$CHECK_REPO"
	SANDBOX_IMAGE=$(server_env_value OF_SANDBOX_IMAGE)
	if [ -z "$SANDBOX_IMAGE" ]; then
		SANDBOX_IMAGE=$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep '/sandbox:' | head -n 1)
	fi
	[ -n "$SANDBOX_IMAGE" ] || fail "cannot find the sandbox image of the stack (OF_SANDBOX_IMAGE)"
	GIT_CLONE_URL=$(git_url_cluster "$CHECK_REPO")
	fx_factory "$HERE/factories/check" "$CHECK_FACTORY_ALIAS"
}

# ---------- runs ----------

# run_dispatch FACTORY_UID PROMPT [AGENT] [OVERRIDES_JSON]  Start a run. It sets RUN_ID.
run_dispatch() {
	_rd_body=$(jq -nc --arg prompt "$2" --arg agent "${3:-}" --arg check "$CHECK_ID" --argjson ov "${4:-null}" \
		'{prompt: $prompt, title: ("local stack check " + $check), metadata: {check: $check}}
		 + (if $agent != "" then {agent: $agent} else {} end)
		 + (if $ov != null then {overrides: $ov} else {} end)')
	api_ok POST "/factories/$1/runs" "$_rd_body"
	RUN_ID=$(printf '%s' "$API_BODY" | jq -r '.run_id // .id // empty')
	[ -n "$RUN_ID" ] || fail "the dispatch answer has no run_id: $API_BODY"
	printf '%s\n' "$RUN_ID" >>"$STATE_DIR/runs"
	log "started run $RUN_ID (agent ${3:-foreman})"
}

# tool_directive NAME JSON  The text that makes the claude CLI stand-in call a tool.
tool_directive() { printf '[[tool: %s %s]]' "$1" "$2"; }

# run_get RUN_ID  Read a run. It sets RUN_JSON and RUN_STATE.
run_get() {
	api_call GET "/runs/$1"
	RUN_JSON=$API_BODY
	RUN_STATE=
	case $API_CODE in 2??) RUN_STATE=$(printf '%s' "$RUN_JSON" | jq -r '.state // empty') ;; esac
}

# run_why RUN_ID  A short reason for a failed run (state, failure code, message).
run_why() {
	run_get "$1"
	printf '%s' "$RUN_JSON" | jq -r '"state " + (.state // "?") + ", failure_code " + (.failure_code // "-") + ", " + ((.status_message.message // .error // "") | tostring)' 2>/dev/null
}

# last_seq RUN_ID  The highest message number of the run (0 when none).
last_seq() {
	api_call GET "/runs/$1/messages?limit=1000"
	printf '%s' "$API_BODY" | jq -r '[(.messages // [])[].seq] | max // 0'
}

# run_messages RUN_ID AFTER  Print the messages after a number as JSON lines.
run_messages() {
	_rm_after=${2:-0}
	while :; do
		api_call GET "/runs/$1/messages?limit=1000&after=$_rm_after"
		case $API_CODE in 2??) ;; *) return 1 ;; esac
		printf '%s' "$API_BODY" | jq -c '(.messages // [])[]'
		[ "$(printf '%s' "$API_BODY" | jq -r '.has_more // false')" = true ] || return 0
		_rm_after=$(printf '%s' "$API_BODY" | jq -r '.next_after')
	done
}

# wait_reply RUN_ID AFTER ROLE REGEX MAX_SECONDS  Wait for a message of a role
# after a number whose content matches an extended regular expression. It
# returns 0 when found, 1 on a timeout, 2 when the run ended without it.
# It sets WR_SEQ.
wait_reply() {
	_wr_t0=$(now)
	while :; do
		_wr_hit=$(run_messages "$1" "$2" 2>/dev/null |
			jq -r --arg role "$3" --arg re "$4" 'select(.role == $role and (.content | tostring | test($re))) | "\(.seq)\t\(.content | tostring | .[0:300])"' | head -n 1)
		if [ -n "$_wr_hit" ]; then
			WR_SEQ=${_wr_hit%%	*}
			return 0
		fi
		run_get "$1"
		case $RUN_STATE in
		FAILED | ERROR | CANCELLED)
			# A last look: the message can arrive with the end state.
			_wr_hit=$(run_messages "$1" "$2" 2>/dev/null |
				jq -r --arg role "$3" --arg re "$4" 'select(.role == $role and (.content | tostring | test($re))) | "\(.seq)\t\(.content | tostring | .[0:300])"' | head -n 1)
			if [ -n "$_wr_hit" ]; then
				WR_SEQ=${_wr_hit%%	*}
				return 0
			fi
			return 2
			;;
		esac
		[ $(($(now) - _wr_t0)) -lt "$5" ] || return 1
		sleep 2
	done
}

# dump_messages RUN_ID  Write the last messages of a run to the log.
dump_messages() {
	log "last messages of run $1:"
	run_messages "$1" 0 2>/dev/null | jq -r '"    \(.seq) \(.role): \(.content | tostring | .[0:240] | gsub("\n"; " "))"' | tail -n 12
}

# expect_reply RUN_ID AFTER ROLE REGEX MAX_SECONDS WHAT  wait_reply, and fail the check when it does not match.
expect_reply() {
	wait_reply "$1" "$2" "$3" "$4" "$5"
	_er_rc=$?
	case $_er_rc in
	0) evidence "$6: $3 message $WR_SEQ matches $4" ;;
	1)
		dump_messages "$1"
		fail "$6: no $3 message that matches $4 in $5 seconds ($(run_why "$1"))"
		;;
	*)
		dump_messages "$1"
		fail "$6: the run ended without it ($(run_why "$1"))"
		;;
	esac
}

# run_timeline RUN_ID  Print the timeline events as JSON lines.
run_timeline() {
	api_call GET "/runs/$1/timeline?limit=500"
	printf '%s' "$API_BODY" | jq -c '.items[]?'
}

# task_pods RUN_ID  The task Pods of a run: name, task id, phase.
task_pods() {
	kc -n "$TASK_NAMESPACE" get pods -o json 2>/dev/null |
		jq -r --arg id "$1." '.items[]? | select((.metadata.annotations["openfactory.dev/sandbox-id"] // "") | startswith($id)) |
			"\(.metadata.name) \(.metadata.annotations["openfactory.dev/sandbox-id"]) \(.status.phase)"'
}

# bucket_keys  The keys of the run data bucket, one per line (aws s3 ls). It
# returns 1 when the listing fails. Call it as: keys=$(bucket_keys) || fail "..."
bucket_keys() {
	_bk=$(out_json bucket)
	[ -n "$_bk" ] || return 1
	_bk_out=$(aws_emu s3 ls "s3://$_bk" --recursive 2>&1) || {
		printf '%s\n' "$_bk_out" >&2
		return 1
	}
	printf '%s\n' "$_bk_out" | awk 'NF >= 4 { $1 = $2 = $3 = ""; sub(/^ +/, ""); print }'
}

# fx_artifact_run  A run that writes an artifact. It sets
# ARTIFACT_RUN and ARTIFACT_NAME. The run is made once for all checks.
fx_artifact_run() {
	if [ -s "$STATE_DIR/artifact_run" ]; then
		ARTIFACT_RUN=$(sed -n 1p "$STATE_DIR/artifact_run")
		ARTIFACT_NAME=$(sed -n 2p "$STATE_DIR/artifact_run")
		return 0
	fi
	fx_check_factory
	_fa_tok=$(rand_token)
	ARTIFACT_NAME="fx-$_fa_tok.txt"
	_fa_prompt="Fixture run $_fa_tok. $(tool_directive write_file "$(jq -nc --arg p ".openfactory/artifacts/$ARTIFACT_NAME" --arg c "fixture $_fa_tok" '{path: $p, content: $c}')")"
	run_dispatch "$FACTORY_UID" "$_fa_prompt" "" ""
	ARTIFACT_RUN=$RUN_ID
	expect_reply "$ARTIFACT_RUN" 0 assistant '^DONE' 180 "the fixture run"
	# The artifact is uploaded after the turn. Wait for it.
	_fa_ok=
	_fa_t0=$(now)
	while [ $(($(now) - _fa_t0)) -lt 90 ]; do
		api_call GET "/runs/$ARTIFACT_RUN/artifacts"
		if printf '%s' "$API_BODY" | jq -e --arg n "$ARTIFACT_NAME" '[.artifacts[]? | select(.name == $n)] | length > 0' >/dev/null 2>&1; then
			_fa_ok=1
			break
		fi
		sleep 3
	done
	[ -n "$_fa_ok" ] || fail "the artifact $ARTIFACT_NAME of the fixture run $ARTIFACT_RUN was not uploaded in 90 seconds"
	printf '%s\n%s\n' "$ARTIFACT_RUN" "$ARTIFACT_NAME" >"$STATE_DIR/artifact_run"
}

# ---------- Pods that the checks start ----------

# run_check_pod NAMESPACE NAME SERVICE_ACCOUNT SCRIPT [ENV_NAME ENV_VALUE]  Start a
# Pod that runs a script in the image of check_pod_image, wait for it, print its log and
# delete it. It sets POD_PHASE. The Pod mounts the CA of the local AWS stack at
# /etc/aws-local-ca and a service account token of the audience
# pods.eks.amazonaws.com at /var/run/secrets/pods.eks.amazonaws.com/serviceaccount/token.
run_check_pod() {
	_pod_ns=$1
	_pod_name=$2
	_pod_sa=$3
	_pod_script=$4
	_pod_image=$(check_pod_image)
	_pod_env=
	if [ -n "${5:-}" ]; then
		_pod_env=$(jq -nc --arg n "$5" --arg v "${6:-}" '[{name: $n, value: $v}]')
	else
		_pod_env='[]'
	fi
	kc -n "$_pod_ns" delete pod "$_pod_name" --ignore-not-found --wait=true >/dev/null 2>&1
	jq -nc --arg name "$_pod_name" --arg ns "$_pod_ns" --arg sa "$_pod_sa" --arg image "$_pod_image" \
		--arg script "$_pod_script" --argjson env "$_pod_env" '{
		apiVersion: "v1", kind: "Pod",
		metadata: {name: $name, namespace: $ns, labels: {"app.kubernetes.io/part-of": "of-local-check"}},
		spec: {
			restartPolicy: "Never", serviceAccountName: $sa, terminationGracePeriodSeconds: 1,
			securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: "RuntimeDefault"}},
			containers: [{
				name: "check", image: $image, imagePullPolicy: ($ENV.LOCAL_PULL_POLICY // "Never"),
				command: ["sh", "-c", $script], env: $env,
				securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}},
				volumeMounts: [
					{name: "ca", mountPath: "/etc/aws-local-ca", readOnly: true},
					{name: "pi-token", mountPath: "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount", readOnly: true}
				]
			}],
			volumes: [
				{name: "ca", configMap: {name: "aws-local-ca"}},
				{name: "pi-token", projected: {sources: [{serviceAccountToken: {audience: "pods.eks.amazonaws.com", expirationSeconds: 3600, path: "token"}}]}}
			]
		}}
		# On EKS the Pod Identity webhook mounts the token itself, at the same path.
		| if $ENV.LOCAL_TARGET == "eks" then
			.spec.containers[0].volumeMounts |= map(select(.name != "pi-token"))
			| .spec.volumes |= map(select(.name != "pi-token"))
		else . end' | kc apply -f - >/dev/null || fail "cannot create the Pod $_pod_ns/$_pod_name"
	POD_PHASE=
	_pod_t0=$(now)
	while [ $(($(now) - _pod_t0)) -lt 150 ]; do
		POD_PHASE=$(kc -n "$_pod_ns" get pod "$_pod_name" -o jsonpath='{.status.phase}' 2>/dev/null)
		case $POD_PHASE in Succeeded | Failed) break ;; esac
		sleep 2
	done
	# shellcheck disable=SC2034 # read by the checks that call run_check_pod
	POD_LOG=$(kc -n "$_pod_ns" logs "$_pod_name" 2>&1)
	log "Pod $_pod_ns/$_pod_name ended in phase ${POD_PHASE:-unknown}"
	kc -n "$_pod_ns" delete pod "$_pod_name" --ignore-not-found --wait=false >/dev/null 2>&1
	return 0
}
