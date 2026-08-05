#!/usr/bin/env bash
# Bring up the four segments and prove they behave like four segments.
#
# The assertions run before anything is installed, because a topology
# that does not isolate makes every result taken on it meaningless.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
LAN=10.10.0        # the site, private
WAN=198.51.100     # the transit segment, the internet
CLOUD_A=203.0.113  # one cloud
CLOUD_B=192.0.2    # another cloud

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }

# The node image ships no ping and no wg, so a reachability test run
# through docker exec fails because the tool is absent and reads exactly
# like the network being broken. Enter the namespace and use the host's
# tools: same packets, same interfaces, a result that means what it says.
netns() {
  local pid
  pid=$(docker inspect -f '{{.State.Pid}}' "$(c "$1")") || return 1
  sudo nsenter -t "$pid" -n "${@:2}"
}
reaches() { netns "$1" ping -c1 -W"${3:-3}" "$2" >/dev/null 2>&1; }
fail() { echo "FAIL: $*" >&2; exit 1; }
addr() {
  in_node "$1" ip addr add "$2" dev "$3" 2>/dev/null || true
  in_node "$1" ip link set "$3" up
}

echo "--- the segments ---"
# containerlab attaches to bridges that already exist. They are given no
# address on purpose: a host holding an address on two of them would
# route between them, and the isolation asserted below would be the
# assertion being wrong rather than the topology being right.
for br in cldt-lan cldt-wan cldt-cloud-a cldt-cloud-b; do
  ip link show "$br" >/dev/null 2>&1 || sudo ip link add name "$br" type bridge
  sudo ip link set "$br" up
  sudo ip addr flush dev "$br" 2>/dev/null || true
done

echo "--- deploying ---"
sudo containerlab deploy -t topo.clab.yml --reconfigure >/dev/null

echo "--- addressing ---"
addr router  "$LAN.1/24" eth1;      addr router "$WAN.1/24" eth2
addr cp      "$LAN.10/24" eth1
addr w1      "$LAN.11/24" eth1
addr w2      "$LAN.12/24" eth1
addr edge-a  "$CLOUD_A.1/24" eth1;  addr edge-a "$WAN.2/24" eth2
addr edge-b  "$CLOUD_B.1/24" eth1;  addr edge-b "$WAN.3/24" eth2
addr remote1 "$CLOUD_A.10/24" eth1
addr remote2 "$CLOUD_B.10/24" eth1

echo "--- routing ---"
for n in cp w1 w2; do in_node "$n" ip route replace default via "$LAN.1" dev eth1; done
in_node remote1 ip route replace default via "$CLOUD_A.1" dev eth1
in_node remote2 ip route replace default via "$CLOUD_B.1" dev eth1
# The edges know how to reach each other's clouds across the wan. The
# site's router needs no route back into the site from outside, because
# nothing outside addresses anything inside.
in_node router ip route replace "$CLOUD_A.0/24" via "$WAN.2" dev eth2
in_node router ip route replace "$CLOUD_B.0/24" via "$WAN.3" dev eth2
in_node edge-a ip route replace "$CLOUD_B.0/24" via "$WAN.3" dev eth2
in_node edge-b ip route replace "$CLOUD_A.0/24" via "$WAN.2" dev eth2

echo "--- what each edge does ---"
# The site: anything may leave wearing the router's address, and only
# the answer to something that left may come back. Masquerading alone is
# not a site, because a router that forwards forwards inward too, which
# is what the assertions caught the first time this was written.
in_node router sysctl -qw net.ipv4.ip_forward=1
in_node router iptables -t nat -F POSTROUTING
in_node router iptables -t nat -A POSTROUTING -s "$LAN.0/24" -o eth2 -j MASQUERADE
in_node router iptables -F FORWARD
in_node router iptables -P FORWARD DROP
in_node router iptables -A FORWARD -i eth1 -o eth2 -j ACCEPT
in_node router iptables -A FORWARD -i eth2 -o eth1 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

# The clouds: a public address is reachable, which is the reason a
# remote node is put in one. No translation, no filtering, both ways.
for e in edge-a edge-b; do
  in_node "$e" sysctl -qw net.ipv4.ip_forward=1
  in_node "$e" iptables -F FORWARD
  in_node "$e" iptables -P FORWARD ACCEPT
done

echo "--- proving it ---"

for n in cp w1 w2; do
  reaches "$n" "$CLOUD_A.10" || fail "$n cannot reach cloud A, so the site has no way out"
  reaches "$n" "$CLOUD_B.10" || fail "$n cannot reach cloud B, so the site has no way out"
done
echo "  the site reaches both clouds"

# Different clouds, meeting only across the wan.
reaches remote1 "$CLOUD_B.10" || fail "remote1 cannot reach remote2, so the clouds do not meet"
reaches remote2 "$CLOUD_A.10" || fail "remote2 cannot reach remote1, so the clouds do not meet"
echo "  the two clouds reach each other across the wan"

# The property everything else rests on, per node and per cloud.
for r in remote1 remote2; do
  for n in cp:10 w1:11 w2:12; do
    if reaches "$r" "$LAN.${n#*:}" 2; then
      fail "$r reached ${n%%:*} at $LAN.${n#*:}: the site is not private, and every result after this is meaningless"
    fi
  done
done
echo "  neither cloud reaches any node at the site"

if netns remote1 timeout 3 bash -c "</dev/tcp/$LAN.10/6443" 2>/dev/null; then
  fail "remote1 opened a connection to the API server directly: a tunnel would not be the only way in, so joining over one stays untested"
fi
echo "  neither cloud reaches the API server, so a tunnel is the only way in"

echo
echo "site     $LAN.0/24      cp .10  w1 .11  w2 .12   router .1"
echo "wan      $WAN.0/24      router .1  edge-a .2  edge-b .3"
echo "cloud A  $CLOUD_A.0/24  remote1 .10"
echo "cloud B  $CLOUD_B.0/24  remote2 .10"
