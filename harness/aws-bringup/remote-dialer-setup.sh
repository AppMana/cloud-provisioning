#!/usr/bin/env bash
# Runs on the real on-prem host via `ssh ... bash -s -- ... < this-file`,
# invoked by bringup.sh. Args: JARVIS_KEY EC2_PUB EC2_IP WG_NET TEST_NET.
# Brings up wg0 as the dialer using the real wg-pullup.sh (copied over,
# not re-implemented) and a disjoint test-only dummy interface so this
# never touches the real cluster's 10.101.0.0/16 addressing.
set -euo pipefail
JARVIS_KEY="$1"; EC2_PUB="$2"; EC2_IP="$3"; WG_NET="$4"; TEST_NET="$5"

sudo mkdir -p /etc/wireguard
sudo tee /etc/wireguard/node.env >/dev/null <<EOF
ADDRESS=${WG_NET}.1/24
PRIVATE_KEY=${JARVIS_KEY}
PEER_PUBLIC_KEY=${EC2_PUB}
ALLOWED_IPS=${WG_NET}.2/32,${TEST_NET}.2.0/24
ENDPOINT=${EC2_IP}:51820
KEEPALIVE=5
EOF
sudo chmod 600 /etc/wireguard/node.env

sudo ip link add podtest type dummy 2>/dev/null || true
sudo ip addr add "${TEST_NET}.1.1/32" dev podtest 2>/dev/null || true
sudo ip link set podtest up

# wg-pullup.sh itself arrives separately via scp (see bringup.sh) to
# /tmp/wg-pullup -- installed from there rather than duplicated inline,
# so this is the same artifact the containernet harness runs, not a copy.
sudo install -m 0755 /tmp/wg-pullup /usr/local/sbin/wg-pullup
sudo /usr/local/sbin/wg-pullup node

# Raw `wg set` (unlike wg-quick) installs no kernel routes for allowed-ips --
# it only sets up crypto-key routing for traffic that already arrives on
# wg0. Without this, nothing ever gets routed onto wg0 in the first place.
# BGP would supply this in the real design; here it's explicit.
sudo ip route replace "${TEST_NET}.2.0/24" dev wg0
