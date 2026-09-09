# Tunnel handover

Endpoint placement currently converges, but is not qualified for uninterrupted
traffic. The [source-routing report](validation/microk8s-source-routing-results.json)
records packet loss during both endpoint withdrawal and promotion. A successful
worker replacement at fixed placement does not qualify these transitions.

## Receive ownership constraint

WireGuard's AllowedIPs select an outbound peer and constrain the source
addresses accepted from that peer. On the Linux kernel tested in a QEMU VM,
assigning an identical prefix to another peer removes it from the previous
peer. Keeping both peer objects does not keep both owners. The
[kernel experiment](validation/handover-ownership-results.json) covers IPv4 and
IPv6 host and pod prefixes, repeated application, reversal, and independent
ownership on two devices. It does not measure packet delivery.

A separate [three-VM packet experiment](validation/handover-packet-results.json)
verifies IPv4/IPv6 receive overlap, source rejection, and continued new-path
traffic after old-device removal. Its replies are bound to their receive
device; it does not qualify automatic egress selection or the production
receipt protocol. The [native runner](../controller/pkg/tunneldevice/testdata/handover-vm/README.md)
reproduces that experiment without rebuilding the Kubernetes cluster. The
[extended egress experiment](validation/handover-egress-results.json) also checks
unbound sockets across route selection, rollback, reselection and retirement.
Its isolated Linux routes are distinct from the production policy-routing
integration, which still needs implementation and qualification.

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
Node identity, CNI configuration, or allocated workload and tunnel addresses.
Keep address ownership separate from generation-device ownership. The Linux
packet experiment places the stable address on loopback, outside the two
WireGuard devices. The [Windows address experiment](validation/windows-generation-address-results.json)
failed when assigning one identity address to two adapters through the current
backend; changing DAD settings did not qualify that model. A separate stable
address owner has [native Windows host packet evidence](validation/windows-stable-owner-packet-results.json)
for IPv4/IPv6 route switching, rollback and old-adapter retirement on Server 2022
and 2025. The [host-source isolation experiment](validation/windows-stable-owner-isolation-results.json)
adds positive controls through both generations and rejection during every
handover phase. CNI and production protocol qualification remain required
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
`Transition` now persists and replays prepare, switch, drain, and retire history.
Rollback during prepare aborts B; rollback after switching selects A, drains B,
and retires B. Expired rounds require explicit renewal and fresh receipts. Drain
receipts must be observed after the configured drain deadline.

`Store` saves each successor in a controller-owned ConfigMap using Kubernetes
resource-version compare-and-swap. Concurrent writes and object recreation are
rejected; callers must reload authoritative state before recomputing. Receipt
writers must lack access to this storage: persisted authentication is a trusted
controller assertion. Publish only a successfully committed snapshot.

The phase tests cover restart, rollback, renewal, chronology, receipt replay and
drain timing. An isolated Kubernetes 1.35 API server verifies stale-write and
recreated-object rejection. The package passes the race detector with 86.5%
statement coverage. Production publisher/backend wiring and deployment-specific
drain policy validation remain required before these primitives can authorize
native transitions. Existing production delivery
nonces, native-application checks and source-ownership safeguards remain active.
The [receipt-gate validation report](validation/handover-round-results.json)
records unit and race-test scope; it does not qualify runtime handover.

## Platform implementation boundaries

The shared protocol owns generation identity, phase validation, peer graph
validation, receipts and recovery decisions. Platform backends own device
creation, route installation, native verification, egress selection and removal
of the specified generation. Backend methods must report unsupported operations
explicitly. Avoid OS branches in the protocol state machine.

Linux needs dedicated route ownership for each generation and a verified
egress switch that preserves WireGuard's outer-packet routing exemption.
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
interface. Its [validation evidence](validation/windows-stable-owner-results.json)
records the failed warmup attempts and native preflight rejection. The
[relay-backed VM experiment](validation/windows-stable-owner-packet-results.json)
uses an independently reachable endpoint and validates stable-owner traffic
across selection, rollback and retirement with an independent sampler. This
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
