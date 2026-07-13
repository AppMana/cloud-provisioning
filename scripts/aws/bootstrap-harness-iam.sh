#!/usr/bin/env bash
# One-time bootstrap: mint a least-privilege IAM user scoped to exactly what
# harness/aws-bringup/ needs (launch/terminate one tagged instance, manage
# its security group and key pair) so the harness never has to run under
# whatever admin credential did this bootstrap.
#
# Requires admin-level credentials already exported in the environment
# (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_DEFAULT_REGION) with
# iam:CreateUser/CreatePolicy/CreateAccessKey. Prints the new access key
# once, to stdout — nothing is written to disk here.
set -euo pipefail

USER_NAME="${USER_NAME:-cloud-provisioning-harness}"
POLICY_NAME="${POLICY_NAME:-CloudProvisioningHarness}"

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
PARTITION_ARN="arn:aws:ec2:*:*"

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
        "ec2:DescribeTags"
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
      "Sid": "MutateOnlyOwnTaggedResources",
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:DeleteSecurityGroup",
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:RevokeSecurityGroupIngress",
        "ec2:DeleteKeyPair"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": { "ec2:ResourceTag/${TAG_KEY}": "${TAG_VALUE}" }
      }
    }
  ]
}
EOF
)

echo "account: $(aws sts get-caller-identity --query Account --output text)" >&2

if aws iam get-user --user-name "$USER_NAME" >/dev/null 2>&1; then
  echo "user $USER_NAME already exists, skipping create" >&2
else
  aws iam create-user --user-name "$USER_NAME" >/dev/null
fi

aws iam put-user-policy \
  --user-name "$USER_NAME" \
  --policy-name "$POLICY_NAME" \
  --policy-document "$POLICY_DOC"

echo "policy $POLICY_NAME attached to $USER_NAME" >&2
echo "creating access key (existing keys are left alone — delete manually if rotating)" >&2
aws iam create-access-key --user-name "$USER_NAME"
