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


# containerd cannot stack overlay on the container's own overlay, so
# each node that runs containers gets its image store on a real
# filesystem. kind gives its nodes a volume for the same reason.
#
# The store is seeded from the image rather than started empty. The node
# image ships the cluster's own images inside that directory, and this
# site has no route to a registry by design, so mounting an empty
# directory over it leaves the node with working storage and nothing to
# run out of it.
NODE_IMAGE=$(awk '/image:/{print $2; exit}' topo.clab.yml)
if [ ! -d var/seed/io.containerd.content.v1.content ]; then
  echo "  seeding the image store from $NODE_IMAGE"
  rm -rf var/seed && mkdir -p var/seed
  seed=$(docker create "$NODE_IMAGE")
  docker cp "$seed:/var/lib/containerd/." var/seed/ >/dev/null
  docker rm -f "$seed" >/dev/null
fi
for n in cp cp2 cp3 w1 w2 remote1 remote2; do
  [ -d "var/$n/io.containerd.content.v1.content" ] && continue
  rm -rf "var/$n"; mkdir -p "var/$n"
  sudo cp -a var/seed/. "var/$n/"
done

# The distribution data-root binds (var-k0s, var-rancher) are wiped in
# the deploy section below, after the previous lab's containers are
# gone: wiping them here would race the running containers' own
# overlay mounts inside those directories, and rm loses that race
# ("Directory not empty" on a live k0s data root).

echo "--- deploying ---"
# A loaded node takes longer to die than docker waits for its exit
# event, so a reconfigure's destroy can report failure having actually
# killed the container, and the deploy then refuses because the corpse
# still exists. Remove whatever is left before deploying; a machine
# that takes two tries to power off still powers off.
for _ in 1 2 3; do
  left=$(docker ps -aq --filter "name=clab-$LAB-")
  [ -z "$left" ] && break
  docker rm -f $left >/dev/null 2>&1
  sleep 2
done
[ -z "$(docker ps -aq --filter "name=clab-$LAB-")" ] || fail "the previous lab's containers cannot be removed"
# The same overlay rule that gives /var/lib/containerd a real
# filesystem applies to every distribution's own data root: k0s runs
# its containerd under /var/lib/k0s, k3s and RKE2 under
# /var/lib/rancher, and an overlay upperdir on the node's own overlay
# is refused by the kernel (measured: k0s's nllb envoy sandbox,
# "failed to mount rootfs ... invalid argument"). Unlike the seeded
# containerd store, these directories carry cluster state, etcd among
# it, so a fresh lab starts them empty rather than inheriting a
# previous cluster's identity. Only after the containers are gone: a
# running node holds overlay mounts inside these, and rm loses that
# race. The mounts' teardown is itself asynchronous, so the wipe
# retries.
for n in cp cp2 cp3 w1 w2 remote1 remote2; do
  for d in var-k0s var-rancher; do
    for _ in 1 2 3 4 5; do
      sudo rm -rf "${d:?}/$n" 2>/dev/null && break
      sleep 2
    done
    [ -e "$d/$n" ] && fail "cannot clear $d/$n, the previous lab still holds mounts in it"
    mkdir -p "$d/$n"
  done
done

# A veth's host side can outlive its container: the outage harness
# re-plumbs NICs on reboot rows, and a pair created that way is not
# torn down by the container's removal. With every lab container gone,
# any link still enslaved to a lab bridge is by definition stale, and
# containerlab refuses to deploy over a name that already exists.
for br in cldt-lan cldt-wan cldt-cloud-a cldt-cloud-b; do
  ip -o link show master "$br" 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1 |
    while read -r ifc; do
      [ -n "$ifc" ] && sudo ip link del "$ifc" 2>/dev/null
    done
done
sudo containerlab deploy -t topo.clab.yml --reconfigure >/dev/null

echo "--- addressing ---"
addr router  "$LAN.1/24" eth1;      addr router "$WAN.1/24" eth2
addr bastion "$LAN.2/24" eth1
addr cp      "$LAN.10/24" eth1
addr cp2     "$LAN.13/24" eth1
addr cp3     "$LAN.14/24" eth1
addr w1      "$LAN.11/24" eth1
addr w2      "$LAN.12/24" eth1
addr edge-a  "$CLOUD_A.1/24" eth1;  addr edge-a "$WAN.2/24" eth2
addr edge-b  "$CLOUD_B.1/24" eth1;  addr edge-b "$WAN.3/24" eth2
addr remote1 "$CLOUD_A.10/24" eth1
addr remote2 "$CLOUD_B.10/24" eth1

echo "--- the wan reaches the internet ---"
# The wan segment is where this lab meets the real world. This host is
# its last hop: it holds an address there, masquerades what leaves, and
# knows how to get back into each cloud.
#
# It gives the site the path it would really have (out through its own
# router, translated, and out again here) and gives a cloud node the one
# it would really have (straight out from a public address). Neither
# creates a way in: this host has no address on the site and no route to
# it, and the site's router still admits nothing it did not ask for.
sudo ip addr replace "$WAN.254/24" dev cldt-wan
sudo sysctl -qw net.ipv4.ip_forward=1
# br_netfilter, loaded on the host because a container cannot load
# modules and the nodes need the files to exist: flannel refuses to
# start at all when /proc/sys/net/bridge/bridge-nf-call-iptables is
# absent (k0s logs the same condition and carries on), and loading
# the module is the standard Kubernetes node prerequisite kind also
# demands of its host. The sysctls are per-namespace on this kernel,
# so the host's own bridges keep their current behavior by pinning
# the host's value to 0; each node sets its own inside its namespace.
if ! [ -e /proc/sys/net/bridge/bridge-nf-call-iptables ]; then
  sudo modprobe br_netfilter || fail "cannot load br_netfilter, which the nodes' networks require"
fi
sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0 net.bridge.bridge-nf-call-ip6tables=0
UPLINK=$(ip route show default | awk '{print $5; exit}')
[ -n "$UPLINK" ] || fail "this host has no default route, so the lab has no internet to reach"
for net in "$WAN.0/24" "$CLOUD_A.0/24" "$CLOUD_B.0/24"; do
  sudo iptables -t nat -C POSTROUTING -s "$net" -o "$UPLINK" -j MASQUERADE 2>/dev/null \
    || sudo iptables -t nat -A POSTROUTING -s "$net" -o "$UPLINK" -j MASQUERADE
done
# Replies to a cloud node have to find their way back to its cloud.
sudo ip route replace "$CLOUD_A.0/24" via "$WAN.2" dev cldt-wan
sudo ip route replace "$CLOUD_B.0/24" via "$WAN.3" dev cldt-wan

echo "--- routing ---"
for n in bastion cp cp2 cp3 w1 w2; do in_node "$n" ip route replace default via "$LAN.1" dev eth1; done
in_node remote1 ip route replace default via "$CLOUD_A.1" dev eth1
in_node remote2 ip route replace default via "$CLOUD_B.1" dev eth1
# The edges know how to reach each other's clouds across the wan. The
# site's router needs no route back into the site from outside, because
# nothing outside addresses anything inside.
in_node router ip route replace "$CLOUD_A.0/24" via "$WAN.2" dev eth2
in_node router ip route replace "$CLOUD_B.0/24" via "$WAN.3" dev eth2
in_node edge-a ip route replace "$CLOUD_B.0/24" via "$WAN.3" dev eth2
in_node edge-b ip route replace "$CLOUD_A.0/24" via "$WAN.2" dev eth2
for e in router edge-a edge-b; do
  in_node "$e" ip route replace default via "$WAN.254" dev eth2
done

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

for n in bastion cp cp2 cp3 w1 w2; do
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
  for n in bastion:2 cp:10 cp2:13 cp3:14 w1:11 w2:12; do
    if reaches "$r" "$LAN.${n#*:}" 2; then
      fail "$r reached ${n%%:*} at $LAN.${n#*:}: the site is not private, and every result after this is meaningless"
    fi
  done
done
echo "  neither cloud reaches any node at the site"

# Every address a site node holds, not the ones this script happens to
# know. A back channel is by definition an address nobody thought to
# check, which is exactly how the management network went unnoticed.
for r in remote1 remote2; do
  for n in bastion cp cp2 cp3 w1 w2; do
    addrs=$(netns "$n" ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1)
    # This is the check that caught the management network. If the list
    # comes back empty it tests nothing and says it passed, which is the
    # failure mode it exists to prevent.
    [ -n "$addrs" ] || fail "could not read $n's addresses, so this check would pass having tested nothing"
    for a in $addrs; do
      if reaches "$r" "$a" 2; then
        fail "$r reached $n at $a: a path exists that the segments do not explain"
      fi
    done
  done
done
echo "  and at no other address any of them holds"

if netns remote1 timeout 3 bash -c "</dev/tcp/$LAN.10/6443" 2>/dev/null; then
  fail "remote1 opened a connection to the API server directly: a tunnel would not be the only way in, so joining over one stays untested"
fi
echo "  neither cloud reaches the API server, so a tunnel is the only way in"

for n in cp cp2 cp3 w1 w2; do
  reaches "$n" 1.1.1.1 || fail "$n has no path off the lab, which it would have through its own router"
done
for r in remote1 remote2; do
  reaches "$r" 1.1.1.1 || fail "$r has no path off the lab, which it would have from a public address"
done
echo "  the site and both clouds reach the internet"

echo
echo "site     $LAN.0/24      bastion .2  cp .10  cp2 .13  cp3 .14  w1 .11  w2 .12  router .1  api-vip .100"
echo "wan      $WAN.0/24      router .1  edge-a .2  edge-b .3"
echo "cloud A  $CLOUD_A.0/24  remote1 .10"
echo "cloud B  $CLOUD_B.0/24  remote2 .10"
