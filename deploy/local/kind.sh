#!/bin/sh
# kind support of the local stack. up.sh sources it after lib.sh when
# KUBE_CONTEXT is kind-<name>. It runs nothing by itself. Test only.
#
# kind has no LoadBalancer and no shared host names, so the stack does this:
#
#   - The cluster comes from deploy/local/kind.yaml, with Calico (the default
#     network plugin of kind does not enforce NetworkPolicy).
#   - Each Service gets a fixed node port. The kind node maps it to the usual
#     port on 127.0.0.1 of the workstation (80, 9443, 5432, 9418).
#   - On the workstation, the default domain 127.0.0.1.nip.io makes every name
#     resolve to 127.0.0.1 through public DNS. Without it, add the names to
#     /etc/hosts (kind_hosts_lines prints the lines).
#   - In Pods, CoreDNS answers the same names with the ClusterIP of each Service
#     (a hosts block in its Corefile). The ports are the same, so the server,
#     the sandbox Pods, the CLI and the browser use the same URLs.

CALICO_VERSION=${CALICO_VERSION:-v3.28.2}

# kind_node_port SERVICE  The fixed node port of a Service of the stack.
kind_node_port() {
	case $1 in
	"$RELEASE") echo 30080 ;;
	aws-emulator) echo 30943 ;;
	postgres) echo 30432 ;;
	git) echo 30418 ;;
	*) die "no node port for the Service $1" ;;
	esac
}

# kind_expose NAMESPACE SERVICE  Give the first port of the Service its fixed node port.
kind_expose() {
	_port=$(kind_node_port "$2")
	_cur=$(k -n "$1" get svc "$2" -o jsonpath='{.spec.ports[0].nodePort}')
	[ "$_cur" = "$_port" ] && return 0
	k -n "$1" patch svc "$2" --type json \
		-p "[{\"op\":\"replace\",\"path\":\"/spec/ports/0/nodePort\",\"value\":$_port}]" >/dev/null
}

# kind_hosts_lines  Print the /etc/hosts lines for a domain that does not resolve to 127.0.0.1.
kind_hosts_lines() {
	echo "127.0.0.1 $AWS_HOST $DB_HOST $GIT_HOST $SERVER_HOST"
}

# kind_check_names  Stop when a name of the stack does not resolve on the workstation.
kind_check_names() {
	for _n in "$AWS_HOST" "$SERVER_HOST"; do
		if have getent; then
			_ip=$(getent hosts "$_n" | awk '{print $1; exit}')
		elif have dscacheutil; then
			_ip=$(dscacheutil -q host -a name "$_n" | awk '/^ip_address:/{print $2; exit}')
		else
			return 0
		fi
		[ "$_ip" = 127.0.0.1 ] || die "$_n resolves to '${_ip:-nothing}', not 127.0.0.1. Add this line to /etc/hosts: $(kind_hosts_lines)"
	done
}

# kind_ensure_cluster  Create the kind cluster and install Calico when necessary.
kind_ensure_cluster() {
	have kind || die "KUBE_CONTEXT=$KUBE_CONTEXT names a kind cluster, but kind is not on the PATH"
	if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
		note "use the kind cluster $KIND_CLUSTER"
	else
		log "create the kind cluster $KIND_CLUSTER from deploy/local/kind.yaml"
		# kind makes the new cluster the current kubectl context. The scripts use
		# --context only, so keep the current context of the user.
		_prev_ctx=$(kubectl config current-context 2>/dev/null || true)
		kind create cluster --name "$KIND_CLUSTER" --config "$LOCAL_REPO/deploy/local/kind.yaml"
		[ -z "$_prev_ctx" ] || kubectl config use-context "$_prev_ctx" >/dev/null
	fi
	if ! k -n kube-system get daemonset calico-node >/dev/null 2>&1; then
		log "install Calico $CALICO_VERSION (a network plugin that enforces NetworkPolicy)"
		k apply -f "https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml" >/dev/null
	fi
	k -n kube-system rollout status daemonset/calico-node --timeout=300s
	k wait --for=condition=Ready nodes --all --timeout=300s >/dev/null
}

# kind_load REF...  Load local images into the kind nodes. A pulled image (for
# example postgres:16) is an index of all platforms, but the local Docker has
# the layers of one platform only. "kind load docker-image" then fails with
# "content digest ... not found". So save the platform of the node only.
kind_load() {
	_platform=$(docker version -f '{{.Server.Os}}/{{.Server.Arch}}')
	_tar=$(mktemp "$LOCAL_DIR/kind-image.XXXXXX")
	for _ref in "$@"; do
		docker save --platform "$_platform" -o "$_tar" "$_ref"
		kind load image-archive "$_tar" --name "$KIND_CLUSTER" >/dev/null
	done
	rm -f "$_tar"
}

# kind_dns  Make CoreDNS answer each name of the stack with the ClusterIP of its
# Service. Only the Services that exist get a line. CoreDNS restarts on a change.
kind_dns() {
	OF_KIND_HOSTS=
	for _spec in "aws-local aws-emulator $AWS_HOST" "$NS_INFRA postgres $DB_HOST" \
		"$NS_INFRA git $GIT_HOST" "$NS_SERVER $RELEASE $SERVER_HOST"; do
		# shellcheck disable=SC2086
		set -- $_spec
		_ip=$(k -n "$1" get svc "$2" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
		case $_ip in "" | None) continue ;; esac
		OF_KIND_HOSTS="$OF_KIND_HOSTS        $_ip $3
"
	done
	export OF_KIND_HOSTS
	_old=$(k -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
	# Remove the block of an earlier run, then add the new one in the server block .:53.
	_new=$(printf '%s\n' "$_old" | awk '
		/# of-local begin/ { skip = 1; next }
		/# of-local end/ { skip = 0; next }
		skip { next }
		{ print }
		/^\.:53 *\{/ && ENVIRON["OF_KIND_HOSTS"] != "" {
			print "    # of-local begin: names of the local stack (deploy/local/kind.sh)"
			print "    hosts {"
			printf "%s", ENVIRON["OF_KIND_HOSTS"]
			print "        ttl 30"
			print "        fallthrough"
			print "    }"
			print "    # of-local end"
		}')
	[ "$_new" != "$_old" ] || return 0
	k -n kube-system patch configmap coredns --type merge \
		-p "$(jq -n --arg c "$_new" '{data: {Corefile: $c}}')" >/dev/null
	k -n kube-system rollout restart deploy/coredns >/dev/null
	k -n kube-system rollout status deploy/coredns --timeout=120s >/dev/null
	note "CoreDNS answers the names of the stack with the Service addresses"
}
