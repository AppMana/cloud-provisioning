# Tunnel handover

## Receive ownership constraint

WireGuard AllowedIPs control both egress peer selection and accepted ingress
sources. A prefix has one peer owner per device; overlapping generations need
separate devices and keys. Keep workload identity addresses on a stable owner,
independent of generation-device lifetime. Handover must preserve physical
NIC configuration, Node identity, CNI settings, and allocated addresses.

The current remote applies its new peer list before publishing its
acknowledgment. The site subsequently reads that acknowledgment and changes
egress. During this interval the remote can reject packets sent through the
previous owner. Reversing that order moves the interval to the other side.
Shorter polling can reduce the interval but does not supply an overlap period.

Keep the existing remote-application and source-ownership checks. Removing
either can break the API path required to receive the corrective configuration.
Do not permit duplicate AllowedIPs in the shared device compiler: the kernel
experiment confirms that this would not implement dual receive ownership.

## Proposed protocol: two tunnel generations

This protocol is an implementation design, not an enabled feature. Its purpose
is to prepare a complete second receive path before moving egress. A generation
contains a complete, validated peer graph and its endpoint placement. Each
participant has a separate WireGuard device, key and UDP port for each live
generation. Reusing keys between devices risks endpoint roaming between them
and must be rejected by the protocol validator.

These are virtual tunnel devices. Each VM retains one physical NIC and its
distribution's supported CNI. The protocol must not alter the physical address,
Node identity, CNI configuration, or allocated workload and tunnel addresses. Keep address ownership separate from generation-device ownership. The Linux
packet experiment places the stable address on loopback, outside the two
WireGuard devices. CNI and production protocol qualification remain required
before enabling it. The existing single-device backend remains a distinct
implementation, not a substitute for that address-owner capability.

| Phase | Required action | Condition for advancing |
| --- | --- | --- |
| Prepare | Keep generation A serving. Publish generation B and install its receive devices and isolated routes. | Every required participant verifies B's devices, authenticated peer paths, routes, and source isolation. |
| Switch | Select B for new egress on each participant while retaining both receive generations. | Every required participant acknowledges its actual egress selection for B. |
| Drain | Keep A receiving in-flight traffic. Prevent further egress selection of A. | All switch receipts remain valid and the documented drain policy is satisfied. |
| Retire | Remove only A's devices, keys and owned routes. | Read-back confirms B remains intact and A's resources are absent. |

Each receipt must bind the cluster, transition ID, immutable plan hash,
generation, Node UID, device public key and phase. Configuration publication is
not an application receipt. Publish receipts only after native read-back;
compare-and-swap protects against a changed plan or replaced Node. A stale
receipt, receipt for a different phase, or changed membership must not advance
the transition. Concurrent placement changes must be serialized or explicitly
supersede the pending plan without creating a third live generation.

Round membership includes every site in the union of previous and desired
receive paths. A site removed from desired placement can still send traffic
until its switch and drain are verified; desired-site receipts alone cannot
authorize its retirement. Cloud egress selects only desired owners, while
retained senders keep their independent return paths and participate in every
phase, including rollback. Missing retained-site identity or readiness blocks
preparation, and recovery rejects a journal that omitted that participant.

An unavailable participant prevents a planned seamless transition from
advancing. Forced removal or failure recovery needs a separate policy and
cannot claim uninterrupted traffic to the unavailable participant. A timer
alone cannot stand in for a missing switch receipt. The drain policy must state
its packet-lifetime assumptions; finite waiting cannot prove delivery under
unbounded delay.

Before switching, rollback removes B and leaves A intact. After any participant
switches, rollback must coordinate a switch back while both receive generations
remain installed. Restart recovery reads persisted transition state and native
devices; it must not infer retirement from process memory or delete all tunnel
devices during initialization.

## Implemented receipt-round binding

The shared [handover package](../controller/pkg/handover/round.go) implements
receipt-round identity and validation. It is not connected to the production
publisher or platform backends. Its plan hash must refer to the complete,
validated public peer graph; graph validation is a separate requirement.

Create a round once for a phase attempt with `NewRound`, then persist it with
a storage compare-and-swap before publication. The binding includes cluster
and transition UIDs, graph hash, direction, phase, every participant's Node UID
and both device keys, a fresh nonce, expiry, and receipt freshness policy.
Generation keys must be distinct across all participants and both generations.
Use `DecodeRound` on restart; it rejects unknown or trailing persisted state.
An observation timeout resumes the same round rather than creating a new nonce.

`Round.Ready` requires a complete set of fresh receipts from the expected node
identities. The transport supplies `AuthenticatedNodeUID`; JSON payloads cannot
populate it. A changed membership invalidates survivor receipts as well as the
replaced node's receipt. A receipt for selecting B cannot authorize a rollback
to A, and a receipt from an earlier attempt cannot satisfy a fresh round.

Hosts still need to perform native phase verification before issuing receipts.
Pod receipt authentication requires a controller-configured container name,
digest-pinned image, and expected runtime image ID. Both the DaemonSet template
and submitting pod must request that image, and its container must be running
and Ready with the expected image ID. Supply architecture-specific runtime IDs
from trusted release artifacts, not from the submitting pod's status. This
checks the container image; a container executing a mounted host binary still
requires separate executable-version admission. Receipt authentication therefore
rejects command overrides in both the DaemonSet template and the submitting
pod, including the cloud host-binary fallback. The admitted release must run
its image entrypoint; pinning the fallback container's image does not admit the
host executable. This restriction does not itself attest mounted executable
contents or enable transition execution.
Receipts also carry `agentContainerID`, which must match the admitted container's
current runtime ID. The observation must not predate its known start time.
Pod credentials and Node boot IDs can remain valid across container restarts;
neither permits replaying a previous container's observation. The sender must
read its current container identity and repeat native verification after a
restart. These API checks do not fence a restart after authentication; native
phase execution and transition fencing remain separate requirements.
`Transition` now persists and replays prepare, switch, drain, and retire history.
Rollback during prepare aborts B; rollback after switching selects A, drains B,
and retires B. Expired rounds require explicit renewal and fresh receipts. Drain
receipts must be observed after the configured drain deadline.

`Store` saves each successor in a controller-owned ConfigMap using Kubernetes
resource-version compare-and-swap. Concurrent writes and object recreation are
rejected; callers must reload authoritative state before recomputing. Receipt
writers must lack access to this storage: persisted authentication is a trusted
controller assertion. Publish only a successfully committed snapshot.

## Platform implementation boundaries

The shared protocol owns generation identity, phase validation, peer graph
validation, receipts and recovery decisions. Platform backends own device
creation, route installation, native verification, egress selection and removal
of the specified generation. Backend methods must report unsupported operations
explicitly. Avoid OS branches in the protocol state machine.

Linux needs dedicated route ownership for each generation and a verified
egress switch that preserves WireGuard's outer-packet routing exemption.
The Linux `prepareStableAddressOwner` component creates an address-only dummy
device bound to mesh UID, Node UID, root public key, and the allocated host
addresses. Initial preparation requires those addresses on the admitted legacy
WireGuard device and rejects ownership conflicts. It leaves legacy addresses
intact; the stable owner can survive generation-device deletion and recover
missing addresses from the same admitted allocation. It is not connected to
normal reconciliation: coordinated retirement, allocation release, and phase
fencing must be integrated before enabling it. Native component tests do not
establish CNI compatibility or deployment qualification.
Route selections bind the complete prepared receive-permission set, including
permissions retained during drain. Before changing routes or policy rules, the
Linux installer rejects missing, expanded, or withdrawn kernel permissions;
coverage of the selected destination alone is insufficient.
After legacy-device retirement, the route installer accepts only a complete
receiver-only destination set with no unresolved claims or relay state. It
skips legacy main-table migration when that device is absent and rejects a
non-WireGuard interface occupying its name. This does not authorize retirement;
the coordinated phase executor must establish that it is safe first.
Legacy retirement is peer-scoped because one site device can serve several
cloud workers. The Linux withdrawal component removes only the admitted peer,
preserving the shared device, addresses, listener and other peer configuration.
It rejects identity or permission drift, overlapping permissions on another
legacy peer, and legacy routes in any table that overlap the retiring sources,
including default and multipath routes. It does not remove those routes itself.
This component also remains disconnected from normal reconciliation: a fenced
executor must verify replacement routing and committed drain receipts, and
persist retirement so the ordinary publisher cannot recreate the old peer.
Linux reconciliation serializes recovery reads, forwarding changes, receipts
and BGP updates using a per-interface kernel file lock on the existing shared
identity mount. Shutdown cleanup takes the same lock. The bootstrap claim still
determines which peer source should be active; the lock prevents overlapping
passes from observing and applying different snapshots concurrently. The lock
file is retained across owners, and process exit releases the kernel lock.
All participating binaries must implement this serialization before it can
fence native phase execution. It does not replace journal authorization or
durable phase state.
Windows needs independent adapter lifetimes, route and source-address behavior,
and HNS/VFP isolation checks on Server 2022 and Server 2025. The current
single-device `Apply`/`Close` contract is insufficient to represent preparation,
selection and retirement independently; do not overload `Close` to select a
generation. Bootstrap-to-service adoption must preserve both generations and
their keys while a transition is active.

## Windows transport isolation

Each generation must reach its peer endpoints without depending on a retiring
or legacy mesh adapter. A route to another worker's private or public address
may already belong to the retained cluster tunnel. Creating a second adapter
does not establish an independent transport path. In
[WireGuardNT 1.1's route resolver](https://git.zx2c4.com/wireguard-nt/tree/driver/socket.c?h=1.1),
route selection excludes the device's own LUID; it does not exclude every other
mesh adapter. Transport verification therefore needs to consider all live and
legacy generations.

The [native stable-owner experiment](../controller/pkg/tunneldevice/testdata/stable-owner-windows/README.md)
checks endpoint routes before creating adapters and rejects a selected `cldt`
interface. This
host experiment does not qualify production handover. A production backend
must own and verify transport exemptions across all live generations, then
pass workload and recovery gates in addition to the native host-source checks. Never replace production endpoint
routes merely to make an experiment pass.

## Qualification gates

First exercise two real receive paths with deliberate delays between each
phase, including packets still arriving through A after egress selects B. Use
VMs and native device read-back; then derive unit tests from those observations.
Verify successful authorized traffic and rejected spoofed sources in IPv4 and
IPv6. Two devices must not broaden any peer's source permissions.

Run all directed survivor pairs through each supported placement transition,
including site-to-site, site-to-cloud and cloud-to-cloud paths. Retain failed
requests and original stream identities. Test API and kubelet access through
each control plane separately. Inject lost receipts, delayed participants,
controller and dialer restarts, Node replacement, rollback, and a second
placement request during each phase. Repeat with AWS workers and both Windows
versions before claiming support beyond the tested Linux VM topology.

Only after those gates pass should the protocol replace the current handover
path. Until then, the existing acknowledgment safeguards and the documented
continuity limitation remain in effect.
