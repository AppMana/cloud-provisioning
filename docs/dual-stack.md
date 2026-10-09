# Dual-stack meshes

A cluster whose pod network allocates IPv6 as well as IPv4 gets a dual-stack
tunnel mesh: every pod, Service and node address of both families is reachable
across the tunnel, through the same routes that carry IPv4. The product detects
this itself; there is no dual-stack switch.

## What the product does

**Detection.** The pod network is dual-stack when Calico has an enabled IPv6
pool, or, for networks without their own address management, when nodes carry
IPv6 pod CIDRs. The controller logs `dual-stack=true` and the IPv6 tunnel
prefix it uses at startup.

**Tunnel addresses.** Each tunnel endpoint and each remote keeps its IPv4
tunnel address and gains an IPv6 one: the IPv4 address in the low 32 bits of
`tunnel.ipv6Prefix` (default `fd00:10:100::/96`), so `10.100.0.128` pairs with
`fd00:10:100::a64:80`. The IPv4 reservation and retirement records therefore
govern both families. The pairing is published as `node-tunnel-address6-<node>`
for site endpoints and as the Machine annotation
`cloud-provisioning.appmana.com/wireguard-addr6` and `localAddress6` in the
remote's identity file. Each address is permitted and routed on its owner's
peer entry only. Set `tunnel.ipv6Prefix: ""` to keep a dual-stack cluster's
mesh IPv4-only.

**Remote CNI addresses.** A remote is given both Calico node addresses,
`projectcalico.org/IPv4Address` and `projectcalico.org/IPv6Address`, set to its
tunnel addresses. Calico keeps a stored address only when its own detection on
that node finds nothing; a remote's `calico-node` then starts with the stored
addresses instead of exiting with "Couldn't autodetect an IPv6 address".

**Forwarding path.** The SYN clamp lowers the TCP MSS of sessions crossing the
tunnel to the tunnel MTU less 40 bytes for IPv4 and less 60 for IPv6, and never
raises a smaller one. TCP port 179 is refused across the tunnel in both
families, so IPv6 BGP cannot establish across it either. Both rules live in
`inet` tables (`cldt-mss-<iface>`, `cldt-bgp-<iface>`); the former IPv4-only
tables of the same names are removed. A node whose tunnel carries IPv6 also has
`net.ipv6.conf.all.forwarding` enabled; a single-stack node is not asked to,
because IPv6 forwarding stops a host accepting router advertisements.

**Site transit.** A site node with no tunnel routes each remote prefix through
the relay's address of the prefix's own family: Linux refuses an IPv6 route
through an IPv4 gateway, which previously failed the whole transit pass at the
first IPv6 prefix. The IPv6 next hop is the relay's
`node-transit-address6-<node>`, the address Calico peers on, because a node's
IPv6 Kubernetes address can sit on a dummy device that answers no neighbour
solicitation on the LAN. Without a published transit address the relay's first
IPv6 node address is used; a single-stack relay gets no IPv6 routes, and a
single-stack transit hashes exactly as before.

**Host traffic to remotes.** A site node's own traffic to a remote leaves from
the node's Kubernetes address of that family, through a relay or its own tunnel
alike. The remotes accept a site node's traffic from those addresses through
whichever endpoint carries the node, so connections survive the endpoint set
changing. The kernel would otherwise pick the LAN address of a node known by an
identity address on a dummy device, which no remote accepts, or the tunnel
address of an endpoint, which goes away with the tunnel. Both broke the API
server's path to remote kubelets: with k0s's konnectivity disabled, logs and
exec dial the kubelet directly.

**Remote node addresses.** A remote's peer entry carries its Machine's
InternalIPs beside the address its tunnel dials. On AWS the tunnel dials the
public ExternalIP while the Node's address, which the API server dials, is the
VPC address. A remote's pod blocks are published when Calico confirms each one,
whichever family and however late, rather than only on Machine changes.

**Transit speaker.** When `transit.bgpPort` is set, IPv6 routes are advertised
with the node's IPv6 address (from `status.hostIPs`) as the next hop, or not at
all when the node has none.

## Required cluster settings

These are checked read-only by `cmd/acceptance preconditions` (see
[the harness README](../harness/e2e/README.md#real-cluster-acceptance)):

| Setting | Requirement | Why |
| --- | --- | --- |
| Pod MTU | At most the tunnel MTU: the endpoint's underlay MTU less 80, **1420** on a 1500-byte LAN, and the remote's fixed 1420 | A pod sends datagrams up to its own MTU unfragmented. Over a smaller tunnel the first datagram to each destination is lost and later ones are fragmented; a sender that sets don't-fragment loses every one. TCP is clamped, UDP cannot be. |
| Calico `IP_AUTODETECTION_METHOD`, `IP6_AUTODETECTION_METHOD` | Methods that find nothing on a remote, e.g. `cidr=` limited to the site's own ranges and excluding the tunnel ranges | Any method that finds an address on a remote replaces the tunnel address the product stores, on every `calico-node` restart. |
| Calico BGP peering | `BGPConfiguration` `default` with `nodeToNodeMeshEnabled: false`, and a `BGPPeer` whose `nodeSelector` and `peerSelector` are both `!has(cloud-provisioning.appmana.com/role)` | The full node mesh makes every node peer with every remote across a tunnel that refuses BGP. Calico reports a node whose configured peers never establish as not ready, so remote `calico-node` pods are never Ready and every rolling update of Calico stalls on them. Peering the site's nodes with each other only leaves the remotes with no peers, which Calico reports as ready. |
| Address ranges | Tunnel ranges disjoint from pools, Service and node addresses, and remote cloud networks outside the autodetection CIDRs | A peer's accept list would otherwise claim an address the cluster already uses. |

For k0s the pod MTU is `spec.network.calico.mtu`.

## Validation

- [VM lab, required settings](validation/k0s-1.36.4-calico-bird-dualstack-results.json):
  5,987 checks over 21 stages, every one passing: four endpoint placements,
  each remote removed and recreated under each placement, kubelet logs and exec
  through the API server and `calico-node` readiness on every node at every
  stage. Moving the endpoints between all nodes and one worker and back kept
  262 of 262 kubelet requests working; before the endpoint source fix 4 of 159
  hung for about a minute each.
- [VM lab, deployment as configured](validation/k0s-1.36.4-calico-bird-dualstack-asis-results.json):
  278 of 335 checks; every cross-tunnel don't-fragment UDP datagram of
  1449 and 1450 bytes is lost, and remote `calico-node` pods are never Ready.
- [CAPA AWS workers](validation/aws-k0s-1.36.4-calico-bird-dualstack-results.json):
  two EC2 workers in us-west-2 joined over the lab's real WAN; 2,753 checks over
  nine rows: both workers, each removed with the other surviving and recreated
  on a new instance and Node, and four endpoint placements.

See also the [tunnel scenarios](tunneling-scenarios.md#dual-stack-native-calico-bgp).
