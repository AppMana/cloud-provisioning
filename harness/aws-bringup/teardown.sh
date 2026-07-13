#!/usr/bin/env bash
# Reverses bringup.sh: terminates the instance, deletes the security
# group and key pair, and removes wg0/the test dummy interface from the
# real on-prem host. Safe to re-run; every step tolerates already-gone
# resources.
set -euo pipefail
cd "$(dirname "$0")"

: "${AWS_DEFAULT_REGION:?export AWS_DEFAULT_REGION}"
: "${ONPREM_SSH_HOST:?export ONPREM_SSH_HOST}"
STATE_DIR="${STATE_DIR:-.state}"

if [ -f "$STATE_DIR/instance_id" ]; then
  INSTANCE_ID=$(cat "$STATE_DIR/instance_id")
  echo "--- terminating $INSTANCE_ID ---"
  aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" >/dev/null 2>&1 || true
  aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" 2>/dev/null || true
fi

if [ -f "$STATE_DIR/sg_id" ]; then
  SG_ID=$(cat "$STATE_DIR/sg_id")
  echo "--- deleting security group $SG_ID ---"
  aws ec2 delete-security-group --group-id "$SG_ID" 2>&1 || echo "  (leave for now -- may still be detaching from the terminated ENI; retry teardown.sh in a minute)"
fi

if [ -f "$STATE_DIR/key_name" ]; then
  KEY_NAME=$(cat "$STATE_DIR/key_name")
  echo "--- deleting key pair $KEY_NAME ---"
  aws ec2 delete-key-pair --key-name "$KEY_NAME" 2>&1 || true
fi

echo "--- removing wg0 / podtest from $ONPREM_SSH_HOST ---"
ssh -o BatchMode=yes "$ONPREM_SSH_HOST" '
  sudo ip link del wg0 2>/dev/null || true
  sudo ip link del podtest 2>/dev/null || true
  sudo rm -f /etc/wireguard/node.env /usr/local/sbin/wg-pullup /tmp/wg-pullup
' || true

echo "--- done. removing local state dir $STATE_DIR ---"
rm -rf "$STATE_DIR"
