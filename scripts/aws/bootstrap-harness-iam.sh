#!/usr/bin/env bash
# One-time bootstrap: mint a least-privilege IAM user for a
# cloud-provisioning identity, scoped to exactly the EC2 actions needed
# to bring up/tear down a tagged node -- plus, optionally, Route53
# access scoped to one hosted zone, for cert-manager/external-dns
# running against that node. One script serves two shapes of caller:
#
#   - the ephemeral harness/aws-bringup/ test (default USER_NAME,
#     no ROUTE53_ZONE_ID)
#   - a persistent in-cluster identity for a real deployment, e.g.
#     USER_NAME=hilton-cloud-worker REGION=ap-east-1
#     ROUTE53_ZONE_ID=Z0730122KCZINH3W18MZ
#
# Requires admin-level credentials already exported in the environment
# (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_DEFAULT_REGION) with
# iam:CreateUser/CreatePolicy/CreateAccessKey. Prints the new access key
# once, to stdout — nothing is written to disk here.
set -euo pipefail

USER_NAME="${USER_NAME:-cloud-provisioning-harness}"
POLICY_NAME="${POLICY_NAME:-CloudProvisioningHarness}"
REGION="${REGION:-*}"
ROUTE53_ZONE_ID="${ROUTE53_ZONE_ID:-}"

# Least privilege here means: describe calls are unscoped (read-only,
# harmless), but every mutation that touches an EXISTING resource
# (terminate, ingress rules, delete) is conditioned on that resource
# carrying the harness's own tag — so this identity can only ever act on
# things it created. Creation calls can't be scoped by an existing
# resource's own tag, so they're instead constrained to require that
# same tag be present on the thing being created (aws:RequestTag).
#
# RunInstances and CreateSecurityGroup each touch more than one resource
# type in a single call (RunInstances: instance + subnet + network-interface
# + volume + image + key-pair + security-group; CreateSecurityGroup: the
# new security-group + the vpc it lives in) — IAM evaluates every implicated
# resource type independently, and aws:RequestTag only ever applies to the
# type actually being created/tagged. Putting a RequestTag condition on
# "Resource": "*" therefore silently fails the check for the OTHER,
# pre-existing resource types (e.g. the vpc), because that condition key
# doesn't exist in their evaluation context. So this is split into two
# statements per AWS's own documented pattern: one unconditioned Allow
# scoped to the ancillary/already-existing resource types, and one
# Allow scoped to only the newly-created resource types, tag-conditioned.
TAG_KEY="Project"
TAG_VALUE="$USER_NAME"
PARTITION_ARN="arn:aws:ec2:${REGION}:*"

# Only appended when ROUTE53_ZONE_ID is set. ChangeResourceRecordSets and
# ListResourceRecordSets are scoped to that one zone; GetChange and
# ListHostedZones(ByName) aren't zone resources in IAM's model (a change
# ID isn't a hosted zone, and zone discovery-by-name has to run before
# you have a zone ARN to scope to), so those stay unscoped -- read-only
# and harmless regardless.
ROUTE53_STATEMENTS=""
if [ -n "$ROUTE53_ZONE_ID" ]; then
  ROUTE53_STATEMENTS=$(cat <<EOF
    ,
    {
      "Sid": "Route53ZoneScoped",
      "Effect": "Allow",
      "Action": ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets"],
      "Resource": "arn:aws:route53:::hostedzone/${ROUTE53_ZONE_ID}"
    },
    {
      "Sid": "Route53Unscoped",
      "Effect": "Allow",
      "Action": ["route53:GetChange", "route53:ListHostedZones", "route53:ListHostedZonesByName"],
      "Resource": "*"
    }
EOF
  )
fi

POLICY_DOC=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DescribeUnscoped",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeImages",
        "ec2:DescribeVpcs",
        "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeKeyPairs",
        "ec2:DescribeAvailabilityZones",
        "ec2:DescribeTags",
        "ec2:DescribeDhcpOptions",
        "ec2:DescribeNetworkInterfaces"
      ],
      "Resource": "*"
    },
    {
      "Sid": "UseExistingAncillaryResources",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:CreateSecurityGroup"
      ],
      "Resource": [
        "${PARTITION_ARN}:vpc/*",
        "${PARTITION_ARN}:subnet/*",
        "${PARTITION_ARN}:network-interface/*",
        "${PARTITION_ARN}:volume/*",
        "${PARTITION_ARN}:image/*",
        "${PARTITION_ARN}:key-pair/*",
        "${PARTITION_ARN}:security-group/*"
      ]
    },
    {
      "Sid": "CreateOwnResourcesMustBeTagged",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:CreateSecurityGroup",
        "ec2:ImportKeyPair"
      ],
      "Resource": [
        "${PARTITION_ARN}:instance/*",
        "${PARTITION_ARN}:security-group/*",
        "${PARTITION_ARN}:key-pair/*"
      ],
      "Condition": {
        "StringEquals": {
          "aws:RequestTag/${TAG_KEY}": "${TAG_VALUE}"
        }
      }
    },
    {
      "Sid": "CreateTagsOnCreate",
      "Effect": "Allow",
      "Action": "ec2:CreateTags",
      "Resource": "*",
      "Condition": {
        "StringEquals": { "ec2:CreateAction": ["RunInstances", "CreateSecurityGroup", "ImportKeyPair"] }
      }
    },
    {
      "Sid": "ModifyOwnInstanceNetworkInterfaces",
      "Effect": "Allow",
      "Action": ["ec2:ModifyNetworkInterfaceAttribute"],
      "Resource": [
        "${PARTITION_ARN}:network-interface/*",
        "${PARTITION_ARN}:security-group/*"
      ],
      "Condition": {
        "StringEquals": { "ec2:ResourceTag/${TAG_KEY}": "${TAG_VALUE}" }
      }
    },
    {
      "Sid": "MutateOnlyOwnTaggedResources",
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:DeleteSecurityGroup",
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:RevokeSecurityGroupIngress",
        "ec2:DeleteKeyPair",
        "ec2:CreateTags"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": { "ec2:ResourceTag/${TAG_KEY}": "${TAG_VALUE}" }
      }
    }${ROUTE53_STATEMENTS}
  ]
}
EOF
)

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
echo "account: $ACCOUNT_ID" >&2

if aws iam get-user --user-name "$USER_NAME" >/dev/null 2>&1; then
  echo "user $USER_NAME already exists, skipping create" >&2
else
  aws iam create-user --user-name "$USER_NAME" >/dev/null
fi

# A customer-managed policy, not inline: inline user policies cap out at
# 2048 bytes, which this policy outgrew as more narrowly-scoped
# statements were added. Managed policies allow 6144.
POLICY_ARN="arn:aws:iam::${ACCOUNT_ID}:policy/${POLICY_NAME}"
if aws iam get-policy --policy-arn "$POLICY_ARN" >/dev/null 2>&1; then
  # A policy can keep at most 5 versions; drop the oldest non-default
  # one first if already at the cap, then push the new version as default.
  OLD_VERSIONS=$(aws iam list-policy-versions --policy-arn "$POLICY_ARN" \
    --query 'Versions[?!IsDefaultVersion].VersionId' --output text)
  VERSION_COUNT=$(aws iam list-policy-versions --policy-arn "$POLICY_ARN" --query 'length(Versions)' --output text)
  if [ "$VERSION_COUNT" -ge 5 ]; then
    OLDEST=$(echo "$OLD_VERSIONS" | tr '\t' '\n' | sort -t v -k2 -n | head -1)
    aws iam delete-policy-version --policy-arn "$POLICY_ARN" --version-id "$OLDEST" >/dev/null
  fi
  aws iam create-policy-version --policy-arn "$POLICY_ARN" --policy-document "$POLICY_DOC" --set-as-default >/dev/null
  echo "policy $POLICY_NAME updated (new default version)" >&2
else
  aws iam create-policy --policy-name "$POLICY_NAME" --policy-document "$POLICY_DOC" >/dev/null
  echo "policy $POLICY_NAME created" >&2
fi
aws iam attach-user-policy --user-name "$USER_NAME" --policy-arn "$POLICY_ARN"
echo "policy $POLICY_NAME attached to $USER_NAME" >&2

# Clean up an old inline policy of the same name from before this script
# switched to a managed policy, if one exists.
if aws iam get-user-policy --user-name "$USER_NAME" --policy-name "$POLICY_NAME" >/dev/null 2>&1; then
  aws iam delete-user-policy --user-name "$USER_NAME" --policy-name "$POLICY_NAME"
  echo "removed stale inline policy $POLICY_NAME" >&2
fi

EXISTING_KEYS=$(aws iam list-access-keys --user-name "$USER_NAME" --query 'length(AccessKeyMetadata)' --output text)
if [ "$EXISTING_KEYS" -ge 2 ]; then
  echo "user already has $EXISTING_KEYS access keys (AWS max is 2) -- not creating another; reuse an existing one or delete one first" >&2
else
  echo "creating access key (existing keys are left alone — delete manually if rotating)" >&2
  aws iam create-access-key --user-name "$USER_NAME"
fi
