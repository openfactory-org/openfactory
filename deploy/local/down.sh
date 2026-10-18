#!/bin/sh
# Stop the local test stack: stop the host-mode servers, delete the namespaces
# of-local-sandboxes, of-local, of-local-infra and aws-local. All AWS state, the
# database and the runs are gone. $LOCAL_DIR stays (the CA, the secrets and the
# provisioning files), except the files of the deleted state: env and outputs.json.
set -eu
LOCAL_USE_SAVED=1
. "$(dirname "$0")/lib.sh"
cd "$LOCAL_REPO"

have kubectl || die "kubectl is not on the PATH"

sh deploy/local/host.sh stop
log "delete the namespaces $NS_SANDBOXES, $NS_SERVER, $NS_INFRA"
k delete namespace "$NS_SANDBOXES" "$NS_SERVER" "$NS_INFRA" --ignore-not-found
log "delete the namespace $NS_AWS"
KUBE_CONTEXT=$KUBE_CONTEXT sh deploy/aws-local/down.sh
# The API key and the provisioned names belong to the state that is gone.
rm -f "$LOCAL_DIR/env" "$LOCAL_DIR/outputs.json"
echo "The local stack is stopped. $LOCAL_DIR keeps the CA and the secrets."
