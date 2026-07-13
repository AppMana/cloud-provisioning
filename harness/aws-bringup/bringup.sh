#!/usr/bin/env bash
# Real AWS bring-up: launches one EC2 instance running the actual
# wg-pullup.sh boot script (the identical file harness/containernet/
# drives in simulation, copied/embedded here, never re-implemented) and
# dials to it from a real on-prem host over the real internet. This is
# the one thing the containernet and raw-netns harnesses can't prove:
# that no NAT/firewall in the real path blocks outbound UDP 51820, and
# that a real cloud security group + real WireGuard handshake actually
# works end to end. Companion: teardown.sh, sharing the same state dir.
#
# Run under the scoped IAM identity from ../../scripts/aws/bootstrap-harness-iam.sh
# (its policy requires every created resource carry Project=cloud-provisioning-harness,
# which this script sets on all of them).
set -euo pipefail
cd "$(dirname "$0")"

: "${AWS_DEFAULT_REGION:?export AWS_DEFAULT_REGION}"
: "${SUBNET_ID:?export SUBNET_ID (a subnet with MapPublicIpOnLaunch=true)}"
: "${VPC_ID:?export VPC_ID}"
: "${AMI_ID:?export AMI_ID (arm64 Ubuntu 24.04)}"
: "${ONPREM_SSH_HOST:?export ONPREM_SSH_HOST (e.g. administrator@100.x.x.x), the dialer side}"
: "${ONPREM_PUBLIC_IP:?export ONPREM_PUBLIC_IP, the dialers real WAN egress IP -- scopes the security group}"

TAG_KEY=Project
TAG_VALUE=cloud-provisioning-harness
INSTANCE_TYPE="${INSTANCE_TYPE:-t4g.micro}"
STATE_DIR="${STATE_DIR:-.state}"
mkdir -p "$STATE_DIR"

# Real keys, both sides. Test-address space (10.100.0.0/24 tunnel,
# 10.222.0.0/16 "pod" stand-ins) is disjoint from the real cluster's
# 10.101.0.0/16 -- this proves the tunnel mechanism without touching
# jarvis's actual Calico state.
JARVIS_KEY=$(wg genkey); JARVIS_PUB=$(echo "$JARVIS_KEY" | wg pubkey)
EC2_KEY=$(wg genkey); EC2_PUB=$(echo "$EC2_KEY" | wg pubkey)
WG_NET=10.100.0
TEST_NET=10.222

echo "--- security group ---"
SG_ID=$(aws ec2 create-security-group \
  --group-name "cloud-provisioning-harness-$$" \
  --description "cloud-provisioning-harness ephemeral test SG" \
  --vpc-id "$VPC_ID" \
  --tag-specifications "ResourceType=security-group,Tags=[{Key=${TAG_KEY},Value=${TAG_VALUE}}]" \
  --query 'GroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --ip-permissions "IpProtocol=udp,FromPort=51820,ToPort=51820,IpRanges=[{CidrIp=${ONPREM_PUBLIC_IP}/32,Description=wg-dialer}]" >/dev/null
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --ip-permissions "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=${ONPREM_PUBLIC_IP}/32,Description=debug-ssh}]" >/dev/null
echo "$SG_ID" > "$STATE_DIR/sg_id"
echo "  $SG_ID"

echo "--- key pair (debug SSH only; not used by the WireGuard test itself) ---"
KEY_NAME="cloud-provisioning-harness-$$"
ssh-keygen -t ed25519 -f "$STATE_DIR/ssh_key" -N "" -q -C "$KEY_NAME"
aws ec2 import-key-pair --key-name "$KEY_NAME" \
  --public-key-material "fileb://$STATE_DIR/ssh_key.pub" \
  --tag-specifications "ResourceType=key-pair,Tags=[{Key=${TAG_KEY},Value=${TAG_VALUE}}]" >/dev/null
echo "$KEY_NAME" > "$STATE_DIR/key_name"
echo "  $KEY_NAME"

echo "--- user-data: the real wg-pullup.sh, listener role + a pod-test dummy iface ---"
WG_PULLUP_B64=$(base64 -w0 ../containernet/image/wg-pullup.sh)
cat > "$STATE_DIR/user-data.sh" <<USERDATA
#!/bin/bash
set -euo pipefail
apt-get update -qq
apt-get install -y -qq wireguard-tools
mkdir -p /etc/wireguard
echo "$WG_PULLUP_B64" | base64 -d > /usr/local/sbin/wg-pullup
chmod +x /usr/local/sbin/wg-pullup
ip link add podtest type dummy
ip addr add ${TEST_NET}.2.1/32 dev podtest
ip link set podtest up
cat > /etc/wireguard/node.env <<EOF
ADDRESS=${WG_NET}.2/24
PRIVATE_KEY=${EC2_KEY}
PEER_PUBLIC_KEY=${JARVIS_PUB}
ALLOWED_IPS=${WG_NET}.1/32,${TEST_NET}.1.0/24
LISTEN_PORT=51820
WAIT_FOR=${WG_NET}.1
EOF
/usr/local/sbin/wg-pullup node > /var/log/wg-pullup.log 2>&1
# Same reasoning as remote-dialer-setup.sh: raw wg set installs no routes.
ip route replace ${TEST_NET}.1.0/24 dev wg0
USERDATA

echo "--- launch ---"
INSTANCE_ID=$(aws ec2 run-instances \
  --image-id "$AMI_ID" --instance-type "$INSTANCE_TYPE" \
  --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
  --key-name "$KEY_NAME" --associate-public-ip-address \
  --tag-specifications "ResourceType=instance,Tags=[{Key=${TAG_KEY},Value=${TAG_VALUE}},{Key=Name,Value=cloud-provisioning-harness}]" \
  --user-data "file://$STATE_DIR/user-data.sh" \
  --query 'Instances[0].InstanceId' --output text)
echo "$INSTANCE_ID" > "$STATE_DIR/instance_id"
echo "  $INSTANCE_ID, waiting for it to enter running state..."
aws ec2 wait instance-running --instance-ids "$INSTANCE_ID"
EC2_IP=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
  --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "$EC2_IP" > "$STATE_DIR/ec2_ip"
echo "  running at $EC2_IP -- cloud-init installs wireguard-tools and blocks in wg-pullup's WAIT_FOR loop until jarvis dials in (expect ~30-90s for apt alone)"

echo "--- dialer side: bring up wg0 on the real on-prem node ($ONPREM_SSH_HOST) ---"
scp -q -o BatchMode=yes ../containernet/image/wg-pullup.sh "$ONPREM_SSH_HOST:/tmp/wg-pullup"
ssh -o BatchMode=yes "$ONPREM_SSH_HOST" bash -s -- "$JARVIS_KEY" "$EC2_PUB" "$EC2_IP" "$WG_NET" "$TEST_NET" < ./remote-dialer-setup.sh

echo "$JARVIS_KEY" > "$STATE_DIR/jarvis.key"; chmod 600 "$STATE_DIR/jarvis.key"

echo "--- verifying: waiting for real handshake over the real internet ---"
ok=0
for _ in $(seq 1 30); do
  if ssh -o BatchMode=yes "$ONPREM_SSH_HOST" "sudo wg show wg0 latest-handshakes 2>/dev/null | awk '{print \$2}'" | grep -qvE '^0?$'; then
    ok=1
    break
  fi
  sleep 2
done
if [ "$ok" = "1" ]; then
  echo "  HANDSHAKE ESTABLISHED"
else
  echo "  FAILED to see a handshake within 60s"
  ssh -o BatchMode=yes "$ONPREM_SSH_HOST" "sudo wg show wg0; ip -br addr show wg0"
  exit 1
fi

echo "--- pod-to-pod over the real tunnel ---"
# -I here must be the source ADDRESS, not the device name: binding to the
# device (SO_BINDTODEVICE) forces egress out that literal interface, which
# for a dummy iface with no real link means nothing is ever routed via
# wg0 at all. Binding the address instead lets normal routing pick wg0.
if ssh -o BatchMode=yes "$ONPREM_SSH_HOST" "ping -c2 -W3 -I ${TEST_NET}.1.1 ${TEST_NET}.2.1 >/dev/null 2>&1"; then
  echo "  OK: ${TEST_NET}.1.1 (jarvis) -> ${TEST_NET}.2.1 (ec2) over the real WAN"
else
  echo "  FAILED"
  exit 1
fi

echo "--- MTU under real WireGuard overhead + real internet path MTU ---"
if ssh -o BatchMode=yes "$ONPREM_SSH_HOST" "ping -c1 -W3 -M do -s 1392 -I ${TEST_NET}.1.1 ${TEST_NET}.2.1 >/dev/null 2>&1"; then
  echo "  1392B fits: OK"
else
  echo "  1392B FAILED (unexpected -- check the real path's actual MTU, not just wg0's configured 1420)"
fi
if ssh -o BatchMode=yes "$ONPREM_SSH_HOST" "ping -c1 -W3 -M do -s 1500 -I ${TEST_NET}.1.1 ${TEST_NET}.2.1 >/dev/null 2>&1"; then
  echo "  1500B transited (unexpected)"
else
  echo "  1500B correctly needs-frag"
fi

echo
echo "All checks passed. State is in $STATE_DIR -- run teardown.sh with the same STATE_DIR to clean up."
