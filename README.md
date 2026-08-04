# cloud-provisioning

Utility for provisioning nodes to on-premises, firewalled clusters.

## What it does

Joins a cloud VM (with a public IP) as a worker node to a k0s cluster that
sits behind NAT/CGNAT with no inbound connectivity — so the cluster can
place public-facing workloads (an ingress gateway, a VPN endpoint) on a
node that the internet can actually reach, while the control plane stays
private.

The user commits ONE resource — a `ProvisionedNodeClaim` — and the
controller derives everything else: the CAPI `Machine` + provider machine
pair, the tunnel-bootstrapping userdata, WireGuard address allocation,
both dialer DaemonSets, and the post-join adoption config. The claim's
spec is request-shaped (`requests: {cpu, memory}`, `arch`,
`internetFacing`) and names no cloud; which cloud fulfills it is decided
by the registered provider that owns the CAPI Cluster's infrastructure
(AWS/CAPA today — other providers slot in by implementing
`join.MachineProvisioner`, never by touching a reconciler).

The node is a normal k0s worker. Pod networking is Calico's ordinary BGP
mesh (`overlay: Never`) carried over a WireGuard underlay — no VXLAN, no
overlay reconfiguration. The tunnel does the encapsulation; Calico does
not know a WAN is involved.

## Separation of concerns (the load-bearing invariant)

The tunnel layer provides NODE-to-NODE reachability only. The only
kernel routes the dialer ever installs are host routes (`/32`/`/128`) to
peer node addresses — anything broader is rejected at parse time.
Pod/service CIDRs appear ONLY in WireGuard's cryptokey accept-list
(AllowedIPs — a packet filter, not a route source). Routing traffic to
pods is Calico's concern: bird learns pod blocks over BGP sessions that
ride the tunnel's host routes. See `controller/cmd/dialer/main.go`'s
package doc.

Three further rules the dialer enforces, each found by a live failure:

- **Never route a peer's own endpoint through the tunnel.** The
  encrypted packet's outer destination is that address, so it matches
  the same route and re-encapsulates forever (observed at line rate).
  Endpoints are read from the live device as well as config, because a
  listener's peers have no configured endpoint — it learns theirs by
  roaming.
- **Prune routes, don't just add them.** A route that becomes wrong (a
  removed peer, an address later revealed as an endpoint) must be
  removed, or it keeps looping or blackholing.
- **Validate the whole peer set before touching the kernel**, and
  install a peer's routes only once that peer is viable (it has an
  endpoint, or a handshake proves the path). A route to a peer
  WireGuard cannot send to is a blackhole.

The asymmetry matters: internal nodes (behind NAT, not WAN-reachable)
always DIAL; the remote only LISTENS and learns their addresses by
roaming.

## How a claim becomes a node

1. **Claim** — `ProvisionedNodeClaim` is committed (gitops). The claim
   reconciler resolves the CAPI Cluster, routes fulfillment to the
   provider owning its infrastructure, resolves `requests` to the
   smallest satisfying instance type from the provider's catalog, and
   creates the `Machine` (+ provider machine) with ownerRefs — deleting
   the claim cascades all the way to instance termination.
2. **Bootstrap** — the join reconciler renders cloud-init from
   `join-patterns/k0s-worker.cloud-config.tmpl` into the Machine-owned
   bootstrap Secret: a pinned-sha256 dialer binary download, the
   WireGuard listener unit (unique `cldt<hash>` interface, never `wg0`),
   a frozen peer snapshot, and a short-TTL k0s join token. CAPA launches
   the instance only once this Secret exists. The userdata tunnel is the
   ONLY way the node can ever join; cloud-init gates `k0s install
   worker` behind API-VIP reachability through it.
3. **Mesh** — on-prem dialer pods (scheduled by the controller onto
   selector-chosen Linux workers; control-plane nodes are excluded — the
   remote side of a tunnel never lands on a controller) self-generate
   their keypairs, publish public keys through the API, and dial out to
   the instance's public endpoint, which the controller mirrors from
   `Machine.status.addresses`. Remotes also peer with each other
   (fully connected mesh; isolated remotes share no LAN).
4. **Adoption** — post-join, the cloud DaemonSet runs the same dialer
   binary against a controller-rendered, public-data-only peer Secret,
   re-derived from live cluster state — how peer/CIDR corrections reach
   a node whose userdata is immutable. The bootstrap systemd unit is
   never disabled (the can't-strand-the-node floor).
5. **Network** — Calico peers BGP across the `cldt*` link and pod/
   service traffic flows. Drop the cluster's Calico MTU to ~1420 to fit
   under WireGuard overhead.

## Layout

```
controller/             Go module: endpoint-controller (claim + join +
                        mesh reconcilers) and the dialer
                        (netlink/wgctrl, no shelling out)
controller/pkg/join/    the two seams, one package per specialization:
                        k0s + kubeadm (ClusterJoinProvider), aws + docker
                        (InfraProvider/MachineProvisioner). Each keeps
                        its own knobs in its own provider-config Secret
controller/pkg/tunnel/  the shared wire contract: Secret key schema,
                        peers-file shape, cldt* interface naming
join-patterns/          versioned cloud-init templates, one per join
                        mechanism — rendered by the join reconciler,
                        never hand-typed
manifests/wg-dialer/    reference deploy: CRD, RBAC (derived from code),
                        controller Deployment, claim example
manifests/cluster/      the externally-managed Cluster/AWSCluster pair a
                        claim resolves against
harness/netns-routing/  single-NIC routing e2e for the real dialer in
                        network namespaces (fast; the safety invariants)
harness/kind-e2e/       run-live.sh: ONE claim becomes a real second node
                        on a kind cluster via CAPD + kubeadm, over a real
                        tunnel, and one delete removes it. run.sh: the
                        same declarative fan-out against shim CRDs
harness/vm-single-nic/  real single-NIC VM (containerlab + vrnetlab),
                        real k0s, real boot/reboot — see its README
scripts/aws/            one-time IAM bootstrap for the least-privilege
                        harness identity
```

## Verification

- `controller/`: `go test ./...` — includes the routing invariants
  (parse-time refusal of any non-host kernel route, endpoints never
  routed through their own tunnel), mesh derivation, claim expansion
  and teardown, and join rendering against fakes.
- `harness/kind-e2e/run-live.sh`: the whole contract on a real API
  server, through the product's own abstractions — one claim becomes a
  real kubeadm-joined node over a real WireGuard tunnel, and one delete
  removes it. **WAN reachability is asserted at every phase on every
  node**: the failure this project exists to prevent is a tunnel
  costing a node its default route, and checking only at the end cannot
  tell "never broke" from "broke and recovered". Requires kind,
  clusterctl and docker; the assertions run inside each node's netns
  with host tools, since the node image ships neither `wg` nor `ping`
  (an assertion that shells into the node for those silently passes on
  empty output — it did, once).
- `controller/pkg/join/aws`: real-AWS integration tests (skipped without
  EC2-capable credentials): the instance-type catalog is verified
  against `DescribeInstanceTypes`, and the rendered AWSMachine spec is
  proven launchable by actually running (and terminating) a tagged
  t4g.nano — same tag-scoped identity conventions as
  `scripts/aws/bootstrap-harness-iam.sh`.
- `harness/vm-single-nic/`: a real, single-NIC Ubuntu VM (real
  QEMU/KVM systemd/PID1 boot, real k0s via k0sctl) running the
  production dialer binary. Single-NIC by design: multi-NIC rigs hide
  exactly the routing mistakes this project exists to prevent. This is
  the level that exists because the jarvis incident's mechanism —
  kubelet resurrecting a stale DaemonSet pod faster than any
  reconcile-based fix can intervene — needs a real init system and a
  real kubelet to reproduce at all.

Historical simulation harnesses (netns scripts, containernet, the
wg-pullup shell dialer, aws-bringup) were retired with the E1/E2-era
code they tested: the shell dialer and the hand-rendered join path no
longer exist, and the safety property they probed for is now a
parse-time invariant with unit tests instead of a live RED control.

## History

The first deployment attempt (hilton, 2026-07) ended with a route
hijack: a hardcoded `AllowedIPs=0.0.0.0/0,::/0` fed into a kernel-route
loop installed a default route via the tunnel on the on-prem node. The
redesign makes that class impossible rather than discouraged: AllowedIPs
and kernel routes are separate inputs end to end, and the dialer refuses
any kernel route broader than a single host at parse time.
