#!/bin/sh
# Remove the local AWS stack. Its state is in memory, so this deletes all of it.
# The CA in $AWS_LOCAL_DIR stays for the next up.sh.
set -eu
kubectl --context "${KUBE_CONTEXT:-orbstack}" delete namespace aws-local --ignore-not-found
kubectl --context "${KUBE_CONTEXT:-orbstack}" delete clusterrolebinding aws-local-emulator-auth-delegator --ignore-not-found
