#!/usr/bin/env bash
# WAN-worker tunnel harness. Two netns:
#   onprem = cluster side. API server VIP (10.101.0.1), a cluster "pod"
#            (10.101.128.1), bird BGP, and the WG DIALER (handshakes
#            outbound to ec2's public IP; PersistentKeepalive = self-heal).
#   ec2    = remote node with a public IP. WG LISTENER (no endpoint), an
#            ec2 "pod" (10.101.130.1), bird BGP. Its ONLY path to the API
#            and to cluster pods is the tunnel + BGP-learned routes.
#
# Pod CIDRs are carried by BGP ONLY (no static pod routes) so a passing
# pod-to-pod test genuinely proves BGP-over-WG.
set -u
R="sudo ip netns exec onprem"; E="sudo ip netns exec ec2"
say(){ printf '\n=== %s\n' "$*"; }
cleanup(){ sudo ip netns del onprem 2>/dev/null; sudo ip netns del ec2 2>/dev/null; }
trap cleanup EXIT; cleanup

sudo ip netns add onprem; sudo ip netns add ec2
sudo ip link add pub-onprem netns onprem type veth peer name pub-ec2 netns ec2
$R sh -c 'ip addr add 203.0.113.1/24 dev pub-onprem; ip link set pub-onprem up; ip link set lo up'
$E sh -c 'ip addr add 203.0.113.2/24 dev pub-ec2; ip link set pub-ec2 up; ip link set lo up'

$R sh -c 'ip link add vip0 type dummy; ip addr add 10.101.0.1/32 dev vip0; ip link set vip0 up'
$R sh -c 'ip link add podc type dummy; ip addr add 10.101.128.1/32 dev podc; ip link set podc up'
$E sh -c 'ip link add pode type dummy; ip addr add 10.101.130.1/32 dev pode; ip link set pode up'

WD=$(mktemp -d); umask 077
wg genkey >"$WD/o.key"; wg pubkey <"$WD/o.key" >"$WD/o.pub"
wg genkey >"$WD/e.key"; wg pubkey <"$WD/e.key" >"$WD/e.pub"
OPUB=$(cat "$WD/o.pub"); EPUB=$(cat "$WD/e.pub")

# wg0 created IN-PLACE in each netns (moving a wg iface between ns loses it).
$R ip link add wg0 type wireguard
$E ip link add wg0 type wireguard
# Dialer: only /32s in allowed-ips; the pod CIDR arrives via BGP, not here.
$R sh -c "wg set wg0 private-key $WD/o.key peer $EPUB endpoint 203.0.113.2:51820 \
  persistent-keepalive 15 allowed-ips 10.100.0.2/32,10.101.130.0/24"
$R sh -c 'ip addr add 10.100.0.1/24 dev wg0; ip link set wg0 mtu 1420 up'
# Listener: no endpoint (learned from the incoming handshake).
$E sh -c "wg set wg0 private-key $WD/e.key listen-port 51820 peer $OPUB \
  allowed-ips 10.100.0.1/32,10.101.0.0/16"
$E sh -c 'ip addr add 10.100.0.2/24 dev wg0; ip link set wg0 mtu 1420 up'
# API VIP (static, bootstraps the join). Pod CIDRs come via BGP; the wg0
# /24 makes the BGP next-hop on-link so bird can resolve it.
$E ip route add 10.101.0.1/32 dev wg0

say "1. ORDERING: ec2 -> API VIP before tunnel (expect FAIL)"
$E ping -c1 -W1 10.101.0.1 >/dev/null 2>&1 && echo "  reachable (unexpected)" || echo "  UNREACHABLE as expected"

say "2. Node waits for tunnel (poll wg peer), THEN reaches API"
up=""
for i in $(seq 1 20); do
  if $R ping -c1 -W1 10.100.0.2 >/dev/null 2>&1; then up=$i; break; fi
  sleep 1
done
echo "  tunnel established after ~${up}s (dialer's keepalive drove the handshake)"
$E ping -c2 -W2 10.101.0.1 >/dev/null 2>&1 && echo "  ec2 -> API VIP OK — k0s join could now proceed" || echo "  FAIL"

say "3. BGP over wg0 — pod CIDRs via BGP ONLY (no static pod routes)"
cat >"$WD/on.conf" <<EOF
router id 10.100.0.1;
protocol device { }
protocol direct { ipv4; interface "wg0"; }
protocol kernel { ipv4 { import none; export all; }; }
protocol static { ipv4; route 10.101.128.0/24 via "podc"; }
protocol bgp p { local 10.100.0.1 as 64512; neighbor 10.100.0.2 as 64512;
  ipv4 { import all; export all; }; }
EOF
cat >"$WD/ec.conf" <<EOF
router id 10.100.0.2;
protocol device { }
protocol direct { ipv4; interface "wg0"; }
protocol kernel { ipv4 { import none; export all; }; }
protocol static { ipv4; route 10.101.130.0/24 via "pode"; }
protocol bgp p { local 10.100.0.2 as 64512; neighbor 10.100.0.1 as 64512;
  ipv4 { import all; export all; }; }
EOF
$R bird -c "$WD/on.conf" -s "$WD/on.ctl"
$E bird -c "$WD/ec.conf" -s "$WD/ec.ctl"
est=""
for i in $(seq 1 20); do
  s=$($R birdc -s "$WD/on.ctl" show protocols p 2>/dev/null | grep -o "Established")
  [ "$s" = "Established" ] && { est=$i; break; }; sleep 1
done
echo "  BGP session Established after ~${est}s"
$E sh -c "birdc -s $WD/ec.ctl show route 2>/dev/null" | grep -q 10.101.128.0/24 && echo "  ec2 learned cluster pod CIDR 10.101.128.0/24 via BGP" || echo "  route not learned"
$R sh -c "birdc -s $WD/on.ctl show route 2>/dev/null" | grep -q 10.101.130.0/24 && echo "  onprem learned ec2 pod CIDR 10.101.130.0/24 via BGP" || echo "  route not learned"

say "4. Pod-to-pod across the tunnel (routed by BGP, not static)"
$R ping -c2 -W2 -I 10.101.128.1 10.101.130.1 >/dev/null 2>&1 && echo "  10.101.128.1 -> 10.101.130.1 OK" || echo "  FAIL"

say "5. MTU under WG overhead"
$R ping -c1 -W2 -M do -s 1392 -I 10.101.128.1 10.101.130.1 >/dev/null 2>&1 && echo "  1392B fits 1420 MTU: OK" || echo "  1392B FAIL"
$R ping -c1 -W2 -M do -s 1500 -I 10.101.128.1 10.101.130.1 >/dev/null 2>&1 && echo "  1500B: transited (unexpected)" || echo "  1500B correctly needs-frag — MTU enforced"

say "6. SELF-HEAL: flap the public link, auto-recover with NO controller"
$R ip link set pub-onprem down
sleep 2
$E ping -c1 -W1 10.101.0.1 >/dev/null 2>&1 && echo "  API up during outage (unexpected)" || echo "  API down during outage (expected)"
$R ip link set pub-onprem up
rec=""
for i in $(seq 1 30); do
  if $R ping -c1 -W1 -I 10.101.128.1 10.101.130.1 >/dev/null 2>&1; then rec=$i; break; fi
  sleep 1
done
if [ -n "$rec" ]; then
  bs=$($R birdc -s "$WD/on.ctl" show protocols p 2>/dev/null | grep -o Established)
  echo "  pod-to-pod (BGP-routed) recovered after ~${rec}s; BGP=$bs — zero manual action"
else
  echo "  did NOT recover"
fi
say "done"; rm -rf "$WD"
