#!/bin/bash
# wg-pullup — bring up one WireGuard peer from a flat env file, in either
# role. This is the one script both sides run; the systemd unit passes
# the peer name as the instance (%i), same pattern as wg-quick@<name>.
#
# Config file: /etc/wireguard/<peer>.env
#   ADDRESS          CIDR to assign to wg0, e.g. 10.100.0.1/24
#   PRIVATE_KEY      this side's private key
#   PEER_PUBLIC_KEY  the remote side's public key
#   ALLOWED_IPS      routes this peer accepts from its remote
#   LISTEN_PORT      required to accept inbound (the public-IP side)
#   ENDPOINT         set only on the side that dials out
#   KEEPALIVE        seconds; set only on the dialing side — this is the
#                    entire self-heal mechanism, no controller involved
#   WAIT_FOR         optional IP; block here until it answers ICMP
#                    (the tunnel-before-join ordering gate)
set -euo pipefail
peer="$1"
# shellcheck disable=SC1090
source "/etc/wireguard/${peer}.env"

ip link add wg0 type wireguard 2>/dev/null || true
ip link set wg0 mtu 1420

keyfile=$(mktemp)
trap 'rm -f "$keyfile"' EXIT
chmod 600 "$keyfile"
printf '%s' "$PRIVATE_KEY" > "$keyfile"

set -- private-key "$keyfile"
[ -n "${LISTEN_PORT:-}" ] && set -- "$@" listen-port "$LISTEN_PORT"
set -- "$@" peer "$PEER_PUBLIC_KEY" allowed-ips "$ALLOWED_IPS"
[ -n "${ENDPOINT:-}" ] && set -- "$@" endpoint "$ENDPOINT"
[ -n "${KEEPALIVE:-}" ] && set -- "$@" persistent-keepalive "$KEEPALIVE"

wg set wg0 "$@"
ip addr add "$ADDRESS" dev wg0 2>/dev/null || true
ip link set wg0 up

if [ -n "${WAIT_FOR:-}" ]; then
  echo "wg-pullup: waiting for $WAIT_FOR over wg0 before continuing..."
  until ping -c1 -W2 "$WAIT_FOR" >/dev/null 2>&1; do sleep 2; done
  echo "wg-pullup: $WAIT_FOR reachable"
fi
