# shellcheck shell=sh
# Checks A1 to A4, S1 and S2. Source this file.

# ---------- A1 ----------

check_A1() {
	[ "$LOCAL_TARGET" != eks ] || skip "LOCAL_TARGET=eks: the stack was made outside deploy/aws/provision.sh"
	need_cmd aws jq sh
	need_file "$LOCAL_DIR/outputs.json"
	need_file "$REPO/deploy/aws/provision.sh"
	aws_guard
	_a1_dir=$WORK/a1
	mkdir -p "$_a1_dir"
	_a1_run() {
		# A1 run NUMBER: provision.sh into a temporary outputs file.
		env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_PROFILE \
			AWS_TARGET=emulator OUTPUTS="$_a1_dir/outputs.$1.json" AWS_LOCAL_DIR="$LOCAL_DIR/aws" LOCAL_DIR="$LOCAL_DIR" \
			KUBE_CONTEXT="$KUBE_CONTEXT" LOCAL_DOMAIN="$LOCAL_DOMAIN" \
			PREFIX="$(out_json prefix)" AWS_REGION="$(out_json region)" \
			SERVER_NAMESPACE="$(out_json serverNamespace)" SERVER_SERVICE_ACCOUNT="$(out_json serverServiceAccount)" \
			sh "$REPO/deploy/aws/provision.sh" >"$_a1_dir/run.$1.txt" 2>&1
	}
	_a1_run 1 || {
		tail -n 20 "$_a1_dir/run.1.txt"
		fail "the first run of provision.sh failed"
	}
	_a1_run 2 || {
		tail -n 20 "$_a1_dir/run.2.txt"
		fail "the second run of provision.sh failed"
	}
	log "output of the second run:"
	cat "$_a1_dir/run.2.txt"
	# provision.sh starts each resource line with its action. The line about the
	# outputs file says "created" each time, because the script writes the file.
	_a1_acted=$(grep -i -E '^(created|updated|changed|deleted|replaced|added|removed)[[:space:]]' "$_a1_dir/run.2.txt" | grep -v -i 'outputs file')
	if [ -n "$_a1_acted" ]; then
		fail "the second run of provision.sh changed resources: $(printf '%s' "$_a1_acted" | head -n 2 | tr '\n' ' ' | cut -c1-200)"
	fi
	_a1_same=$(grep -c -i '^no change[[:space:]]' "$_a1_dir/run.2.txt")
	[ "$_a1_same" -ge 10 ] || fail "the second run of provision.sh reports only $_a1_same resource(s) as 'no change'"
	for _a1_n in 1 2; do
		[ -s "$_a1_dir/outputs.$_a1_n.json" ] || fail "run $_a1_n of provision.sh wrote no outputs file"
		if [ "$(jq -S . "$_a1_dir/outputs.$_a1_n.json")" != "$(jq -S . "$LOCAL_DIR/outputs.json")" ]; then
			jq -S . "$_a1_dir/outputs.$_a1_n.json" >"$_a1_dir/got.json"
			jq -S . "$LOCAL_DIR/outputs.json" >"$_a1_dir/want.json"
			diff "$_a1_dir/got.json" "$_a1_dir/want.json" 2>&1 | head -n 20
			fail "the outputs of run $_a1_n differ from $LOCAL_DIR/outputs.json"
		fi
	done
	evidence "provision.sh ran twice into a temporary outputs file, the second run reported $_a1_same resources as 'no change' and created or changed none, and both outputs equal outputs.json"
	pass
}

# ---------- A2 ----------

# a2_env_names POD  The names (never the values) of the environment variables of
# a running server container.
a2_env_names() {
	kc -n of-local exec "$1" -c server -- sh -c 'env | cut -d= -f1' 2>/dev/null | sort
}

check_A2() {
	need_chart
	need_cmd kubectl jq aws
	aws_guard
	_a2_pods=$(server_pods)
	[ -n "$_a2_pods" ] || fail "no server Pod in of-local"
	kc -n of-local get pods -l app.kubernetes.io/instance=openfactory,app.kubernetes.io/component=server -o json >"$WORK/a2-pods.json"
	for _a2_p in $_a2_pods; do
		# 1. The Pod spec has no static key, and the container credential variables are set.
		_a2_bad=$(jq -r --arg p "$_a2_p" '.items[] | select(.metadata.name == $p) | .spec.containers[] | .env[]? |
			select(.name | test("^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)$")) | .name' "$WORK/a2-pods.json")
		[ -z "$_a2_bad" ] || fail "the Pod $_a2_p sets $_a2_bad in its spec"
		# 2. The ConfigMaps and Secrets that the Pod reads have no static key.
		for _a2_ref in $(jq -r --arg p "$_a2_p" '.items[] | select(.metadata.name == $p) | .spec.containers[] |
			.envFrom[]? | (if .secretRef then "secret/" + .secretRef.name else "configmap/" + .configMapRef.name end)' "$WORK/a2-pods.json" | sort -u); do
			_a2_keys=$(kc -n of-local get "$_a2_ref" -o json 2>/dev/null | jq -r '(.data // {}) | keys[]')
			if printf '%s\n' "$_a2_keys" | grep -Eq '^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)$'; then
				fail "$_a2_ref (read by $_a2_p) has a static AWS key"
			fi
		done
		# 3. The running process has none (only names are read).
		_a2_names=$(a2_env_names "$_a2_p")
		if [ -z "$_a2_names" ]; then
			log "kubectl exec into $_a2_p gave no environment; the spec checks stand alone"
		else
			printf '%s\n' "$_a2_names" | grep -Eq '^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)$' && fail "the process in $_a2_p has a static AWS key in its environment"
			printf '%s\n' "$_a2_names" | grep -q '^AWS_CONTAINER_CREDENTIALS_FULL_URI$' || fail "the process in $_a2_p has no AWS_CONTAINER_CREDENTIALS_FULL_URI"
			printf '%s\n' "$_a2_names" | grep -q '^AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE$' || fail "the process in $_a2_p has no AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"
		fi
		evidence "$_a2_p has no AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY or AWS_SESSION_TOKEN"
	done
	# 4. A run writes its data, and the objects are in the bucket.
	fx_artifact_run
	_a2_keys=$(bucket_keys) || fail "aws s3 ls s3://$(out_json bucket) failed"
	printf '%s\n' "$_a2_keys" | grep -c . | sed 's/^/objects in the bucket: /'
	printf '%s\n' "$_a2_keys" | grep -q "$ARTIFACT_RUN" || fail "the bucket $(out_json bucket) has no object for the run $ARTIFACT_RUN"
	evidence "the bucket has objects of the run $ARTIFACT_RUN: $(printf '%s\n' "$_a2_keys" | grep -c "$ARTIFACT_RUN") key(s)"
	# 5. The broker issued credentials to the server service account.
	if [ "$LOCAL_TARGET" = eks ]; then
		_a2_assoc=$(aws_real eks list-pod-identity-associations --cluster-name "$(out_json clusterName)" \
			--namespace of-local --service-account openfactory --query 'associations[0].associationId' --output text 2>&1)
		case $_a2_assoc in a-*) ;; *) fail "no Pod Identity association for of-local/openfactory: $_a2_assoc" ;; esac
		evidence "Pod Identity association $_a2_assoc gives of-local/openfactory its credentials"
		pass
		return
	fi
	_a2_log=$(kc -n aws-local logs deploy/aws-emulator --tail=-1 2>&1)
	_a2_n=$(printf '%s\n' "$_a2_log" | grep -c 'issued ns=of-local sa=openfactory')
	[ "$_a2_n" -ge 1 ] || {
		printf '%s\n' "$_a2_log" | grep -E 'issued|refused' | tail -n 5
		fail "the broker log has no line 'issued ns=of-local sa=openfactory'"
	}
	evidence "the broker log has $_a2_n line(s) 'issued ns=of-local sa=openfactory'"
	pass
}

# ---------- A3 ----------

check_A3() {
	[ "$LOCAL_TARGET" != eks ] || skip "LOCAL_TARGET=eks: real Pod Identity credentials last hours, so a renewal does not happen within the check"
	need_chart
	need_cmd kubectl jq curl
	_a3_t0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	_a3_start=$(now)
	_a3_tok=$(rand_token)
	printf 'a3 %s\n' "$_a3_tok" >"$WORK/a3.txt"
	_a3_i=0
	log "write to the object store every 15 seconds for 4 minutes. The broker caps the credentials at 6 minutes, so the driver must renew them."
	while [ $(($(now) - _a3_start)) -lt 240 ]; do
		_a3_i=$((_a3_i + 1))
		api_upload "/attachments?name=a3-$_a3_tok-$_a3_i.txt" "$WORK/a3.txt" text/plain
		case $API_CODE in 2??) ;; *) log "upload $_a3_i answered $API_CODE" ;; esac
		sleep 15
	done
	log "uploads: $_a3_i"
	kc -n aws-local logs deploy/aws-emulator --tail=-1 --timestamps --since-time="$_a3_t0" 2>&1 | grep 'issued ns=of-local sa=openfactory' >"$WORK/a3-issued.txt"
	_a3_n=$(grep -c . "$WORK/a3-issued.txt")
	log "issued lines since $_a3_t0: $_a3_n"
	sed 's/\(role=\)[^ ]*/\1.../' "$WORK/a3-issued.txt"
	[ "$_a3_n" -ge 3 ] || fail "the broker issued $_a3_n credential set(s) to the server in 4 minutes of use; renewal needs at least 3 (two replicas first issue at most 2)"
	_a3_distinct=$(sed -n 's/.*expires=\([^ ]*\).*/\1/p' "$WORK/a3-issued.txt" | sort -u | grep -c .)
	[ "$_a3_distinct" -ge 3 ] || fail "only $_a3_distinct distinct expires values among $_a3_n issued lines"
	# Each expiry must be inside the cap (6 minutes) of the time of its line.
	while IFS= read -r _a3_line; do
		_a3_ts=${_a3_line%% *}
		_a3_exp=$(printf '%s\n' "$_a3_line" | sed -n 's/.*expires=\([^ ]*\).*/\1/p')
		_a3_ets=$(iso_to_epoch "$_a3_ts")
		_a3_eex=$(iso_to_epoch "$_a3_exp")
		[ -n "$_a3_ets" ] && [ -n "$_a3_eex" ] || continue
		_a3_left=$((_a3_eex - _a3_ets))
		[ "$_a3_left" -le 420 ] || fail "credentials issued at $_a3_ts expire $_a3_left seconds later, more than the cap of 6 minutes"
	done <"$WORK/a3-issued.txt"
	evidence "$_a3_n credential sets in 4 minutes with $_a3_distinct distinct expiry times, each within the 6 minute cap"
	pass
}

# ---------- A4 ----------

check_A4() {
	need_chart
	need_cmd kubectl jq
	if [ "$LOCAL_TARGET" = eks ]; then
		# The Pod Identity webhook gives credential variables only to service accounts with an association.
		_a4_script='env | grep -q "^AWS_CONTAINER_CREDENTIALS_FULL_URI=" && echo HAS-CREDENTIAL-URI || echo NO-CREDENTIAL-URI'
		kc -n of-local create serviceaccount of-local-check-other --dry-run=client -o yaml | kc apply -f - >/dev/null || fail "cannot make the service account of-local-check-other"
		run_check_pod of-local of-local-check-a4 of-local-check-other "$_a4_script"
		printf '%s\n' "$POD_LOG" | grep -q '^NO-CREDENTIAL-URI$' || fail "a service account without an association got AWS_CONTAINER_CREDENTIALS_FULL_URI"
		run_check_pod of-local of-local-check-a4-control openfactory "$_a4_script"
		printf '%s\n' "$POD_LOG" | grep -q '^HAS-CREDENTIAL-URI$' || fail "the service account openfactory got no AWS_CONTAINER_CREDENTIALS_FULL_URI"
		evidence "of-local-check-other: no credential URI; openfactory: credential URI set"
		pass
		return
	fi
	_a4_url="https://aws.$LOCAL_DOMAIN:9443/__pod-identity/v1/credentials"
	# shellcheck disable=SC2016 # $URL and $(...) run in the check Pod; they expand later, not here
	_a4_script='code=$(curl -sS -m 20 --cacert /etc/aws-local-ca/ca.crt -o /tmp/body -w "%{http_code}" -H "Authorization: $(cat /var/run/secrets/pods.eks.amazonaws.com/serviceaccount/token)" "$URL"); echo "HTTP=$code"; if grep -q AccessKeyId /tmp/body; then echo "BODY-HAS-CREDENTIALS"; else head -c 300 /tmp/body; echo; fi'
	kc -n of-local create serviceaccount of-local-check-other --dry-run=client -o yaml | kc apply -f - >/dev/null || fail "cannot make the service account of-local-check-other"
	# The service account without an association is refused.
	run_check_pod of-local of-local-check-a4 of-local-check-other "$_a4_script" URL "$_a4_url"
	printf '%s\n' "$POD_LOG" | sed 's/^/    /'
	printf '%s\n' "$POD_LOG" | grep -q '^HTTP=403$' || fail "the service account without an association got $(printf '%s\n' "$POD_LOG" | grep '^HTTP=' | head -n 1), not HTTP=403"
	printf '%s\n' "$POD_LOG" | grep -q 'BODY-HAS-CREDENTIALS' && fail "the broker returned credentials to a service account without an association"
	evidence "service account of-local-check-other: HTTP=403 and no credentials"
	# The control: the server service account gets credentials through the same path.
	run_check_pod of-local of-local-check-a4-control openfactory "$_a4_script" URL "$_a4_url"
	printf '%s\n' "$POD_LOG" | grep '^HTTP=' | sed 's/^/    control: /'
	printf '%s\n' "$POD_LOG" | grep -q '^HTTP=200$' || fail "the control (service account openfactory, same path) got $(printf '%s\n' "$POD_LOG" | grep '^HTTP=' | head -n 1), not HTTP=200, so the 403 proves nothing"
	evidence "control: service account openfactory gets HTTP=200 on the same path"
	_a4_broker=$(kc -n aws-local logs deploy/aws-emulator --tail=-1 2>&1)
	printf '%s\n' "$_a4_broker" | grep -q 'refused ns=of-local sa=of-local-check-other' || fail "the broker log has no line 'refused ns=of-local sa=of-local-check-other'"
	evidence "the broker log has 'refused ns=of-local sa=of-local-check-other'"
	pass
}

# ---------- S1 ----------

# check_S1_run  One run of the live tests. It uses $_s1_url, $_s1_region and $_s1_out of check_S1.
check_S1_run() {
	(
		cd "$REPO" || exit 1
		unset AWS_PROFILE AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE AWS_SESSION_TOKEN \
			AWS_CONTAINER_CREDENTIALS_FULL_URI AWS_CONTAINER_CREDENTIALS_RELATIVE_URI \
			AWS_CONTAINER_AUTHORIZATION_TOKEN AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
		if [ "$LOCAL_TARGET" = eks ]; then
			# The Go driver reads static keys: export the keys of the CLI session.
			eval "$(aws_real configure export-credentials --format env)"
		elif [ -f "$LOCAL_DIR/aws/server.env" ]; then
			# shellcheck source=/dev/null
			. "$LOCAL_DIR/aws/server.env"
		else
			AWS_CA_BUNDLE=$LOCAL_DIR/aws/ca.crt
			AWS_ACCESS_KEY_ID=$(sed -n 's/^aws_access_key_id *= *//p' "$LOCAL_DIR/aws/credentials")
			AWS_SECRET_ACCESS_KEY=$(sed -n 's/^aws_secret_access_key *= *//p' "$LOCAL_DIR/aws/credentials")
			export AWS_CA_BUNDLE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
		fi
		AWS_REGION=${_s1_region:-us-east-1}
		export AWS_REGION
		# The same settings as make test-aws-local, with the bucket of outputs.json.
		OF_TEST_S3_URL=$_s1_url OF_TEST_S3_MAX_OBJECTS=1000 \
			go test -count=1 -run Live -v ./internal/objectstore
	) >"$_s1_out" 2>&1
	return $?
}

check_S1() {
	need_cmd go jq
	need_file "$LOCAL_DIR/outputs.json"
	_s1_bucket=$(out_json bucket)
	_s1_region=$(out_json region)
	_s1_url="s3://$_s1_bucket/check-s1?region=${_s1_region:-us-east-1}&endpoint=https://aws.$LOCAL_DOMAIN:9443&path_style=true"
	[ "$LOCAL_TARGET" != eks ] || _s1_url="s3://$_s1_bucket/check-s1?region=$_s1_region"
	_s1_out=$WORK/s1.out
	_s1_try=0
	while :; do
		_s1_try=$((_s1_try + 1))
		check_S1_run
		_s1_rc=$?
		# A name lookup can fail for a moment on a workstation. Try again once.
		if [ "$_s1_rc" -ne 0 ] && [ "$_s1_try" -lt 2 ] && grep -q 'no such host' "$_s1_out"; then
			log "a name lookup failed: try again"
			sleep 3
			continue
		fi
		break
	done
	sed -e 's/\(AWS_[A-Z_]*=\)[^ ]*/\1***/g' "$_s1_out" | tail -n 40
	[ "$_s1_rc" -eq 0 ] || fail "go test -run Live ./internal/objectstore failed (exit $_s1_rc)"
	for _s1_t in TestLiveS3Store TestLiveS3RunData; do
		grep -q -- "--- PASS: $_s1_t" "$_s1_out" || fail "$_s1_t did not pass (skipped or missing)"
	done
	grep -q -- '--- FAIL' "$_s1_out" && fail "a live test failed"
	evidence "TestLiveS3Store and TestLiveS3RunData passed against $(out_json bucket) on the emulator"
	pass
}

# ---------- S2 ----------

check_S2() {
	need_cmd kubectl jq
	fx_artifact_run
	api_ok GET "/runs/$ARTIFACT_RUN/artifacts"
	_s2_url=$(printf '%s' "$API_BODY" | jq -r --arg n "$ARTIFACT_NAME" '[.artifacts[]? | select(.name == $n)][0].url // empty')
	[ -n "$_s2_url" ] || fail "the artifact list of run $ARTIFACT_RUN has no presigned url (the store cannot presign?)"
	_s2_host=$(printf '%s' "$_s2_url" | sed 's|^[a-z]*://\([^/]*\)/.*|\1|')
	log "presigned URL host: $_s2_host (the signature is not logged)"
	_s2_ca="--cacert /etc/aws-local-ca/ca.crt"
	case $_s2_host in
	"aws.$LOCAL_DOMAIN:9443") ;;
	*.amazonaws.com) [ "$LOCAL_TARGET" = eks ] || fail "the presigned URL has the host $_s2_host outside LOCAL_TARGET=eks"; _s2_ca= ;;
	*) fail "the presigned URL has the host $_s2_host, not aws.$LOCAL_DOMAIN:9443: Pods and clients cannot share it" ;;
	esac
	# shellcheck disable=SC2016 # $URL and $(...) run in the check Pod; they expand later, not here
	_s2_script='code=$(curl -sS -m 30 '"$_s2_ca"' -o /tmp/obj -w "%{http_code}" "$URL"); echo "HTTP=$code"; echo "SIZE=$(wc -c </tmp/obj | tr -d " ")"; echo "CONTENT=$(head -c 200 /tmp/obj)"'
	run_check_pod "$TASK_NAMESPACE" of-local-check-s2 default "$_s2_script" URL "$_s2_url"
	printf '%s\n' "$POD_LOG" | sed 's/X-Amz-[A-Za-z-]*=[^& ]*/X-Amz-...=***/g' | sed 's/^/    /'
	printf '%s\n' "$POD_LOG" | grep -q '^HTTP=200$' || fail "a Pod in $TASK_NAMESPACE got $(printf '%s\n' "$POD_LOG" | grep '^HTTP=' | head -n 1) from the presigned URL, not HTTP=200"
	printf '%s\n' "$POD_LOG" | grep -q "^CONTENT=fixture " || fail "the Pod read other content than the artifact that the run wrote"
	evidence "a Pod in $TASK_NAMESPACE read the artifact $ARTIFACT_NAME of run $ARTIFACT_RUN through the presigned URL, with the CA of the stack"
	pass
}
