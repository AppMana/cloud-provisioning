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
Do not permit duplicate AllowedIPs in the shared device compiler: a single
device cannot implement dual receive ownership.

## Protocol: two tunnel generations

The two-generation handover protocol prepares a complete second receive path
before moving egress. A generation contains a complete, validated peer graph
and its endpoint placement. Each participant has a separate WireGuard device,
key and UDP port for each live generation. Reusing keys between devices risks
endpoint roaming between them and is rejected by the protocol validator.

These are virtual tunnel devices. Each VM retains one physical NIC and its
distribution's supported CNI. The protocol must not alter the physical address,
Node identity, CNI configuration, or allocated workload and tunnel addresses.
Keep address ownership separate from generation-device ownership (for example,
placing the stable address on an address-only owner outside the two WireGuard
devices).

| Phase | Required action | Condition for advancing |
| --- | --- | --- |
| Prepare | Keep generation A serving. Publish generation B and install its receive devices and isolated routes. | Every required participant verifies B's devices, authenticated peer paths, routes, and source isolation. |
| Switch | Select B for new egress on each participant while retaining both receive generations. | Every required participant acknowledges its actual egress selection for B. |
| Drain | Keep A receiving in-flight traffic. Prevent further egress selection of A. | All switch receipts remain valid and the documented drain policy is satisfied. |
| Retire | Remove only A's devices, keys and owned routes. | Read-back confirms B remains intact and A's resources are absent. |

Each receipt binds the cluster, transition ID, immutable plan hash, generation,
Node UID, device public key and phase. Configuration publication is not an
application receipt. Publish receipts only after native read-back;
compare-and-swap protects against a changed plan or replaced Node. A stale
receipt, receipt for a different phase, or changed membership does not advance
the transition. Concurrent placement changes are serialized or explicitly
supersede the pending plan without creating a third live generation.

Round membership includes every site in the union of previous and desired
receive paths. A site removed from desired placement can still send traffic
until its switch and drain are verified; desired-site receipts alone cannot
authorize its retirement. Cloud egress selects only desired owners, while
retained senders keep their independent return paths and participate in every
phase, including rollback.

Before switching, rollback removes B and leaves A intact. After any participant
switches, rollback coordinates a switch back while both receive generations
remain installed. Restart recovery reads persisted transition state and native
devices; it never infers retirement from process memory or deletes tunnel
devices during initialization.

## Receipt-round binding and journal storage

The shared [handover package](../controller/pkg/handover/round.go) implements
receipt-round identity, transition history, journal storage and agent admission:

- **Round binding (`NewRound` / `DecodeRound`):** Binds cluster and transition
  UIDs, graph hash, direction, phase, every participant's Node UID and both
  device keys, a fresh nonce, expiry, and receipt freshness policy. Generation
  keys must be distinct across all participants and both generations.
- **Receipt authentication (`Round.Ready`):** Requires a complete set of fresh
  receipts from the expected node identities, authenticated via pod-bound
  tokens, digest-pinned container images, and current `agentContainerID` runtime
  identities.
- **Transition history (`Transition`):** Persists and replays prepare, switch,
  drain, and retire history in a controller-owned ConfigMap (`Store`) using
  Kubernetes resource-version compare-and-swap. Optional
  `journal.expiryRecovery: "rollback"` requests rollback when a published
  prepare, switch, or drain round expires.
- **Inventory and RBAC:** Normal controller reconciliation reserves each
  immutable Machine UID in a controller-owned inventory ConfigMap before
  creating its journal and grants agents named `get`-only access to the
  inventory and reserved journals.
- **Checkpointing and maintenance:** The agent execution boundary holds the
  reconciliation lock through checkpoint admission, platform phase execution,
  live identity revalidation and receipt construction. Completed journals use a
  maintenance callback under the same lock to restore committed devices, stable
  address owners, and selected routes without replaying retired receivers.

## Platform implementation boundaries

The shared protocol owns generation identity, phase validation, peer graph
validation, receipts and recovery decisions. Platform backends own device
creation, route installation, native verification, egress selection and removal
of the specified generation:

- **Linux:** Uses an address-only dummy device (`prepareStableAddressOwner`) for
  stable host addresses, dedicated route ownership per generation, and a
  per-interface kernel file lock on the shared identity mount to serialize
  recovery reads, forwarding changes, receipts and BGP updates. Receiver route
  read-back revalidates device identity, permissions, authenticated transport
  and policy rules before issuing receipts.
- **Windows:** Requires independent adapter lifetimes, route and source-address
  behavior, and HNS/VFP isolation checks on Server 2022 and Server 2025. Each
  generation must reach its peer endpoints without depending on a retiring or
  legacy mesh adapter, because [WireGuardNT 1.1's route resolver](https://git.zx2c4.com/wireguard-nt/tree/driver/socket.c?h=1.1)
  excludes only the device's own LUID.

## Qualification gates

Exercise two real receive paths with deliberate delays between each phase,
including packets arriving through A after egress selects B. Verify successful
authorized traffic and rejected spoofed sources in IPv4 and IPv6. Run all
directed survivor pairs through each supported placement transition (site-to-site,
site-to-cloud and cloud-to-cloud), and inject lost receipts, delayed
participants, controller and dialer restarts, Node replacement, rollback, and
concurrent placement requests during each phase.
