#!/usr/bin/env bash
# Bring up the two segments and prove they are two segments.
#
# Addresses, routes, and the one rule that makes the site a site: the
# router masquerades what leaves and forwards nothing in. Then the
# assertions, which run before anything is installed, because a
# topology that does not isolate is not worth building a cluster on.
set -euo pipefail
cd "$(dirname "$0")"

LAN_NET=10.10.0
WAN_NET=203.0.113
LAB=cldt

# lan side
declare -A LAN=( [router]=1 [cp]=10 [w1]=11 [w2]=12 )
# wan side, addresses the site treats as public
declare -A WAN=( [router]=1 [remote1]=10 [remote2]=11 )

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }

# The node image ships no ping and no wg, so a reachability test run
# with docker exec fails because the tool is missing and reads exactly
# like the network being broken. Enter the namespace and use the host's
# tools instead: same packets, same interfaces, a result that means
# what it says.
netns() {
  local pid
  pid=$(docker inspect -f '{{.State.Pid}}' "$(c "$1")") || return 1
  sudo nsenter -t "$pid" -n "${@:2}"
}
reaches() { netns "$1" ping -c1 -W"${3:-3}" "$2" >/dev/null 2>&1; }

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "--- the two segments ---"
# containerlab attaches to bridges that already exist. They are given no
# address on purpose: a host with an address on both would route between
# them, and the isolation this harness asserts would be the assertion
# being wrong rather than the topology being right.
for br in cldt-lan cldt-wan; do
  ip link show "$br" >/dev/null 2>&1 || sudo ip link add name "$br" type bridge
  sudo ip link set "$br" up
  sudo ip addr flush dev "$br" 2>/dev/null || true
done

echo "--- deploying the topology ---"
sudo containerlab deploy -t topo.clab.yml --reconfigure >/dev/null

echo "--- addressing ---"
for n in "${!LAN[@]}"; do
  in_node "$n" ip addr add "$LAN_NET.${LAN[$n]}/24" dev eth1 2>/dev/null || true
  in_node "$n" ip link set eth1 up
done
for n in "${!WAN[@]}"; do
  dev=eth1; [ "$n" = router ] && dev=eth2
  in_node "$n" ip addr add "$WAN_NET.${WAN[$n]}/24" dev "$dev" 2>/dev/null || true
  in_node "$n" ip link set "$dev" up
done

echo "--- the site's only way out ---"
# Everything at the site leaves through the router, and leaves wearing
# the router's address. Nothing is forwarded inward: there is no rule
# admitting it and no route to send it by.
in_node router sysctl -qw net.ipv4.ip_forward=1
in_node router iptables -t nat -F POSTROUTING
in_node router iptables -t nat -A POSTROUTING -s "$LAN_NET.0/24" -o eth2 -j MASQUERADE

# Masquerading alone is not a site. A router that forwards will happily
# forward inward, and the far side walks straight in, which the
# assertions below caught the first time this ran. What makes the site
# private is the direction: anything may leave, and only the answer to
# something that left may come back.
in_node router iptables -F FORWARD
in_node router iptables -P FORWARD DROP
in_node router iptables -A FORWARD -i eth1 -o eth2 -j ACCEPT
in_node router iptables -A FORWARD -i eth2 -o eth1 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
for n in cp w1 w2; do
  in_node "$n" ip route replace default via "$LAN_NET.1" dev eth1
done
# The remotes are on the far side and route to the site by nothing at
# all. Their default is the wan segment, which is as far as it goes.
for n in remote1 remote2; do
  in_node "$n" ip route replace default via "$WAN_NET.1" dev eth1
done

echo "--- proving the two segments are two segments ---"

# The site reaches the far side, because it opened the connection.
for n in cp w1 w2; do
  reaches "$n" "$WAN_NET.10" \
    || fail "$n cannot reach the far side, so the site has no way out"
done
echo "  the site reaches the far side"

# The far side reaches no address at the site. This is the property the
# whole thing rests on, so it is asserted per node rather than sampled.
for r in remote1 remote2; do
  for n in cp w1 w2; do
    if reaches "$r" "$LAN_NET.${LAN[$n]}" 2; then
      fail "$r reached $n at $LAN_NET.${LAN[$n]}: the site is not private, and every result after this would be meaningless"
    fi
  done
done
echo "  the far side reaches no node at the site"

# And cannot reach the API server's port even when it knows where to
# look, which is the specific thing the previous harness had to allow.
if netns remote1 timeout 3 bash -c "</dev/tcp/$LAN_NET.10/6443" 2>/dev/null; then
  fail "remote1 opened a connection to the API server directly: the tunnel is not the only path, so joining over it is untested"
fi
echo "  the far side cannot reach the API server except through a tunnel"

echo
echo "lan  $LAN_NET.0/24   cp .10  w1 .11  w2 .12   router .1"
echo "wan  $WAN_NET.0/24   remote1 .10  remote2 .11  router .1"
echo "the site leaves through the router and nothing comes back in"
