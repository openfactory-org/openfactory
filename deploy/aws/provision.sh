#!/bin/sh
# Create the AWS resources of an Open Factory install, in the AWS emulator
# (the local stack) or in a real AWS account. The calls and the policy documents
# are the same for both targets. Only the endpoint and the credentials differ.
#
#   AWS_TARGET=emulator sh deploy/aws/provision.sh
#   AWS_TARGET=aws AWS_PROFILE=<profile> AWS_REGION=<region> sh deploy/aws/provision.sh
#
# Resources (names come from PREFIX):
#   VPC 10.0.0.0/16 tagged Name=PREFIX, and two subnets in two Availability Zones
#   IAM role PREFIX-cluster   trusts eks.amazonaws.com; AmazonEKSClusterPolicy attached
#   IAM role PREFIX-server    trusts pods.eks.amazonaws.com (sts:AssumeRole, sts:TagSession);
#                             inline policy PREFIX-runs-access: s3:GetObject, s3:PutObject,
#                             s3:DeleteObject on the bucket objects, s3:ListBucket on the bucket
#   EKS cluster PREFIX        waits until ACTIVE
#   Pod Identity association  cluster PREFIX, SERVER_NAMESPACE / SERVER_SERVICE_ACCOUNT -> server role
#   S3 bucket PREFIX-runs     Block Public Access (all four settings) and default encryption SSE-S3
#   ECR repositories          openfactory, tool-claude, tool-codex, sandbox
#   RDS instance PREFIX       PostgreSQL 16, db.t4g.micro, 20 GB, in a DB subnet group PREFIX
#
# The script never deletes a resource and never changes one that exists. For each
# resource it looks it up first. A resource that exists and matches is reused
# ("no change"). A resource that exists and does not match stops the script with a
# message that names the resource and the difference ("FAILED", exit status 1).
# Only a part that is missing from a resource that matches is added (for example
# the policy attachment of a role that the script made). A second run changes
# nothing and writes the same outputs file.
#
# Settings (environment variables):
#   AWS_TARGET              emulator or aws. Required.
#   PREFIX                  Name prefix of the resources. Default: of-local.
#   AWS_REGION              Region. Default: us-east-1.
#   SERVER_NAMESPACE        Namespace of the server Pods. Default: of-local.
#   SERVER_SERVICE_ACCOUNT  Service account of the server Pods. Default: openfactory.
#   OUTPUTS                 Outputs file. Default: $LOCAL_DIR/outputs.json.
#   LOCAL_DIR               Default: ~/.openfactory-local.
#   AWS_LOCAL_DIR           Target emulator: the directory with connect.env (made by
#                           deploy/aws-local/up.sh). The script sources it. It removes
#                           real AWS credentials from the script and points the AWS CLI
#                           at the emulator. Default: $LOCAL_DIR/aws.
#   ASSUME_YES              Target aws: 1 skips the question before the script creates
#                           resources that cost money.
#   RDS_ENGINE_VERSION      PostgreSQL 16 version for a new instance. Default: the newest
#                           16.x that the Region offers.
#
# Target emulator: the account of the CLI must be 000000000000, or the script stops.
# Target aws: the script refuses the account 000000000000. It prints the resources that
# cost money (EKS cluster, RDS instance, VPC) and asks y/N on the terminal. The RDS
# instance uses a master password that RDS keeps in AWS Secrets Manager. The script
# makes no node group, no DNS name and no internet route: see docs/self-hosting/kubernetes.md.
#
# Output: one line per resource on stderr (created, exists, no change or FAILED), and
# the path of the outputs file on stdout. The outputs file is JSON, mode 0644, written
# in one step: target, region, accountId, prefix, vpcId, subnetIds, clusterName,
# clusterRoleArn, serverRoleArn, podIdentityAssociationId, serverNamespace,
# serverServiceAccount, bucket, ecrRepositories, rdsInstance, rdsEndpoint. It has no secret.
#
# Needs: the AWS CLI version 2 and jq.

set -eu

case ${1:-} in
-h | --help)
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
	exit 0
	;;
'') ;;
*)
	printf 'error: unknown argument: %s (try --help)\n' "$1" >&2
	exit 1
	;;
esac

TARGET=${AWS_TARGET:-}
PREFIX=${PREFIX:-of-local}
REGION=${AWS_REGION:-us-east-1}
SERVER_NAMESPACE=${SERVER_NAMESPACE:-of-local}
SERVER_SERVICE_ACCOUNT=${SERVER_SERVICE_ACCOUNT:-openfactory}
LOCAL_DIR=${LOCAL_DIR:-$HOME/.openfactory-local}
OUTPUTS=${OUTPUTS:-$LOCAL_DIR/outputs.json}
AWS_LOCAL_DIR=${AWS_LOCAL_DIR:-$LOCAL_DIR/aws}
ASSUME_YES=${ASSUME_YES:-0}
RDS_ENGINE_VERSION=${RDS_ENGINE_VERSION:-}

EMULATOR_ACCOUNT=000000000000
VPC_CIDR=10.0.0.0/16
SUBNET_CIDRS="10.0.0.0/24 10.0.1.0/24"
CLUSTER_POLICY_ARN=arn:aws:iam::aws:policy/AmazonEKSClusterPolicy
ECR_REPOSITORIES="openfactory tool-claude tool-codex sandbox"
RDS_CLASS=db.t4g.micro
RDS_STORAGE=20

ERRF=
TMP_OUT=
cleanup() {
	[ -z "$ERRF" ] || rm -f "$ERRF"
	[ -z "$TMP_OUT" ] || rm -f "$TMP_OUT"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------- messages ----------

log() { printf '%s\n' "$*" >&2; }
report() { printf '%-9s %-38s %s\n' "$1" "$2" "${3:-}" >&2; }
fail() {
	report FAILED "$1" "$2"
	log "The script deleted nothing and did not change the resource above. Fix the difference yourself, or use another PREFIX."
	exit 1
}
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# ---------- AWS calls ----------

# call runs the AWS CLI. It sets OUT (stdout) and RC (exit status), and keeps
# stderr in the file ERRF. It never exits.
call() {
	RC=0
	OUT=$(aws "$@" 2>"$ERRF") || RC=$?
}
# is_err PATTERN: the last call failed with an error that matches PATTERN.
is_err() { grep -Eq "$1" "$ERRF"; }
# awsfail RESOURCE: stop, with the first lines of the error of the last call.
awsfail() {
	msg=$(sed -n '1,3p' "$ERRF" | tr '\n' ' ')
	fail "$1" "the AWS call failed: ${msg:-exit status $RC}"
}
# jqs JSON FILTER [jq options]: run jq on a string.
jqs() {
	_j=$1
	shift
	printf '%s' "$_j" | jq "$@"
}

# ---------- checks of the environment ----------

command -v aws >/dev/null 2>&1 || die "the AWS CLI is not on the PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on the PATH"
aws --version 2>&1 | grep -q '^aws-cli/2\.' || die "the AWS CLI version 2 is necessary"

case $TARGET in
emulator | aws) ;;
*) die "set AWS_TARGET to emulator or aws" ;;
esac
printf '%s' "$PREFIX" | grep -Eq '^[a-z][a-z0-9]*(-[a-z0-9]+)*$' && [ ${#PREFIX} -le 40 ] ||
	die "PREFIX must be 1-40 lowercase letters, digits and single hyphens, and start with a letter (got: $PREFIX)"
printf '%s' "$SERVER_NAMESPACE" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' ||
	die "SERVER_NAMESPACE is not a Kubernetes name (got: $SERVER_NAMESPACE)"
printf '%s' "$SERVER_SERVICE_ACCOUNT" | grep -Eq '^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$' ||
	die "SERVER_SERVICE_ACCOUNT is not a Kubernetes name (got: $SERVER_SERVICE_ACCOUNT)"

ERRF=$(mktemp)

if [ "$TARGET" = emulator ]; then
	[ -f "$AWS_LOCAL_DIR/connect.env" ] ||
		die "$AWS_LOCAL_DIR/connect.env does not exist: start the emulator with deploy/aws-local/up.sh, or set AWS_LOCAL_DIR"
	# connect.env unsets the real AWS credentials and points the CLI at the emulator.
	# shellcheck disable=SC1091
	. "$AWS_LOCAL_DIR/connect.env"
	[ -n "${AWS_ENDPOINT_URL:-}" ] || die "$AWS_LOCAL_DIR/connect.env does not set AWS_ENDPOINT_URL: the CLI would call real AWS"
	[ -z "${AWS_ACCESS_KEY_ID:-}" ] || die "AWS_ACCESS_KEY_ID is set after connect.env: stopping, so that real keys stay out of emulator calls"
else
	# A leftover emulator endpoint must not decide where a real run goes.
	unset AWS_ENDPOINT_URL
fi
export AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION" AWS_PAGER=""

call sts get-caller-identity --output json
[ "$RC" -eq 0 ] || awsfail "aws sts get-caller-identity"
ACCOUNT=$(jqs "$OUT" -r '.Account')
if [ "$TARGET" = emulator ]; then
	[ "$ACCOUNT" = "$EMULATOR_ACCOUNT" ] ||
		die "get-caller-identity returned account $ACCOUNT, not the emulator's $EMULATOR_ACCOUNT: stopping"
else
	[ "$ACCOUNT" != "$EMULATOR_ACCOUNT" ] ||
		die "target aws, but the account is $EMULATOR_ACCOUNT (the emulator): stopping. Check AWS_PROFILE and the AWS CLI configuration."
	log "Target aws: account $ACCOUNT, Region $REGION, prefix $PREFIX."
	log "This script creates resources that cost money:"
	log "  - EKS cluster $PREFIX (control plane fee per hour)"
	log "  - RDS instance $PREFIX ($RDS_CLASS, ${RDS_STORAGE} GB, PostgreSQL 16; billed per hour and per GB)"
	log "  - VPC $PREFIX ($VPC_CIDR, two subnets; no NAT gateway, no charge by itself)"
	log "It also creates IAM roles, an S3 bucket and ECR repositories (small charges for what you store)."
	if [ "$ASSUME_YES" != 1 ]; then
		printf 'Create them in account %s? [y/N] ' "$ACCOUNT" >&2
		ans=
		# Read from the terminal, also when stdin is a pipe.
		if ! read -r ans </dev/tty 2>/dev/null; then
			die "no terminal to ask on: set ASSUME_YES=1 to continue without the question"
		fi
		case $ans in
		y | Y | yes | YES) ;;
		*)
			log "Stopped: no resource was created."
			exit 1
			;;
		esac
	fi
fi
if [ "$TARGET" = emulator ]; then
	WAIT_CLUSTER=120
	WAIT_RDS=60
else
	WAIT_CLUSTER=1800
	WAIT_RDS=3600
fi

BUCKET="$PREFIX-runs"
CLUSTER_ROLE="$PREFIX-cluster"
SERVER_ROLE="$PREFIX-server"
SERVER_POLICY="$PREFIX-runs-access"

log "Provisioning prefix $PREFIX, Region $REGION, account $ACCOUNT (target $TARGET)."

# ---------- VPC and subnets ----------

VPC_ID=
ensure_vpc() {
	res="vpc $PREFIX"
	call ec2 describe-vpcs --filters "Name=tag:Name,Values=$PREFIX" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	n=$(jqs "$OUT" '.Vpcs | length')
	if [ "$n" -gt 1 ]; then
		fail "$res" "$n VPCs have the tag Name=$PREFIX; expected at most one"
	elif [ "$n" -eq 1 ]; then
		cidr=$(jqs "$OUT" -r '.Vpcs[0].CidrBlock')
		[ "$cidr" = "$VPC_CIDR" ] || fail "$res" "CIDR block is $cidr, expected $VPC_CIDR"
		VPC_ID=$(jqs "$OUT" -r '.Vpcs[0].VpcId')
		report "no change" "$res" "$VPC_ID"
		return
	fi
	call ec2 create-vpc --cidr-block "$VPC_CIDR" \
		--tag-specifications "ResourceType=vpc,Tags=[{Key=Name,Value=$PREFIX}]" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	VPC_ID=$(jqs "$OUT" -r '.Vpc.VpcId')
	call ec2 wait vpc-available --vpc-ids "$VPC_ID"
	[ "$RC" -eq 0 ] || awsfail "$res"
	# EKS needs DNS support and DNS host names in the VPC.
	call ec2 modify-vpc-attribute --vpc-id "$VPC_ID" --enable-dns-support '{"Value":true}'
	[ "$RC" -eq 0 ] || awsfail "$res (DNS support)"
	call ec2 modify-vpc-attribute --vpc-id "$VPC_ID" --enable-dns-hostnames '{"Value":true}'
	[ "$RC" -eq 0 ] || awsfail "$res (DNS host names)"
	report created "$res" "$VPC_ID"
}

# ensure_subnet AZ CIDR: sets SUBNET_ID.
SUBNET_ID=
ensure_subnet() {
	az=$1
	cidr=$2
	name="$PREFIX-$az"
	res="subnet $name"
	call ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:Name,Values=$name" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	n=$(jqs "$OUT" '.Subnets | length')
	if [ "$n" -gt 1 ]; then
		fail "$res" "$n subnets in $VPC_ID have the tag Name=$name; expected at most one"
	elif [ "$n" -eq 1 ]; then
		got_az=$(jqs "$OUT" -r '.Subnets[0].AvailabilityZone')
		got_cidr=$(jqs "$OUT" -r '.Subnets[0].CidrBlock')
		[ "$got_az" = "$az" ] || fail "$res" "Availability Zone is $got_az, expected $az"
		[ "$got_cidr" = "$cidr" ] || fail "$res" "CIDR block is $got_cidr, expected $cidr"
		SUBNET_ID=$(jqs "$OUT" -r '.Subnets[0].SubnetId')
		report "no change" "$res" "$SUBNET_ID"
		return
	fi
	call ec2 create-subnet --vpc-id "$VPC_ID" --cidr-block "$cidr" --availability-zone "$az" \
		--tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=$name}]" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	SUBNET_ID=$(jqs "$OUT" -r '.Subnet.SubnetId')
	report created "$res" "$SUBNET_ID"
}

# ---------- IAM ----------

# ensure_role NAME SERVICE ACTIONS_JSON: sets ROLE_ARN. Reports one line.
ROLE_ARN=
ROLE_NEW=0
ensure_role() {
	name=$1
	svc=$2
	acts=$3
	res="iam role $name"
	trust=$(jq -n -c --arg svc "$svc" --argjson acts "$acts" \
		'{Version: "2012-10-17", Statement: [{Effect: "Allow", Principal: {Service: $svc}, Action: $acts}]}')
	call iam get-role --role-name "$name" --output json
	if [ "$RC" -eq 0 ]; then
		# The trust policy must allow exactly this service, and at least these actions.
		# shellcheck disable=SC2016 # the $names are jq variables; they expand later, not here
		diff=$(jqs "$OUT" -r --arg svc "$svc" --argjson acts "$acts" '
			.Role.AssumeRolePolicyDocument
			| (if type == "string" then fromjson else . end).Statement
			| (if type == "array" then . else [.] end)
			| map(select(.Effect == "Allow")) as $s
			| ([$s[].Principal | if type == "object" then to_entries[] | .key as $k | .value | (if type == "array" then .[] else . end) | "\($k):\(.)" else "\(.)" end] | unique) as $p
			| ([$s[].Action | if type == "array" then .[] else . end] | unique) as $a
			| if $p != ["Service:" + $svc] then "trust policy principals are \($p | join(",")), expected Service:\($svc)"
			  elif ($acts | map(IN($a[])) | all | not) then "trust policy actions are \($a | join(",")), expected \($acts | join(","))"
			  else "" end')
		[ -z "$diff" ] || fail "$res" "$diff"
		ROLE_ARN=$(jqs "$OUT" -r '.Role.Arn')
		ROLE_NEW=0
		return
	fi
	is_err 'NoSuchEntity' || awsfail "$res"
	call iam create-role --role-name "$name" --assume-role-policy-document "$trust" \
		--description "Open Factory install $PREFIX" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	ROLE_ARN=$(jqs "$OUT" -r '.Role.Arn')
	ROLE_NEW=1
	if [ "$TARGET" = aws ]; then
		# IAM is eventually consistent: let EKS see the role.
		call iam wait role-exists --role-name "$name"
		sleep 10
	fi
}

ensure_cluster_role() {
	res="iam role $CLUSTER_ROLE"
	ensure_role "$CLUSTER_ROLE" eks.amazonaws.com '["sts:AssumeRole"]'
	CLUSTER_ROLE_ARN=$ROLE_ARN
	if [ "$ROLE_NEW" = 1 ]; then
		report created "$res" "$CLUSTER_ROLE_ARN"
	else
		report "no change" "$res" "$CLUSTER_ROLE_ARN"
	fi
	res="policy attachment $CLUSTER_ROLE"
	call iam list-attached-role-policies --role-name "$CLUSTER_ROLE" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	# shellcheck disable=SC2016 # the $names are jq variables; they expand later, not here
	if jqs "$OUT" -e --arg a "$CLUSTER_POLICY_ARN" '.AttachedPolicies | map(.PolicyArn) | index($a)' >/dev/null; then
		report "no change" "$res" "AmazonEKSClusterPolicy"
		return
	fi
	call iam attach-role-policy --role-name "$CLUSTER_ROLE" --policy-arn "$CLUSTER_POLICY_ARN"
	if [ "$RC" -eq 0 ]; then
		report created "$res" "AmazonEKSClusterPolicy"
	elif [ "$TARGET" = emulator ]; then
		report skipped "$res" "the emulator refused the AWS managed policy: $(sed -n '1p' "$ERRF")"
	else
		awsfail "$res"
	fi
}

# policy_doc: the inline policy of the server role.
policy_doc() {
	jq -n -c --arg b "$BUCKET" '{
		Version: "2012-10-17",
		Statement: [
			{Sid: "RunDataObjects", Effect: "Allow",
			 Action: ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
			 Resource: "arn:aws:s3:::\($b)/*"},
			{Sid: "RunDataList", Effect: "Allow",
			 Action: "s3:ListBucket",
			 Resource: "arn:aws:s3:::\($b)"}]}'
}
# The same policy in a form that ignores the order and the Sid values.
NORM_POLICY='[.Statement | (if type == "array" then . else [.] end)[]
	| {Effect, Action: ([.Action] | flatten | sort), Resource: ([.Resource] | flatten | sort)}] | sort_by(tostring)'

ensure_server_role() {
	res="iam role $SERVER_ROLE"
	ensure_role "$SERVER_ROLE" pods.eks.amazonaws.com '["sts:AssumeRole","sts:TagSession"]'
	SERVER_ROLE_ARN=$ROLE_ARN
	if [ "$ROLE_NEW" = 1 ]; then
		report created "$res" "$SERVER_ROLE_ARN"
	else
		report "no change" "$res" "$SERVER_ROLE_ARN"
	fi
	res="iam policy $SERVER_ROLE/$SERVER_POLICY"
	want=$(policy_doc)
	call iam get-role-policy --role-name "$SERVER_ROLE" --policy-name "$SERVER_POLICY" --output json
	if [ "$RC" -eq 0 ]; then
		got_n=$(jqs "$OUT" -c ".PolicyDocument | (if type == \"string\" then fromjson else . end) | $NORM_POLICY")
		want_n=$(jqs "$want" -c "$NORM_POLICY")
		[ "$got_n" = "$want_n" ] ||
			fail "$res" "the policy differs. Expected statements $want_n, found $got_n"
		report "no change" "$res" "S3 on $BUCKET"
		return
	fi
	is_err 'NoSuchEntity' || awsfail "$res"
	call iam put-role-policy --role-name "$SERVER_ROLE" --policy-name "$SERVER_POLICY" --policy-document "$want"
	[ "$RC" -eq 0 ] || awsfail "$res"
	report created "$res" "S3 on $BUCKET"
}

# ---------- EKS ----------

wait_cluster_active() {
	t=0
	while :; do
		call eks describe-cluster --name "$PREFIX" --output json
		[ "$RC" -eq 0 ] || awsfail "eks cluster $PREFIX"
		st=$(jqs "$OUT" -r '.cluster.status')
		case $st in
		ACTIVE) return 0 ;;
		FAILED | DELETING) fail "eks cluster $PREFIX" "status is $st" ;;
		esac
		[ "$t" -lt "$WAIT_CLUSTER" ] || fail "eks cluster $PREFIX" "status is $st after ${t}s, not ACTIVE"
		sleep 5
		t=$((t + 5))
	done
}

ensure_cluster() {
	res="eks cluster $PREFIX"
	call eks describe-cluster --name "$PREFIX" --output json
	if [ "$RC" -eq 0 ]; then
		got=$(jqs "$OUT" -r '.cluster.roleArn')
		[ "$got" = "$CLUSTER_ROLE_ARN" ] || fail "$res" "cluster role is $got, expected $CLUSTER_ROLE_ARN"
		got=$(jqs "$OUT" -r '.cluster.resourcesVpcConfig.vpcId // empty')
		[ -z "$got" ] || [ "$got" = "$VPC_ID" ] || fail "$res" "VPC is $got, expected $VPC_ID"
		st=$(jqs "$OUT" -r '.cluster.status')
		if [ "$st" != ACTIVE ]; then
			wait_cluster_active
		fi
		report "no change" "$res" "ACTIVE"
		return
	fi
	is_err 'ResourceNotFoundException' || awsfail "$res"
	call eks create-cluster --name "$PREFIX" --role-arn "$CLUSTER_ROLE_ARN" \
		--resources-vpc-config "subnetIds=$SUBNET_1,$SUBNET_2" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	wait_cluster_active
	report created "$res" "ACTIVE"
}

ensure_association() {
	res="pod identity $SERVER_NAMESPACE/$SERVER_SERVICE_ACCOUNT"
	call eks list-pod-identity-associations --cluster-name "$PREFIX" \
		--namespace "$SERVER_NAMESPACE" --service-account "$SERVER_SERVICE_ACCOUNT" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	n=$(jqs "$OUT" '.associations | length')
	if [ "$n" -gt 1 ]; then
		fail "$res" "$n associations exist for this service account; expected at most one"
	elif [ "$n" -eq 1 ]; then
		ASSOCIATION_ID=$(jqs "$OUT" -r '.associations[0].associationId')
		call eks describe-pod-identity-association --cluster-name "$PREFIX" --association-id "$ASSOCIATION_ID" --output json
		[ "$RC" -eq 0 ] || awsfail "$res"
		got=$(jqs "$OUT" -r '.association.roleArn')
		[ "$got" = "$SERVER_ROLE_ARN" ] || fail "$res" "association $ASSOCIATION_ID uses the role $got, expected $SERVER_ROLE_ARN"
		report "no change" "$res" "$ASSOCIATION_ID -> $SERVER_ROLE"
		return
	fi
	call eks create-pod-identity-association --cluster-name "$PREFIX" \
		--namespace "$SERVER_NAMESPACE" --service-account "$SERVER_SERVICE_ACCOUNT" \
		--role-arn "$SERVER_ROLE_ARN" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	ASSOCIATION_ID=$(jqs "$OUT" -r '.association.associationId')
	report created "$res" "$ASSOCIATION_ID -> $SERVER_ROLE"
}

# ---------- S3 ----------

ensure_bucket() {
	res="s3 bucket $BUCKET"
	call s3api head-bucket --bucket "$BUCKET"
	if [ "$RC" -ne 0 ]; then
		if is_err 'NoSuchBucket|\(404\)|Not Found'; then
			if [ "$REGION" = us-east-1 ]; then
				call s3api create-bucket --bucket "$BUCKET"
			else
				call s3api create-bucket --bucket "$BUCKET" \
					--create-bucket-configuration "LocationConstraint=$REGION"
			fi
			[ "$RC" -eq 0 ] || awsfail "$res"
			call s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
				BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
			[ "$RC" -eq 0 ] || awsfail "$res (Block Public Access)"
			call s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
				'{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
			[ "$RC" -eq 0 ] || awsfail "$res (default encryption)"
			report created "$res" "Block Public Access on, SSE-S3"
			return
		elif is_err '\(403\)|Forbidden|AccessDenied'; then
			fail "$res" "a bucket with this name exists and this account cannot access it (another account owns it)"
		fi
		awsfail "$res"
	fi
	# The bucket exists: check it.
	call s3api get-bucket-location --bucket "$BUCKET" --output json
	if [ "$RC" -eq 0 ]; then
		loc=$(jqs "$OUT" -r '.LocationConstraint // "us-east-1"')
		[ "$loc" != EU ] || loc=eu-west-1
		[ "$loc" = "$REGION" ] || fail "$res" "bucket is in Region $loc, expected $REGION"
	fi
	call s3api get-public-access-block --bucket "$BUCKET" --output json
	if [ "$RC" -ne 0 ]; then
		is_err 'NoSuchPublicAccessBlockConfiguration' || awsfail "$res"
		fail "$res" "Block Public Access is not configured; expected all four settings on"
	fi
	# shellcheck disable=SC2016 # the $names are jq variables; they expand later, not here
	off=$(jqs "$OUT" -r '.PublicAccessBlockConfiguration as $c
		| ["BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"]
		| map(select($c[.] != true)) | join(",")')
	[ -z "$off" ] || fail "$res" "Block Public Access settings that are off: $off; expected all four on"
	call s3api get-bucket-encryption --bucket "$BUCKET" --output json
	if [ "$RC" -ne 0 ]; then
		is_err 'ServerSideEncryptionConfigurationNotFoundError' || awsfail "$res"
		fail "$res" "default encryption is not configured; expected SSE-S3 (AES256)"
	fi
	algs=$(jqs "$OUT" -r '[.ServerSideEncryptionConfiguration.Rules[].ApplyServerSideEncryptionByDefault.SSEAlgorithm] | unique | join(",")')
	[ "$algs" = AES256 ] || fail "$res" "default encryption is $algs; expected SSE-S3 (AES256)"
	report "no change" "$res" "Block Public Access on, SSE-S3"
}

# ---------- ECR ----------

ECR_JSON='{}'
ensure_ecr() {
	for repo in $ECR_REPOSITORIES; do
		res="ecr repository $repo"
		call ecr describe-repositories --repository-names "$repo" --output json
		if [ "$RC" -eq 0 ]; then
			uri=$(jqs "$OUT" -r '.repositories[0].repositoryUri')
			report "no change" "$res" "$uri"
		else
			is_err 'RepositoryNotFoundException' || awsfail "$res"
			call ecr create-repository --repository-name "$repo" --output json
			[ "$RC" -eq 0 ] || awsfail "$res"
			uri=$(jqs "$OUT" -r '.repository.repositoryUri')
			report created "$res" "$uri"
		fi
		# shellcheck disable=SC2016 # the $names are jq variables; they expand later, not here
		ECR_JSON=$(jqs "$ECR_JSON" -c --arg k "$repo" --arg v "$uri" '. + {($k): $v}')
	done
}

# ---------- RDS ----------

DBSG_OK=0
ensure_db_subnet_group() {
	res="rds subnet group $PREFIX"
	want=$(printf '%s\n%s\n' "$SUBNET_1" "$SUBNET_2" | sort | tr '\n' ' ')
	call rds describe-db-subnet-groups --db-subnet-group-name "$PREFIX" --output json
	if [ "$RC" -eq 0 ]; then
		got_vpc=$(jqs "$OUT" -r '.DBSubnetGroups[0].VpcId')
		[ "$got_vpc" = "$VPC_ID" ] || fail "$res" "VPC is $got_vpc, expected $VPC_ID"
		got=$(jqs "$OUT" -r '[.DBSubnetGroups[0].Subnets[].SubnetIdentifier] | sort | join("\n")' | tr '\n' ' ')
		[ "$got" = "$want" ] || fail "$res" "subnets are ${got}, expected ${want}"
		DBSG_OK=1
		report "no change" "$res" "$VPC_ID"
		return
	fi
	is_err 'DBSubnetGroupNotFound' || awsfail "$res"
	call rds create-db-subnet-group --db-subnet-group-name "$PREFIX" \
		--db-subnet-group-description "Open Factory install $PREFIX" \
		--subnet-ids "$SUBNET_1" "$SUBNET_2" --output json
	if [ "$RC" -eq 0 ]; then
		DBSG_OK=1
		report created "$res" "$VPC_ID"
	elif [ "$TARGET" = emulator ]; then
		report skipped "$res" "the emulator refused it: $(sed -n '1p' "$ERRF")"
	else
		awsfail "$res"
	fi
}

pick_engine_version() {
	[ -z "$RDS_ENGINE_VERSION" ] || return 0
	call rds describe-db-engine-versions --engine postgres --output json
	[ "$RC" -eq 0 ] || awsfail "rds engine versions"
	RDS_ENGINE_VERSION=$(jqs "$OUT" -r '[.DBEngineVersions[].EngineVersion | select(startswith("16."))
		| select(test("^16\\.[0-9]+$"))] | sort_by(split(".") | map(tonumber)) | last // empty')
	[ -n "$RDS_ENGINE_VERSION" ] || fail "rds instance $PREFIX" "the Region $REGION offers no PostgreSQL 16 version"
}

RDS_ENDPOINT=
read_rds_endpoint() {
	RDS_ENDPOINT=$(jqs "$OUT" -r '.DBInstances[0].Endpoint | if . == null then "" else "\(.Address):\(.Port)" end')
}

wait_rds() {
	t=0
	while :; do
		call rds describe-db-instances --db-instance-identifier "$PREFIX" --output json
		[ "$RC" -eq 0 ] || awsfail "rds instance $PREFIX"
		st=$(jqs "$OUT" -r '.DBInstances[0].DBInstanceStatus')
		case $st in
		available)
			read_rds_endpoint
			return 0
			;;
		failed | incompatible-* | deleting) fail "rds instance $PREFIX" "status is $st" ;;
		esac
		if [ "$t" -ge "$WAIT_RDS" ]; then
			if [ "$TARGET" = emulator ]; then
				report note "rds instance $PREFIX" "status is $st after ${t}s; the emulator keeps a record only, continuing"
				read_rds_endpoint
				return 0
			fi
			fail "rds instance $PREFIX" "status is $st after ${t}s, not available"
		fi
		sleep 5
		t=$((t + 5))
	done
}

ensure_rds() {
	res="rds instance $PREFIX"
	call rds describe-db-instances --db-instance-identifier "$PREFIX" --output json
	if [ "$RC" -eq 0 ]; then
		eng=$(jqs "$OUT" -r '.DBInstances[0].Engine')
		ver=$(jqs "$OUT" -r '.DBInstances[0].EngineVersion')
		cls=$(jqs "$OUT" -r '.DBInstances[0].DBInstanceClass')
		sto=$(jqs "$OUT" -r '.DBInstances[0].AllocatedStorage')
		[ "$eng" = postgres ] || fail "$res" "engine is $eng, expected postgres"
		case $ver in
		16.*) ;;
		*) fail "$res" "engine version is $ver, expected 16.x" ;;
		esac
		[ "$cls" = "$RDS_CLASS" ] || fail "$res" "instance class is $cls, expected $RDS_CLASS"
		[ "$sto" = "$RDS_STORAGE" ] || fail "$res" "allocated storage is $sto GB, expected $RDS_STORAGE GB"
		st=$(jqs "$OUT" -r '.DBInstances[0].DBInstanceStatus')
		[ "$st" = available ] || wait_rds
		read_rds_endpoint
		report "no change" "$res" "postgres $ver ${RDS_ENDPOINT:-no endpoint}"
		return
	fi
	is_err 'DBInstanceNotFound' || awsfail "$res"
	pick_engine_version
	set -- --db-instance-identifier "$PREFIX" --engine postgres --engine-version "$RDS_ENGINE_VERSION" \
		--db-instance-class "$RDS_CLASS" --allocated-storage "$RDS_STORAGE" --storage-type gp3 \
		--storage-encrypted --no-publicly-accessible \
		--db-name openfactory --master-username openfactory
	if [ "$DBSG_OK" = 1 ]; then
		set -- "$@" --db-subnet-group-name "$PREFIX"
	fi
	if [ "$TARGET" = aws ]; then
		# RDS makes the master password and keeps it in AWS Secrets Manager.
		set -- "$@" --manage-master-user-password
	else
		# The emulator keeps a record only: the password is not used and not kept.
		pw=$(od -An -N18 -tx1 /dev/urandom | tr -d ' \n')
		set -- "$@" --master-user-password "$pw"
	fi
	call rds create-db-instance "$@" --output json
	[ "$RC" -eq 0 ] || awsfail "$res"
	wait_rds
	report created "$res" "postgres $RDS_ENGINE_VERSION ${RDS_ENDPOINT:-no endpoint yet}"
}

# ---------- run ----------

ensure_vpc

call ec2 describe-availability-zones --filters Name=state,Values=available \
	Name=opt-in-status,Values=opt-in-not-required,opted-in --output json
[ "$RC" -eq 0 ] || awsfail "availability zones of $REGION"
ZONES=$(jqs "$OUT" -r '[.AvailabilityZones[].ZoneName] | sort | .[]')
AZ_1=$(printf '%s\n' "$ZONES" | sed -n '1p')
AZ_2=$(printf '%s\n' "$ZONES" | sed -n '2p')
[ -n "$AZ_1" ] && [ -n "$AZ_2" ] || fail "subnets of $PREFIX" "Region $REGION has fewer than two Availability Zones"
CIDR_1=${SUBNET_CIDRS%% *}
CIDR_2=${SUBNET_CIDRS##* }
ensure_subnet "$AZ_1" "$CIDR_1"
SUBNET_1=$SUBNET_ID
ensure_subnet "$AZ_2" "$CIDR_2"
SUBNET_2=$SUBNET_ID

CLUSTER_ROLE_ARN=
SERVER_ROLE_ARN=
ensure_cluster_role
ensure_server_role
ensure_cluster
ASSOCIATION_ID=
ensure_association
ensure_bucket
ensure_ecr
ensure_db_subnet_group
ensure_rds

# ---------- outputs ----------

mkdir -p "$(dirname "$OUTPUTS")"
TMP_OUT=$(mktemp "$OUTPUTS.XXXXXX")
jq -n --arg target "$TARGET" --arg region "$REGION" --arg account "$ACCOUNT" --arg prefix "$PREFIX" \
	--arg vpc "$VPC_ID" --arg s1 "$SUBNET_1" --arg s2 "$SUBNET_2" \
	--arg croleArn "$CLUSTER_ROLE_ARN" --arg sroleArn "$SERVER_ROLE_ARN" --arg assoc "$ASSOCIATION_ID" \
	--arg ns "$SERVER_NAMESPACE" --arg sa "$SERVER_SERVICE_ACCOUNT" --arg bucket "$BUCKET" \
	--argjson ecr "$ECR_JSON" --arg rds "$PREFIX" --arg rdsEndpoint "$RDS_ENDPOINT" '{
		target: $target,
		region: $region,
		accountId: $account,
		prefix: $prefix,
		vpcId: $vpc,
		subnetIds: [$s1, $s2],
		clusterName: $prefix,
		clusterRoleArn: $croleArn,
		serverRoleArn: $sroleArn,
		podIdentityAssociationId: $assoc,
		serverNamespace: $ns,
		serverServiceAccount: $sa,
		bucket: $bucket,
		ecrRepositories: $ecr,
		rdsInstance: $rds,
		rdsEndpoint: $rdsEndpoint
	}' >"$TMP_OUT"
chmod 644 "$TMP_OUT"
if [ -f "$OUTPUTS" ] && cmp -s "$TMP_OUT" "$OUTPUTS"; then
	report "no change" "outputs file" "$OUTPUTS"
else
	mv "$TMP_OUT" "$OUTPUTS"
	TMP_OUT=
	report created "outputs file" "$OUTPUTS"
fi
printf '%s\n' "$OUTPUTS"
