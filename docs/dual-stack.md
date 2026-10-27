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
| Pod MTU | At most the smallest tunnel MTU on the path. Windows derives its budget from peer egress MTUs less 80, capped at 1420; a 1450-byte egress requires 1370. Linux endpoints use their configured or derived tunnel MTU. | A pod sends datagrams up to its own MTU unfragmented. Over a smaller tunnel the first datagram to each destination is lost and later ones are fragmented; a sender that sets don't-fragment loses every one. TCP is clamped, UDP cannot be. |
| Calico BGP peering | Every `BGPPeer` the site configured leaves cloud nodes out: `!has(cloud-provisioning.appmana.com/role)` is a conjunct of its `nodeSelector` (and of its `peerSelector`, if set), or it names a site node. | The tunnel refuses BGP, and Calico reports a node whose configured peers never establish as not ready. The product replaces the full node mesh with BGPPeer `cloud-provisioning-site-mesh` among site nodes, and runs Calico on remotes with their addresses stated ([Calico address ownership](windows-gateway.md#calico-address-ownership)); it does not change peers the site configured, and refuses tunnel-class claims while one reaches a cloud node. |
| Address ranges | Tunnel ranges disjoint from pools, Service and node addresses, and remote cloud networks outside the autodetection CIDRs | A peer's accept list would otherwise claim an address the cluster already uses. |

For k0s the pod MTU is `spec.network.calico.mtu`.

For a site with native AWS workers, choose a common workload MTU that fits
every participating tunnel and workload-facing interface. A smaller destination
pod interface can also return an ICMP fragmentation-needed error: lowering only
the site's MTU can move a cold-path UDP failure to the cloud-to-site direction.
TCP success and a warmed path-MTU cache do not qualify the UDP path.

Configure each CNI through its existing owner: k0s Calico uses
`spec.network.calico.mtu`, Linux AWS VPC CNI uses `POD_MTU` (rendered by
`aws/vpc_cni.py --pod-mtu`), and the patched Windows VPC CNI uses the optional
`podMTU` setting described in the [plugin documentation](../providers/aws-vpc-cni-plugins/README.md).
Do not lower the physical management interface MTU to configure workloads.
Changing a template does not update existing pod interfaces. Verify fresh pods'
actual interface MTUs on every platform, preserve existing-workload continuity,
and test the first UDP datagram on new directed paths before repeat traffic.
These native CNI runtime checks require retained observations; a successful
configuration preflight alone does not establish them.
