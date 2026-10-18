#!/bin/sh
# Start the local AWS stack in a Kubernetes cluster: the AWS emulator behind a
# TLS front, reachable as https://$AWS_LOCAL_HOST:9443 from the workstation and
# from Pods. Test only. NOT FOR PRODUCTION.
#
#   sh deploy/aws-local/up.sh
#
# Settings (environment variables):
#   KUBE_CONTEXT     kubectl context. Default: orbstack. For kind-<name>, the
#                    script loads the image into kind and gives the Service the
#                    node port 30943 (deploy/local/kind.yaml maps it to 9443).
#   AWS_LOCAL_HOST   Host name of the stack. It must resolve to the Service on
#                    the workstation and in Pods. Default: aws.k8s.orb.local.
#   AWS_LOCAL_DIR    Where the CA, keys and env files go. Default: ~/.aws/emulator/aws-local.
#   AWS_LOCAL_BUCKET Bucket for run data. Default: openfactory. Set it to the
#                    empty string to make no bucket (make local-up does that: the
#                    provisioning script makes the bucket).
#   AWS_LOCAL_BUILD  0 uses the image of-local/aws-emulator when it exists. Default: 1 (build it).
#   AWS_LOCAL_KEEP   1 keeps a running emulator (and its state) when the image and the
#                    certificate did not change. Default: 0, which always restarts it.
#   SANDBOX_NAMESPACE  If set, the CA goes to the ConfigMap aws-local-ca there.
#
# Output in $AWS_LOCAL_DIR:
#   ca.crt        CA of the TLS front (new only when missing).
#   connect.env   For the AWS CLI: . it in a subshell.
#   server.env    OF_OBJECT_STORE_URL, AWS_CA_BUNDLE and the emulator keys for the server.
#   pod-template.yaml  Mounts the CA in sandbox Pods (SSL_CERT_DIR).
# The emulator keeps state in memory: a restart empties it and makes new keys.
set -eu
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
CTX=${KUBE_CONTEXT:-orbstack}
HOST=${AWS_LOCAL_HOST:-aws.k8s.orb.local}
DIR=${AWS_LOCAL_DIR:-$HOME/.aws/emulator/aws-local}
BUCKET=${AWS_LOCAL_BUCKET-openfactory}
KEEP=${AWS_LOCAL_KEEP:-0}
IMAGE=of-local/aws-emulator:latest
K="kubectl --context $CTX"
case $CTX in kind-*) KIND_CLUSTER=${CTX#kind-} ;; *) KIND_CLUSTER= ;; esac

mkdir -p "$DIR" && chmod 700 "$DIR"

if [ "${AWS_LOCAL_BUILD:-1}" = 0 ] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "== use the existing image $IMAGE"
else
  echo "== build $IMAGE"
  ctx=$(mktemp -d)
  trap 'rm -rf "$ctx"' EXIT
  arch=$(docker version --format '{{.Server.Arch}}')
  (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$ctx/tlsfront" ./deploy/aws-local/tlsfront)
  # The Pod Identity broker is a second binary of the image, when the Dockerfile copies it.
  if grep -q '^COPY podidentity ' Dockerfile; then
    (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$ctx/podidentity" ./deploy/aws-local/podidentity)
  fi
  cp Dockerfile entrypoint.sh "$ctx/"
  docker build -q --pull -t "$IMAGE" "$ctx" >/dev/null
fi
if [ -n "$KIND_CLUSTER" ]; then
  kind load docker-image "$IMAGE" --name "$KIND_CLUSTER" >/dev/null
fi

CERT_NEW=0
if [ ! -f "$DIR/ca.crt" ] || [ ! -f "$DIR/tls.crt" ] || ! openssl x509 -in "$DIR/tls.crt" -noout -checkhost "$HOST" | grep -q 'does match'; then
  echo "== make the CA and the certificate for $HOST"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
    -subj "/CN=Open Factory local AWS CA" -keyout "$DIR/ca.key" -out "$DIR/ca.crt" 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$HOST" \
    -keyout "$DIR/tls.key" -out "$DIR/tls.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$HOST" > "$DIR/tls.ext"
  openssl x509 -req -in "$DIR/tls.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial \
    -days 825 -extfile "$DIR/tls.ext" -out "$DIR/tls.crt" 2>/dev/null
  chmod 600 "$DIR"/*.key
  CERT_NEW=1
fi

echo "== deploy to $CTX"
$K apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Namespace
metadata: {name: aws-local}
YAML
$K -n aws-local create secret tls aws-local-tls --cert="$DIR/tls.crt" --key="$DIR/tls.key" \
  --dry-run=client -o yaml | $K apply -f - >/dev/null
sed "s/__HOST__/$HOST/g" k8s.yaml | $K apply -f - >/dev/null
if [ -n "$KIND_CLUSTER" ]; then
  $K -n aws-local patch svc aws-emulator --type json \
    -p '[{"op":"replace","path":"/spec/ports/0/nodePort","value":30943}]' >/dev/null
fi
if [ "$KEEP" = 1 ] && [ "$CERT_NEW" = 0 ]; then
  # The annotation holds the image id. The Pod restarts only when the image changes.
  id=$(docker image inspect --format '{{.Id}}' "$IMAGE" | sed 's/^sha256://' | cut -c1-12)
  $K -n aws-local patch deploy/aws-emulator --type merge \
    -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"of-local/image-id\":\"$id\"}}}}}" >/dev/null
else
  $K -n aws-local rollout restart deploy/aws-emulator >/dev/null
fi
$K -n aws-local rollout status deploy/aws-emulator --timeout=180s >/dev/null

echo "== wait for https://$HOST:9443"
i=0
until curl -sf --cacert "$DIR/ca.crt" "https://$HOST:9443/__admin/health" >/dev/null; do
  i=$((i+1)); [ $i -lt 60 ] || { echo "the stack is not reachable at https://$HOST:9443"; exit 1; }
  sleep 1
done

echo "== read this run's keys"
$K -n aws-local exec deploy/aws-emulator -- cat /home/emulator/.aws/emulator/9297/emulator_credentials > "$DIR/credentials"
chmod 600 "$DIR/credentials"
key=$(sed -n 's/^aws_access_key_id *= *//p' "$DIR/credentials")
secret=$(sed -n 's/^aws_secret_access_key *= *//p' "$DIR/credentials")

cat > "$DIR/config" <<CFG
[default]
region = us-east-1
endpoint_url = https://$HOST:9443
ca_bundle = $DIR/ca.crt
s3 =
  signature_version = s3v4
  addressing_style = path
CFG
cat > "$DIR/connect.env" <<ENV
# Local AWS stack (test only). Use in a subshell: ( . $DIR/connect.env && aws ... )
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE AWS_DEFAULT_PROFILE \\
  AWS_WEB_IDENTITY_TOKEN_FILE AWS_ROLE_ARN AWS_ROLE_SESSION_NAME AWS_CONTAINER_CREDENTIALS_RELATIVE_URI \\
  AWS_CONTAINER_CREDENTIALS_FULL_URI AWS_CONTAINER_AUTHORIZATION_TOKEN AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
export AWS_ENDPOINT_URL=https://$HOST:9443
export AWS_SHARED_CREDENTIALS_FILE=$DIR/credentials
export AWS_CONFIG_FILE=$DIR/config
export AWS_CA_BUNDLE=$DIR/ca.crt
export AWS_EC2_METADATA_DISABLED=true
export AWS_REGION=us-east-1 AWS_DEFAULT_REGION=us-east-1
export AWS_PAGER=""
ENV

# shellcheck source=/dev/null
acct=$( . "$DIR/connect.env" && aws sts get-caller-identity --query Account --output text)
if [ "$acct" != "000000000000" ]; then
  echo "get-caller-identity returned account $acct, not the emulator's 000000000000: stopping"
  exit 1
fi
if [ -n "$BUCKET" ]; then
  # shellcheck source=/dev/null
  ( . "$DIR/connect.env" && aws s3api create-bucket --bucket "$BUCKET" >/dev/null )
fi

umask 077
{
echo "# Object storage of a local Open Factory server on the local AWS stack (test only)."
if [ -n "$BUCKET" ]; then
  echo "export OF_OBJECT_STORE_URL='s3://$BUCKET/dev?region=us-east-1&endpoint=https://$HOST:9443&path_style=true'"
fi
cat <<ENV
export AWS_CA_BUNDLE=$DIR/ca.crt
export AWS_ACCESS_KEY_ID=$key
export AWS_SECRET_ACCESS_KEY=$secret
export AWS_REGION=us-east-1
ENV
} > "$DIR/server.env"
cat > "$DIR/pod-template.yaml" <<YAML
# Merge into OF_K8S_POD_TEMPLATE: sandbox Pods trust the local AWS stack's CA.
volumes:
  - name: aws-local-ca
    configMap: {name: aws-local-ca}
containers:
  - name: sandbox
    env:
      - {name: SSL_CERT_DIR, value: /etc/ssl/certs:/etc/aws-local-ca}
    volumeMounts:
      - {name: aws-local-ca, mountPath: /etc/aws-local-ca, readOnly: true}
YAML
if [ -n "${SANDBOX_NAMESPACE:-}" ]; then
  $K create namespace "$SANDBOX_NAMESPACE" --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K -n "$SANDBOX_NAMESPACE" create configmap aws-local-ca --from-file=ca.crt="$DIR/ca.crt" \
    --dry-run=client -o yaml | $K apply -f - >/dev/null
fi

echo "Local AWS stack ready at https://$HOST:9443 (account 000000000000${BUCKET:+, bucket $BUCKET})."
echo "  AWS CLI:  ( . $DIR/connect.env && aws s3 ls )"
echo "  Server:   . $DIR/server.env before openfactory serve"
echo "Emulator telemetry is off in this stack."
