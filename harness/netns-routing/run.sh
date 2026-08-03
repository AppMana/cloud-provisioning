#!/usr/bin/env bash
# Single-NIC routing e2e for the dialer binary, entirely in network
# namespaces on one machine. Two nodes ("onprem", "cloud"), each with
# exactly ONE uplink and a default route -- faithful to the real
# topologies (a LAN host behind a gateway; an EC2 instance with a VPC
# default route). A third namespace ("wan") routes between them,
# standing in for the internet.
#
# What this proves, against the REAL binary and a REAL kernel:
#   1. The tunnel comes up by dialing out and traffic flows.
#   2. The ONLY kernel routes the dialer adds are /32 host routes to
#      peer node addresses, in the main table, via the cldt* interface.
#   3. Longest-prefix match does the entire routing job: a peer /32
#      beats both the default route and a broader covering route.
#   4. The toxic input class from the 2026-07-22 incident
#      (--pod-cidrs=0.0.0.0/0,::/0) adds ZERO kernel routes -- the
#      CIDRs land only in WireGuard AllowedIPs (the cryptokey filter).
#   5. A route-host broader than a single host is refused at parse
#      time; nothing is installed.
#   6. The default route is byte-identical before and after everything.
#
# Requires: root (ip netns), the wireguard kernel module, go.
set -euo pipefail
cd "$(dirname "$0")"
REPO_DIR="$(cd ../.. && pwd)"

NS_ONPREM=cldt-e2e-onprem
NS_CLOUD=cldt-e2e-cloud
NS_WAN=cldt-e2e-wan
IFACE=cldttest0000
LOG_DIR="${LOG_DIR:-$(mktemp -d /tmp/cldt-netns-e2e.XXXXXX)}"

cleanup() {
  ip netns del "$NS_ONPREM" 2>/dev/null || true
  ip netns del "$NS_CLOUD" 2>/dev/null || true
  ip netns del "$NS_WAN" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

fail() { echo "FAIL: $*" >&2; exit 1; }

modprobe wireguard 2>/dev/null || true

echo "--- build the real dialer ---"
( cd "$REPO_DIR/controller" && CGO_ENABLED=0 go build -o "$LOG_DIR/wg-dialer" ./cmd/dialer )

echo "--- topology: onprem(192.0.2.10) <-> wan <-> cloud(198.51.100.20), one uplink + default route each ---"
ip netns add "$NS_ONPREM"; ip netns add "$NS_CLOUD"; ip netns add "$NS_WAN"
ip link add op-up type veth peer name op-wan
ip link add cl-up type veth peer name cl-wan
ip link set op-up netns "$NS_ONPREM"; ip link set op-wan netns "$NS_WAN"
ip link set cl-up netns "$NS_CLOUD";  ip link set cl-wan netns "$NS_WAN"
ip -n "$NS_ONPREM" addr add 192.0.2.10/24 dev op-up
ip -n "$NS_WAN"    addr add 192.0.2.1/24  dev op-wan
ip -n "$NS_CLOUD"  addr add 198.51.100.20/24 dev cl-up
ip -n "$NS_WAN"    addr add 198.51.100.1/24  dev cl-wan
for ns in "$NS_ONPREM" "$NS_CLOUD" "$NS_WAN"; do ip -n "$ns" link set lo up; done
ip -n "$NS_ONPREM" link set op-up up; ip -n "$NS_CLOUD" link set cl-up up
ip -n "$NS_WAN" link set op-wan up;   ip -n "$NS_WAN" link set cl-wan up
ip -n "$NS_ONPREM" route add default via 192.0.2.1
ip -n "$NS_CLOUD"  route add default via 198.51.100.1
ip netns exec "$NS_WAN" sysctl -qw net.ipv4.ip_forward=1

ip netns exec "$NS_ONPREM" ping -c1 -W2 198.51.100.20 >/dev/null || fail "wan plumbing: onprem cannot reach cloud pre-tunnel"

echo "--- identities (generated here, never leave this run) ---"
ONPREM_KEY=$(wg genkey); ONPREM_PUB=$(echo "$ONPREM_KEY" | wg pubkey)
CLOUD_KEY=$(wg genkey);  CLOUD_PUB=$(echo "$CLOUD_KEY" | wg pubkey)
# Tunnel addresses + stand-in node VIPs (RFC 5737 test space).
ONPREM_TUN=10.100.0.1; CLOUD_TUN=10.100.0.2
ONPREM_VIP=203.0.113.1; CLOUD_VIP=203.0.113.2

mkdir -p "$LOG_DIR/onprem" "$LOG_DIR/cloud"
cat > "$LOG_DIR/cloud/peers.json" <<EOF
{"privateKey":"$CLOUD_KEY","localAddress":"$CLOUD_TUN/24","peers":[
  {"publicKey":"$ONPREM_PUB","allowedIPs":["$ONPREM_TUN/32","$ONPREM_VIP/32"],"routeHosts":["$ONPREM_TUN","$ONPREM_VIP"]}]}
EOF
cat > "$LOG_DIR/onprem/peers.json" <<EOF
{"privateKey":"$ONPREM_KEY","localAddress":"$ONPREM_TUN/24","peers":[
  {"publicKey":"$CLOUD_PUB","endpoint":"198.51.100.20:51820","allowedIPs":["$CLOUD_TUN/32","$CLOUD_VIP/32"],"routeHosts":["$CLOUD_TUN","$CLOUD_VIP"]}]}
EOF

# A broader covering route for CLOUD_VIP already exists on onprem: the
# dialer's /32 must beat it by longest-prefix, not by replacing it.
ip -n "$NS_ONPREM" route add 203.0.113.0/24 via 192.0.2.1

DEFAULT_BEFORE_ONPREM=$(ip -n "$NS_ONPREM" route show default)
DEFAULT_BEFORE_CLOUD=$(ip -n "$NS_CLOUD" route show default)

echo "--- assert 5: a route-host broader than a host is refused at parse time ---"
cat > "$LOG_DIR/onprem/toxic-route.json" <<EOF
{"privateKey":"$ONPREM_KEY","localAddress":"$ONPREM_TUN/24","peers":[
  {"publicKey":"$CLOUD_PUB","allowedIPs":["$CLOUD_TUN/32"],"routeHosts":["10.0.0.0/8"]}]}
EOF
if ip netns exec "$NS_ONPREM" timeout 5 "$LOG_DIR/wg-dialer" \
    --iface="$IFACE" --peers-file="$LOG_DIR/onprem/toxic-route.json" \
    >"$LOG_DIR/onprem/toxic-route.log" 2>&1; then
  :
fi
grep -q "single hosts" "$LOG_DIR/onprem/toxic-route.log" || fail "no parse-time refusal of a /8 route-host"
if ip -n "$NS_ONPREM" link show "$IFACE" >/dev/null 2>&1; then
  fail "the interface was created despite the refused route-host -- refusal must precede ANY kernel mutation"
fi
echo "  refused /8 route-host at parse time; kernel untouched (no link, no routes)"

echo "--- start both roles (cloud listens; onprem dials out) ---"
ip netns exec "$NS_CLOUD" "$LOG_DIR/wg-dialer" \
  --iface="$IFACE" --peers-file="$LOG_DIR/cloud/peers.json" \
  --listen-port=51820 --pod-cidrs=10.244.0.0/16 --service-cidrs=10.96.0.0/12 \
  --poll-interval=2s >"$LOG_DIR/cloud/dialer.log" 2>&1 &
CLOUD_PID=$!
ip netns exec "$NS_ONPREM" "$LOG_DIR/wg-dialer" \
  --iface="$IFACE" --peers-file="$LOG_DIR/onprem/peers.json" \
  --pod-cidrs=10.244.0.0/16 --service-cidrs=10.96.0.0/12 \
  --poll-interval=2s >"$LOG_DIR/onprem/dialer.log" 2>&1 &
ONPREM_PID=$!
trap 'kill $CLOUD_PID $ONPREM_PID 2>/dev/null || true; cleanup' EXIT

echo "--- assert 1: handshake + tunnel traffic ---"
ok=""
for _ in $(seq 1 15); do
  if ip netns exec "$NS_ONPREM" ping -c1 -W1 "$CLOUD_TUN" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
[ -n "$ok" ] || { cat "$LOG_DIR/onprem/dialer.log" "$LOG_DIR/cloud/dialer.log" >&2; fail "no tunnel traffic within 15s"; }
echo "  onprem -> cloud tunnel ping OK"

echo "--- assert 2: only /32 host routes via $IFACE, main table ---"
ONPREM_ROUTES=$(ip -n "$NS_ONPREM" route show dev "$IFACE" | grep -v "proto kernel" | sed 's/[[:space:]]*$//' | sort)
WANT_ONPREM=$(printf '%s\n' "$CLOUD_TUN scope link" "$CLOUD_VIP scope link" | sort)
[ "$ONPREM_ROUTES" == "$WANT_ONPREM" ] || fail "onprem routes via $IFACE: got [$ONPREM_ROUTES], want exactly the two peer /32s"
CLOUD_ROUTES=$(ip -n "$NS_CLOUD" route show dev "$IFACE" | grep -v "proto kernel" | sed 's/[[:space:]]*$//' | sort)
WANT_CLOUD=$(printf '%s\n' "$ONPREM_TUN scope link" "$ONPREM_VIP scope link" | sort)
[ "$CLOUD_ROUTES" == "$WANT_CLOUD" ] || fail "cloud routes via $IFACE: got [$CLOUD_ROUTES], want exactly the two peer /32s"
for ns in "$NS_ONPREM" "$NS_CLOUD"; do
  [ -z "$(ip netns exec "$ns" ip rule list | grep -v '^0:\|^32766:\|^32767:')" ] || fail "$ns: unexpected FIB rules (no route-table games allowed)"
done
echo "  exactly two /32s per side, no extra FIB rules"

echo "--- assert 3: longest-prefix beats both the default and a covering /24 ---"
ip -n "$NS_ONPREM" route get "$CLOUD_VIP" | grep -q "dev $IFACE" || fail "covering 203.0.113.0/24 won over the peer /32"
ip -n "$NS_CLOUD" route get "$ONPREM_VIP" | grep -q "dev $IFACE" || fail "cloud: default route won over the peer VIP /32"
echo "  peer /32s win by longest-prefix on both sides"

echo "--- assert 4: toxic --pod-cidrs=0.0.0.0/0,::/0 adds ZERO kernel routes ---"
kill $ONPREM_PID 2>/dev/null || true; wait $ONPREM_PID 2>/dev/null || true
ROUTES_BEFORE_TOXIC=$(ip -n "$NS_ONPREM" route show | sort)
ip netns exec "$NS_ONPREM" "$LOG_DIR/wg-dialer" \
  --iface="$IFACE" --peers-file="$LOG_DIR/onprem/peers.json" \
  --pod-cidrs=0.0.0.0/0,::/0 --service-cidrs= \
  --poll-interval=2s >"$LOG_DIR/onprem/dialer-toxic.log" 2>&1 &
ONPREM_PID=$!
sleep 3
ROUTES_AFTER_TOXIC=$(ip -n "$NS_ONPREM" route show | sort)
[ "$ROUTES_BEFORE_TOXIC" == "$ROUTES_AFTER_TOXIC" ] || fail "toxic pod-cidrs CHANGED the route table:
--- before ---
$ROUTES_BEFORE_TOXIC
--- after ---
$ROUTES_AFTER_TOXIC"
ip netns exec "$NS_ONPREM" wg show "$IFACE" allowed-ips | grep -q "0.0.0.0/0" \
  || fail "0.0.0.0/0 missing from AllowedIPs -- it must land in the cryptokey filter, just never in a route"
ip netns exec "$NS_ONPREM" ping -c1 -W2 "$CLOUD_TUN" >/dev/null || fail "tunnel broken under toxic pod-cidrs"
ip netns exec "$NS_ONPREM" ping -c1 -W2 198.51.100.20 >/dev/null || fail "UNDERLAY HIJACKED: plain wan traffic no longer flows with 0.0.0.0/0 in AllowedIPs"
echo "  route table byte-identical; 0.0.0.0/0 in AllowedIPs only; underlay + tunnel both alive"

echo "--- assert 6: default routes byte-identical ---"
[ "$(ip -n "$NS_ONPREM" route show default)" == "$DEFAULT_BEFORE_ONPREM" ] || fail "onprem default route changed"
[ "$(ip -n "$NS_CLOUD" route show default)" == "$DEFAULT_BEFORE_CLOUD" ] || fail "cloud default route changed"
echo "  default routes untouched"

echo
echo "ALL ASSERTIONS PASSED (logs: $LOG_DIR)"
