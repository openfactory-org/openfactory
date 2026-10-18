#!/bin/sh
# Start the local test stack: the AWS emulator, PostgreSQL, a git server and
# two Open Factory servers in a local Kubernetes cluster. The claude harness
# runs a stand-in CLI with fixed answers (deploy/local/testkit).
# Test only. NOT FOR PRODUCTION. Every step is safe to run again.
#
#   make local-up        (or: sh deploy/local/up.sh)
#
# Settings: see lib.sh and docs/self-hosting/local-testing.md
# (LOCAL_SERVER_MODE, LOCAL_MODEL, LOCAL_FORGE, LOCAL_GITHUB_REPO, KUBE_CONTEXT,
# LOCAL_DOMAIN, LOCAL_DIR, LOCAL_BUILD).
#
# KUBE_CONTEXT=kind-<name> uses a kind cluster, and creates it when it does not
# exist (see kind.sh). The default is the OrbStack cluster.
set -eu
. "$(dirname "$0")/lib.sh"
# shellcheck source=deploy/local/kind.sh
[ -z "$KIND_CLUSTER" ] || . "$LOCAL_REPO/deploy/local/kind.sh"
cd "$LOCAL_REPO"
check_settings
# The scripts that up.sh starts (host.sh) read the same settings.
export LOCAL_SERVER_MODE LOCAL_MODEL LOCAL_FORGE LOCAL_GITHUB_REPO KUBE_CONTEXT LOCAL_DOMAIN LOCAL_DIR LOCAL_BUILD LOCAL_RETENTION LOCAL_IMAGE_TAG

for t in docker kubectl aws openssl curl jq go; do
	have "$t" || die "$t is not on the PATH"
done
docker info >/dev/null 2>&1 || die "docker does not answer: start it and try again"
if [ -n "$KIND_CLUSTER" ]; then
	kind_ensure_cluster
	kind_check_names
fi
k config get-contexts "$KUBE_CONTEXT" >/dev/null 2>&1 || die "the kubectl context $KUBE_CONTEXT does not exist (set KUBE_CONTEXT)"

mkdir -p "$LOCAL_DIR"
chmod 700 "$LOCAL_DIR"

# The secrets are made once and kept (mode 0600).
gen_file "$LOCAL_DIR/secret.key" openssl rand -base64 32
gen_file "$LOCAL_DIR/db.password" openssl rand -hex 24
gen_file "$LOCAL_DIR/webhook.secret" openssl rand -hex 32

# ---------- 1. images ----------

step_images() {
	log "1/7 images"
	if [ "$LOCAL_BUILD" = 1 ]; then
		IMAGE_TAG=$LOCAL_IMAGE_TAG sh scripts/images/build.sh build server tool-claude sandbox
		docker build -t "$TESTKIT_IMAGE" -f deploy/local/testkit/Dockerfile .
		if [ "$LOCAL_MODEL" = claude ]; then
			# The real CLI stays in the image as claude-real (check R4).
			docker build --target tool-claude --build-arg TOOL_BASE="$(image_ref tool-claude)" -t "$STANDIN_IMAGE" -f deploy/local/testkit/Dockerfile .
		else
			docker build --target tool-claude -t "$STANDIN_IMAGE" -f deploy/local/testkit/Dockerfile .
		fi
		if [ "$LOCAL_SERVER_MODE" = host ]; then
			make build
		fi
	else
		note "LOCAL_BUILD=0: use the images that exist"
		if [ "$LOCAL_SERVER_MODE" = host ] && [ ! -x bin/openfactory ]; then
			make build
		fi
	fi
	for ref in "$(image_ref server)" "$(image_ref tool-claude)" "$(image_ref sandbox)" "$TESTKIT_IMAGE" "$STANDIN_IMAGE"; do
		docker image inspect "$ref" >/dev/null 2>&1 || die "the image $ref does not exist: run with LOCAL_BUILD=1"
	done
	# The Pods use the pull policy Never, so PostgreSQL must be in the local Docker.
	docker image inspect postgres:16 >/dev/null 2>&1 || docker pull postgres:16
	if [ -n "$KIND_CLUSTER" ]; then
		note "load the images into the kind cluster $KIND_CLUSTER"
		kind_load "$(image_ref server)" "$(image_ref tool-claude)" "$(image_ref sandbox)" \
			"$TESTKIT_IMAGE" "$STANDIN_IMAGE" postgres:16
	fi
}

# ---------- 2. emulator ----------

ns_apply() {
	k create namespace "$1" --dry-run=client -o yaml | k apply -f - >/dev/null
}

step_emulator() {
	log "2/7 AWS emulator at https://$AWS_HOST:9443"
	AWS_LOCAL_DIR=$LOCAL_DIR/aws AWS_LOCAL_HOST=$AWS_HOST AWS_LOCAL_BUCKET='' \
		AWS_LOCAL_KEEP=1 AWS_LOCAL_BUILD=$LOCAL_BUILD KUBE_CONTEXT=$KUBE_CONTEXT \
		sh deploy/aws-local/up.sh
	emulator_env_ok || die "aws sts get-caller-identity does not return the emulator account 000000000000: stopping"
	[ -z "$KIND_CLUSTER" ] || kind_dns
	ns_apply "$NS_SERVER"
	ns_apply "$NS_SANDBOXES"
	for ns in "$NS_SERVER" "$NS_SANDBOXES"; do
		k -n "$ns" create configmap aws-local-ca --from-file=ca.crt="$LOCAL_DIR/aws/ca.crt" \
			--dry-run=client -o yaml | k apply -f - >/dev/null
	done
	note "the CA is in the ConfigMap aws-local-ca of $NS_SERVER and $NS_SANDBOXES"
}

# ---------- 3. provisioning ----------

step_provision() {
	log "3/7 provision the AWS resources in the emulator"
	env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_PROFILE \
		-u AWS_ROLE_ARN -u AWS_WEB_IDENTITY_TOKEN_FILE \
		AWS_TARGET=emulator AWS_LOCAL_DIR="$LOCAL_DIR/aws" OUTPUTS="$LOCAL_DIR/outputs.json" \
		SERVER_NAMESPACE=$NS_SERVER SERVER_SERVICE_ACCOUNT=$RELEASE \
		sh deploy/aws/provision.sh
	[ -s "$LOCAL_DIR/outputs.json" ] || die "the provisioning script wrote no $LOCAL_DIR/outputs.json"
	BUCKET=$(jq -r '.bucket // empty' "$LOCAL_DIR/outputs.json")
	[ -n "$BUCKET" ] || die "no bucket in $LOCAL_DIR/outputs.json"
}

# ---------- 4. infrastructure ----------

step_infra() {
	log "4/7 PostgreSQL and git server in $NS_INFRA"
	ns_apply "$NS_INFRA"
	k -n "$NS_INFRA" create secret generic postgres-auth \
		--from-file=password="$LOCAL_DIR/db.password" --dry-run=client -o yaml | k apply -f - >/dev/null
	k apply -f deploy/local/infra.yaml >/dev/null
	if [ -n "$KIND_CLUSTER" ]; then
		for svc in postgres git; do
			kind_expose "$NS_INFRA" "$svc"
		done
		kind_dns
	fi
	# The test image keeps its tag: restart the Pods that use it when it changes.
	id=$(image_id "$TESTKIT_IMAGE")
	k -n "$NS_INFRA" patch deploy/git --type merge \
		-p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"of-local/image-id\":\"$id\"}}}}}" >/dev/null
	for d in postgres git; do
		k -n "$NS_INFRA" rollout status deploy/"$d" --timeout=180s
	done
}

# ---------- 5. server ----------

# write_pod_template  Makes $LOCAL_DIR/pod-template.yaml: the template of sandbox Pods.
write_pod_template() {
	(
		P_SANDBOX_IMAGE=$(image_ref sandbox)
		export P_SANDBOX_IMAGE
		umask 077
		render deploy/local/pod-template.yaml > "$LOCAL_DIR/pod-template.yaml"
	)
}

# gen_values  Makes $LOCAL_DIR/values.generated.yaml from deploy/local/values.yaml.
gen_values() {
	server_ref=$(image_ref server)
	ids="$(image_id "$server_ref")-$(image_id "$TESTKIT_IMAGE")-$(cksum < "$LOCAL_DIR/aws/ca.crt" | cut -d' ' -f1)"
	(
		P_SECRET_KEY=$(secret_value "$LOCAL_DIR/secret.key")
		P_SERVER_REPOSITORY=${server_ref%:*}
		P_SERVER_TAG=${server_ref##*:}
		P_DATABASE_URL="postgres://openfactory:$(secret_value "$LOCAL_DIR/db.password")@postgres.$NS_INFRA.svc:5432/openfactory?sslmode=disable"
		P_OBJECT_STORE_URL="s3://$BUCKET/runs?region=us-east-1&endpoint=https://$AWS_HOST:9443&path_style=true"
		P_RETENTION=$LOCAL_RETENTION
		P_RUNNER_IMAGE=$server_ref
		P_RUNNER_CLAUDE_IMAGE=$STANDIN_IMAGE
		P_SANDBOX_IMAGE=$(image_ref sandbox)
		P_PUBLIC_URL=http://$SERVER_HOST
		P_IMAGE_IDS=$ids
		P_TESTKIT_IMAGE=$TESTKIT_IMAGE
		P_AWS_HOST=$AWS_HOST
		P_POD_TEMPLATE=$(sed 's/^/    /' "$LOCAL_DIR/pod-template.yaml")
		export P_SECRET_KEY P_SERVER_REPOSITORY P_SERVER_TAG P_DATABASE_URL P_OBJECT_STORE_URL P_RETENTION \
			P_RUNNER_IMAGE P_RUNNER_CLAUDE_IMAGE P_SANDBOX_IMAGE P_PUBLIC_URL P_IMAGE_IDS \
			P_TESTKIT_IMAGE P_AWS_HOST P_POD_TEMPLATE
		umask 077
		render deploy/local/values.yaml > "$LOCAL_DIR/values.generated.yaml"
	)
}

step_server_chart() {
	log "5/7 server from the Helm chart in $NS_SERVER (2 replicas)"
	sh deploy/local/host.sh stop
	write_pod_template
	gen_values
	(
		umask 077
		OF_HELM_RENDER_VALUES="$LOCAL_DIR/values.generated.yaml" \
			OF_HELM_RENDER_OUT="$LOCAL_DIR/rendered.yaml" \
			OF_HELM_RENDER_NAMESPACE=$NS_SERVER OF_HELM_RENDER_RELEASE=$RELEASE \
			go test ./deploy/helm -run TestRenderToFile -count=1 >"$LOCAL_DIR/render.log" 2>&1
	) || { cat "$LOCAL_DIR/render.log" >&2; die "rendering the chart failed"; }
	# The renderer puts the namespace in every object (the sandbox Role goes to $NS_SANDBOXES).
	chmod 600 "$LOCAL_DIR/rendered.yaml"
	k apply -f "$LOCAL_DIR/rendered.yaml" >/dev/null
	if [ -n "$KIND_CLUSTER" ]; then
		kind_expose "$NS_SERVER" "$RELEASE"
		kind_dns
	fi
	k -n "$NS_SERVER" rollout status deploy/"$RELEASE" --timeout=300s
	ready=$(k -n "$NS_SERVER" get deploy/"$RELEASE" -o jsonpath='{.status.readyReplicas}')
	[ "${ready:-0}" = 2 ] || die "$ready of 2 server replicas are ready"
}

step_server_host() {
	log "5/7 two server processes on the workstation (ports $HOST_PORT_1 and $HOST_PORT_2)"
	# The chart objects of an earlier chart-mode run would take port 80 and the sandbox namespace.
	k -n "$NS_SERVER" delete deploy,service,pdb,configmap,secret,serviceaccount \
		-l app.kubernetes.io/instance=$RELEASE --ignore-not-found >/dev/null
	write_pod_template
	sh deploy/local/host.sh start
}

# ---------- 6. ready and bootstrap ----------

# host_not_ready N  Stop with what is known about host server N.
host_not_ready() {
	_pid=$(cat "$LOCAL_DIR/run/server-$1.pid" 2>/dev/null || true)
	if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
		echo "host server $1 (pid $_pid) runs, but /readyz does not answer 200. Last log lines:" >&2
	else
		echo "host server $1 (pid ${_pid:-?}) is not running. Last log lines:" >&2
	fi
	tail -n 5 "$LOCAL_DIR/logs/server-$1.log" >&2 2>/dev/null || true
	die "host server $1 is not ready: see $LOCAL_DIR/logs/server-$1.log"
}

wait_http() { # URL TRIES
	i=0
	until curl -fsS --max-time 3 -o /dev/null "$1" 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -lt "$2" ] || return 1
		sleep 2
	done
}

step_bootstrap() {
	log "6/7 wait for the server and make the first team"
	url=$(server_url)
	if [ "$LOCAL_SERVER_MODE" = host ]; then
		wait_http "http://127.0.0.1:$HOST_PORT_1/readyz" 90 || host_not_ready 1
		wait_http "http://127.0.0.1:$HOST_PORT_2/readyz" 90 || host_not_ready 2
	else
		wait_http "$url/readyz" 90 || die "the server is not reachable at $url/readyz (does $SERVER_HOST resolve to the LoadBalancer Service? see LOCAL_DOMAIN)"
	fi
	API_KEY=$(env_value OF_API_KEY)
	if [ -n "$API_KEY" ]; then
		# Right after a rolling update, a connection can go to an old replica
		# that stops (code 000). Try again, so that a working key is kept.
		i=0
		while :; do
			code=$(curl_auth "$API_KEY" -s -o /dev/null -w '%{http_code}' --max-time 10 "$url/api/v1/me" || true)
			i=$((i + 1))
			[ "$code" = 000 ] && [ "$i" -lt 15 ] || break
			sleep 2
		done
		if [ "$code" = 200 ]; then
			note "the API key in $LOCAL_DIR/env still works: keep it"
			return
		fi
	fi
	resp=$(mktemp "$LOCAL_DIR/bootstrap.XXXXXX")
	code=$(curl -s -o "$resp" -w '%{http_code}' --max-time 20 --retry 5 --retry-connrefused --retry-delay 2 \
		-X POST -H 'Content-Type: application/json' \
		-d '{"team_name":"of-local","admin_email":"admin@of-local.test","admin_name":"Local admin","key_name":"local"}' \
		"$url/api/v1/bootstrap" || true)
	case $code in
	201)
		API_KEY=$(jq -r '.key // empty' "$resp")
		rm -f "$resp"
		[ -n "$API_KEY" ] || die "the bootstrap answer has no key"
		note "made the team of-local and the admin admin@of-local.test"
		;;
	409)
		rm -f "$resp"
		die "the server is already bootstrapped, and $LOCAL_DIR/env has no working API key. Run: make local-down, then make local-up (this deletes the database and all runs)"
		;;
	*)
		rm -f "$resp"
		die "the bootstrap failed (HTTP $code)"
		;;
	esac
}

# ---------- 7. files and summary ----------

step_files() {
	log "7/7 write $LOCAL_DIR/env"
	url=$(server_url)
	(
		umask 077
		{
			echo "# Client settings of the local stack (test only). Do not share: it holds an API key."
			echo "export OF_SERVER='$url'"
			echo "export OF_API_KEY='$API_KEY'"
			echo "export OF_PUBLIC_URL='$url'"
			echo "export LOCAL_SERVER_MODE='$LOCAL_SERVER_MODE'"
			echo "export LOCAL_MODEL='$LOCAL_MODEL'"
			echo "export LOCAL_FORGE='$LOCAL_FORGE'"
			[ -z "$LOCAL_GITHUB_REPO" ] || echo "export LOCAL_GITHUB_REPO='$LOCAL_GITHUB_REPO'"
			echo "export LOCAL_DOMAIN='$LOCAL_DOMAIN'"
			echo "export LOCAL_DIR='$LOCAL_DIR'"
			echo "export KUBE_CONTEXT='$KUBE_CONTEXT'"
		} > "$LOCAL_DIR/env.new"
	)
	chmod 600 "$LOCAL_DIR/env.new"
	mv "$LOCAL_DIR/env.new" "$LOCAL_DIR/env"
}

summary() {
	url=$(server_url)
	echo
	echo "The local stack is ready (server mode: $LOCAL_SERVER_MODE, model: $LOCAL_MODEL, forge: $LOCAL_FORGE)."
	echo "  Server:      $url"
	echo "  Dashboard:   $url/login   (admin@of-local.test and OF_API_KEY from $LOCAL_DIR/env)"
	echo "  AWS:         https://$AWS_HOST:9443   ( . $LOCAL_DIR/aws/connect.env && aws s3 ls )"
	echo "  PostgreSQL:  $DB_HOST:5432 (user and database openfactory; password in $LOCAL_DIR/db.password)"
	if [ "$LOCAL_MODEL" = claude ]; then
		echo "  claude CLI:  $STANDIN_IMAGE (the real CLI for a run with a model credential)"
	else
		echo "  claude CLI:  the stand-in $STANDIN_IMAGE (fixed answers, no cost)"
	fi
	echo "  Git server:  git://$GIT_HOST:9418/<repo>.git"
	echo "  Client:      eval \"\$(make -s local-env)\" && openfactory factory list"
	echo "  Checks:      make local-check     State: make local-status     Stop: make local-down"
	if [ -n "$KIND_CLUSTER" ]; then
		echo "  kind:        the cluster $KIND_CLUSTER stays after make local-down (kind delete cluster --name $KIND_CLUSTER)"
	fi
	if [ "$LOCAL_MODEL" = claude ]; then
		echo "  Claude:      openfactory secret set CLAUDE_OAUTH_TOKEN --type anthropic_oauth_token"
	fi
	if [ "$LOCAL_FORGE" = github ]; then
		echo "  GitHub:      openfactory secret set GITHUB_TOKEN   (repository $LOCAL_GITHUB_REPO)"
	fi
}

step_images
step_emulator
step_provision
step_infra
if [ "$LOCAL_SERVER_MODE" = host ]; then
	step_server_host
else
	step_server_chart
fi
step_bootstrap
step_files
summary
