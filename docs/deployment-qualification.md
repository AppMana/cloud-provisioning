# Deployment qualification

A release requires fresh measurements of one immutable candidate. Historical
results do not qualify changed binaries or a different cloud CNI.

## Safety controls

- The Linux dialer rejects a non-WireGuard interface before changing its
  addresses or MTU, and refuses to delete it during retirement. It validates
  the entire requested address list before creating or changing a tunnel.
- Tunnel policy setup checks both IP families before adding rules. Conflicting
  marks, masks, selectors, actions, tables or priorities fail without replacing
  foreign policy. Teardown matches the configured mark and lookup shape rather
  than deleting every exemption at the shared priority. These ownership checks
  do not qualify arbitrary higher-priority policy routing on a deployment site.
- `acceptance matrix` refuses to create probes when required preconditions fail.
- BGP preflight requires the documented site mesh and checks every additional
  peer for cloud exclusion. External router peers can retain the Linux-only
  selector, conjuncted with `!has(cloud-provisioning.appmana.com/role)`.
  Other selector expressions remain unqualified; do not delete router peers
  merely to make this check pass.
- `siteDialer.nodeSelector` bounds initial privileged site-agent admission,
  independently of `tunnel.endpoints`. Admission pins node names and retains
  existing agents when labels are removed. Removal is deliberately not an
  automatic rollout mechanism. Set both selectors for a canary. An empty
  admission selector preserves legacy all-Linux-site placement.
- The optional AWS allocation observer checks current Machine UID, Node UID,
  provider ID, account, region, VPC, subnet, ownership tag and attached ENIs.
  It publishes only secondary IPv4 /32s or delegated IPv6 /80s, polls allocation
  changes, rejects overlapping remote ownership and withdraws released IPs.
  API failure preserves previous state and fails reconciliation. AWS nodes do
  not receive Calico address annotations. Masquerade conflicts block publication.
- BGP transit distinguishes failed reads from authoritative empty state and
  withdraws the last deleted worker's routes.
- The dual-stack VM site supports `-k0s-calico-site-only`, bounding Calico
  installation and both IPAM families to pre-labelled site nodes.
- `awsrow -pod-family IPv4|IPv6 -k0s-calico-site-only` measures separate
  single-family campaigns. IPv6-only probes include TCP, Service, DNS, large
  transfers and UDP; missing requested families fail instead of being skipped.
  Every AWS matrix also compares the source IP reported by the destination
  pod with the sending pod IP for each directed pod path. Sender-local socket
  addresses alone cannot establish source preservation.
  Native campaigns also require one Ready CNI agent per measured node,
  Calico confined to site nodes, AWS CNI confined to cloud nodes, and a single
  AWS-first CNI configuration on each cloud worker. Competing plugins fail
  the ownership check even when existing workload probes still pass.

These safeguards do not implement or prove the entire routing/migration
contract. In particular, a successful allocation observation does not establish
source preservation, return routing, rollback, or compatibility with live routers.

## AWS observer configuration

Use `awsPodObserver.configMap` containing `scope.json`; optionally supply
`awsPodObserver.credentialsSecret` with SDK credential environment variables.
Prefer refreshable role credentials. The observer needs only
`ec2:DescribeNetworkInterfaces`; it does not allocate ENIs or change AWS routes.
CAPA provisioning credentials and AWS-node IPAM credentials remain separate.

For installations without workload identity, set
`awsPodObserver.credentialsFileSecret` instead of `credentialsSecret`. The
Secret must contain `credentials.json` with the STS response's `Credentials`
object: `AccessKeyId`, `SecretAccessKey`, `SessionToken` and RFC3339
`Expiration`. The chart mounts the entire directory read-only and grants the
nonroot controller group read access. Do not use a `subPath` mount: it would
prevent projected Secret rotation from reaching the process.

A separate trusted credential producer must update the Secret well before
expiration, allowing for Kubernetes projection delay. The observer's SDK cache
refreshes one minute before expiry and rereads the file without restarting the
controller. Missing, malformed or expired replacements fail observation rather
than falling back to other credentials. Existing observed allocations retain
the observer's existing API-failure behavior. This option does not renew CAPA,
harness or registry credentials, and it does not itself issue credentials.
Qualify rotation across an actual expiry boundary, including producer outage,
before relying on this mechanism for unattended operation.

The isolated AWS harness provides `aws/observer_credentials.py` as a separate
producer. Run it with setup credentials that remain refreshable for the entire
campaign; the one-hour harness federation session cannot issue these sessions.
From `harness/e2e`:

```sh
python3 aws/observer_credentials.py --work-dir "$CAMPAIGN_DIRECTORY" \
  --runner "$ISOLATED_RUNNER" --cluster-uid "$KUBE_SYSTEM_UID" \
  --receipts "$FRESH_PRIVATE_RECEIPTS_DIRECTORY" --duration 86400
```

Set `awsPodObserver.credentialsFileSecret=native-pod-observer-file` in the
candidate deployment. The producer verifies the distinct Docker daemon,
explicit cluster UID, AWS account and Secret ownership before each renewal.
It issues region-limited `ec2:DescribeNetworkInterfaces` credentials and renews
ten minutes before expiry. Secret replacement uses its existing resource
version. It does not change IAM policies, networking or controller workloads.
A failed or ambiguous write stops the process without automatic retry; inspect
the actual Secret before restarting. Retain all receipts and require terminal
completion; a missing terminal receipt is incomplete, including process death.
Successful Secret publication alone does not establish projection to the Pod
or successful observer API calls across expiry.

Do not enable the Windows gateway adapter in the same deployment.

```json
{
  "account": "REPLACE", "region": "us-west-2", "vpc": "vpc-REPLACE",
  "subnets": ["subnet-REPLACE"],
  "namespace": "cloud-provisioning", "cluster": "qualification",
  "ownerTag": "REPLACE", "ownerValue": "REPLACE",
  "family": "ipv4", "podRanges": ["172.29.0.0/24"]
}
```

Ranges are validation boundaries, never per-peer route advertisements. All
observed ENIs must carry the configured ownership tag, including IPAM-created
secondary ENIs. IPv4 qualification uses secondary-IP mode; IPv6 requires an
IPv6 range and delegated /80s. Register cloud nodes with both
`cloud-provisioning.appmana.com/role=cloud-worker` and
`cloud-provisioning.appmana.com/cni=aws-vpc` before CNI installation. Configure
site-only Calico first. The observer alone does not install AWS VPC CNI.

For a separate native IPv6 campaign, use `aws/bootstrap.py --ipv6` when
creating its infrastructure and `aws/native_access.py --family ipv6` when
configuring IPAM and observation. The recorded VPC and subnet IPv6 ranges feed
the existing CAPA claim's IPv6 configuration and primary-address assignment.
Use `aws/vpc_cni.py --family ipv6` and `awsrow -pod-family IPv6` for that
campaign. Changing the family of an existing native CNI campaign is rejected.

## Qualification order

From `harness/e2e`:

```sh
go run ./cmd/qualification -list
go run ./cmd/qualification -evidence .state/qualification/evidence.json
```

Keep evidence outside the source tree. Populate rows only from retained
measurements of one immutable candidate. Each row names its candidate, status,
relative artifact path and SHA-256, attempts/failures, management and site
failures, recovery/duration seconds, campaign worker count and cloud CNI.
`workers` means campaign participants (including deletion tests), not remaining
workers after cleanup. The gate verifies file hashes and row completeness;
review the raw artifacts for the stated scope. The qualification command is
read-only. The `deploy-qualified` command below enforces these checks before
its own apply operation; direct Helm, kubectl and GitOps writes are not gated.

Run artifact verification on Linux. The verifier streams regular files beneath
the evidence JSON directory and rejects paths or symlinks that escape it.
Symlinks resolving within that directory are allowed. Keep the evidence
directory immutable during verification and deployment; a matching hash does
not authenticate the producer or prove that an artifact measures its claimed
scenario.


## Applying a qualified manifest

The `release/immutable-artifacts` row must hash a deployment bundle JSON with
this schema. Retain the remaining immutable version and binary evidence in the
site version-bundle artifacts. Both manifests must be the exact rendered
resources used in qualification, with immutable image references and the
reviewed effective configuration. File paths are relative to the evidence JSON
directory. Each cluster identity is its `kube-system` namespace UID.

```json
{
  "candidate": "FULL_40_CHARACTER_COMMIT",
  "targets": [
    {"site": "site-a", "kubeSystemUID": "FIRST_CLUSTER_UID", "manifest": "site-a.yaml", "sha256": "MANIFEST_SHA256"},
    {"site": "site-b", "kubeSystemUID": "SECOND_CLUSTER_UID", "manifest": "site-b.yaml", "sha256": "MANIFEST_SHA256"}
  ]
}
```

From `harness/e2e`, first run the command without `-apply`:

```sh
go run ./cmd/deploy-qualified \
  -evidence /private/qualification/evidence.json \
  -candidate FULL_40_CHARACTER_COMMIT -site site-a \
  -kubeconfig /private/target.kubeconfig -context EXPLICIT_CONTEXT
```

It rejects failed or incomplete qualification before any cluster request,
verifies both manifest hashes, checks the selected cluster identity and runs
`kubectl apply --dry-run=server`. Adding `-apply` repeats the identity check
and applies the same verified bytes. An ambiguous apply failure is never
retried automatically: inspect actual resources before another attempt.
The bundle is limited to 1 MiB and each rendered manifest to 64 MiB.

The gate rejects mutable references in container, init-container and ephemeral
container image fields, including nested workload templates and Lists. It also
checks the controller's dialer, cloud and Windows image flags and requires
canonical SHA-256 values for configured dialer/CNI bootstrap downloads.
Duplicate YAML keys and duplicate artifact flags fail closed.

This entry point trusts the evidence producer and does not validate the
semantics of raw measurements. Images embedded in shell scripts, ConfigMaps,
Secrets or arbitrary operator configuration still need producer review; the
static check cannot establish their runtime interpretation or availability. Keep
kubeconfig, credentials, context selection and evidence unchanged throughout
the operation. Control competing GitOps and Helm writers separately. It does
not arm node-local rollback, assess rollout health, sequence canary expansion,
or enforce admission on other clients. Those deployment requirements remain
necessary before production use; a successful server dry run does not satisfy
them. Do not regenerate or edit manifests between qualification and apply.

## Required campaign coverage

Row IDs use `site-a` for the first site profile and `site-b` for the second.
Keep the mapping to actual cluster identities in the private qualification
bundle. Both profiles remain required; aliases must not be used to count one
cluster's measurements twice. Evidence with older row IDs must be explicitly
mapped and reviewed before it can satisfy the gate.

Each site/family campaign also requires `agent-upgrade` evidence. Record
`upgradeFrom` as the full immutable predecessor commit and set
`upgradeObserved` only after observing that transition to the candidate.
A restart of the same version does not satisfy this row. Upgrade measurements
require continuous management observation and zero retained packet failures;
bounded fault-recovery results cannot substitute for an uninterrupted upgrade.

Upgrade the image through its owner. The endpoint controller reconciles both
dialer DaemonSets from its `--dialer-image` argument, rendered from the chart's
`dialerImage` values. Direct DaemonSet image patches can be overwritten during
the measurement and cannot establish the intended predecessor or candidate.
Wait for the controller deployment to converge, then verify both DaemonSets'
desired image before observing their rollouts. Retain the resource snapshots.
Require exactly one nonterminating, Ready agent on every intended node, the
expected immutable runtime image identity, and a changed Pod UID at each
transition. A successful `rollout status` alone does not prove the requested
version ran: another writer can change the desired revision while it waits.

For receipt-enabled agents, update the expected runtime image identities in
the controller's receipt policy alongside its dialer image setting. Verify
the running image identity on every architecture after the owner reconciles
the DaemonSets. Keep the policy's retirement authority, freshness limits and
historical evidence unchanged during this image-only upgrade.

Set `image.digest` and `dialerImage.digest` to their tested `sha256:` manifest
digests, with plain registry/repository names in the corresponding `repository`
values. A digest takes precedence over the tag and chart application version.
These fields pin image selection; they do not establish that the selected
artifacts passed qualification. Include the rendered chart and effective values
in the immutable release bundle, including any separate cloud or Windows image
overrides.

For a multi-architecture release, retain the index digest, each platform's
manifest digest and its executable checksum. Verify the runtime-reported image
identity on each architecture; it can identify the selected platform manifest
rather than the requested index. Bind receipt policy to those verified runtime
identities instead of assuming they equal the publication index. An amd64 image
resolution check does not qualify ARM64 execution. Moving unchanged binaries
from a single-platform reference to an index tests artifact selection and
rollout continuity, but does not satisfy the version-changing `agent-upgrade`
requirement by itself.

Apply platform coverage checks to the cluster's prerequisite images as well:
kube-proxy, the selected CNI and its init containers, pause, and API-access
proxies must support every node platform on which their owners schedule them.
A SHA-256 pin can identify an amd64-only manifest even when its upstream tag
has an ARM64 variant. Inspect the effective owner configuration and rendered
workloads, retain the platform descriptors, and verify native execution on new
workers. Node registration and successful dialer startup do not establish CNI
or Service readiness. An executable-format failure is a failed bootstrap;
preserve it and withdraw test workers through their normal lifecycle. Correct
the persistent image owner before a fresh campaign. Registry descriptors alone
do not qualify native execution, and a corrected image reference does not waive
workload or management continuity checks during its rollout.

Verify that image corrections survive owner restart, leadership changes and
manifest regeneration. With k0s static configuration, editing the configuration
file does not refresh the running controller's configuration snapshot. A
controller can therefore regenerate an older image reference even after the
file and current DaemonSet have been corrected; Windows-node presence changes
also trigger kube-proxy regeneration. Refresh each controller through the
qualified maintenance procedure, then exercise regeneration and check effective
image references and native execution again. Before arming rollback, compare
its captured configuration with the current desired image pins. File readback
and a successful pod rollout alone do not establish restart durability.

Exercise image resolution with both warm and empty caches, and after switching
between a platform reference and its index. A low-level container-store listing
does not prove kubelet can resolve a local alias with pull policy `Never`.
Rollback must demonstrate that the predecessor starts through the actual runtime;
retain a retrievable immutable reference and usable pull credentials. Verify
running executable bytes when resolving an index-versus-platform identity
mismatch, and retain failed rollout and rollback attempts separately.

1. Freeze binaries, chart and multi-architecture image digests, CAPI/CAPA,
   k0s, both Calico variants and AWS VPC CNI versions. Prove ARM64 runtime,
   not merely an image manifest. Rebuild after any code change.
2. Complete faithful VM topologies. The first site profile needs wired/Wi-Fi route preference,
   VIPs, Tailscale policy tables and IPv6 RA behavior. The second site profile needs its VyOS
   peers, routing policy and Windows site compatibility. The existing identity
   VM profile is a starting point, not proof of these properties.
3. Integrate and rehearse the [node-local rollback guard](network-guard.md) with
   snapshots limited to owned changes. It must work without SSH, tailnet,
   cluster API connectivity or serial intervention. Preserve existing defaults,
   policy rules, firewall rules and external BGP peers. Establish a baseline
   before changes, then run three MTU/BGP migration and rollback repetitions
   per site. Renew pods deliberately to observe actual MTU changes.
4. Run two real EC2 workers per site/family campaign in isolated test
   infrastructure. Verify no Calico installer/Felix on AWS, native allocations,
   original packet sources, return routes, DNS, Services and ingress. Exercise
   each endpoint placement, address churn, pod and worker replacement, final
   claim deletion, API loss, controller and cloud network-manager restarts,
   route conflicts and MTU blackholes. Retain every attempt, including failed convergence attempts.
5. Require zero management and existing-site regressions, cloud failover within
   60 seconds, worker reboot recovery within 120 seconds, and autonomous rollback
   within five minutes. A timed soak is not required.
   Resolve failures and repeat affected qualification; never reuse a pass from
   different binaries or a different CNI topology.
6. Only after the gate passes, stage the first site before the second. Establish
   maintenance/GitOps ownership, arm the verified local rollback, admit one
   canary, verify it, expand site-agent admission, then add cloud capacity.
   Preserve existing CAPI/CAPA installations and router peers.

## Continuous management observations

### Failure scenario descriptions

Every failure test description must state the injected fault, affected resources
and identities, observation lanes, recovery deadline, allowed interruption, and
limitations of the evidence. Record these before injection. Retain every failed
request and incomplete attempt; an expected interruption remains a measured
interruption. Report maintenance acceptance separately from release eligibility.
Do not remove failed samples or relax a gate to represent an exception.

Each individual test description must carry its own scope and limitations; a
reference to this table alone is insufficient. Use these fields in the private
test artifact before starting the test:

- **Fault and scope:** injection method, affected identities, candidate and
  dependency versions, topology, and required qualification row, if any.
- **Availability contract:** permitted interruption by observation lane, lanes
  that must remain uninterrupted, and the explicitly approved maintenance
  scope. Permission for one endpoint does not extend to other nodes or lanes.
- **Recovery measurement:** injection time origin, usable recovery condition,
  deadline, and observation period after recovery. One successful request does
  not establish sustained recovery; keep subsequent failures in the result.
- **Safety and cleanup:** abort condition, autonomous rollback coverage and
  deadline, preserved foreign state, and expected resource cleanup.
- **Limitations:** untested combinations, sampling gaps and resolution,
  intentional target replacement, prerequisites, and conclusions this test
  cannot establish.

After execution, attach raw evidence and separate the observed outcome from
release eligibility. Record skipped or blocked injections as incomplete, not
passed; do not silently replace the planned fault with an easier operation.
Update the individual description with limitations discovered during execution,
including missing lanes, early termination, clock uncertainty and unavailable
logs. State which acceptance criteria the retained evidence cannot establish.
If none are known beyond the declared scope, say so explicitly; do not leave
the limitations field blank. A limitation explains the result and never waives
an acceptance criterion.

| Scenario | Required behavior and permitted interruption | Evidence limitations |
| --- | --- | --- |
| Baseline or observer failure before injection | Require completed samples on every declared lane before injecting the fault. If baseline traffic fails or observation is incomplete, abort injection and collect the original samplers and identities. | The fault remains untested. Missing samples do not establish availability, and observed loss does not by itself identify its cause. Removing an unreachable node changes coverage and cannot qualify the original test. A later successful baseline does not erase the earlier failure. |
| Local MTU controller maintenance | Restart one controller at a time; its direct API endpoint may be unavailable for at most 60 seconds. Other API endpoints and existing workloads must remain available. Verify recovery before advancing. | This local maintenance allowance does not qualify production migration or satisfy zero-management-failure rows. Static configuration validation does not prove coordinated rollback. Verify rendered CNI configuration and fresh pod MTU; existing pods can retain their old MTU. |
| Calico pod replacement | Replace one Calico pod through its existing DaemonSet with configuration unchanged. Preserve original workload identities and require zero workload and management failures through replacement readiness. Capture packets and netlink events on both ends of any implicated path. | A passing restart does not establish continuity when changing MTU, restarting controllers, or replacing several CNI pods. Verify capture integrity and drop counters; timing correlation alone does not identify packet-loss cause. |
| Disposable owner-managed CNI MTU rollback | Change the owning k0s configuration, verify installed CNI and fresh pod MTUs, then let the original autonomous guard restore the source bytes and MTUs. Observe host and pod IPv4/IPv6 traffic throughout all repetitions. | A single-controller restart can interrupt API, exec and logs even when packet probes pass. Assess management continuity separately. Small ICMP probes do not qualify large first-packet Service traffic, worker reboot, multi-controller migration, or deployment to another site. Report the deliberate rollback timer separately from convergence time. |
| Control-plane VM capacity maintenance | Change one VM at a time through the existing VM lifecycle. Preserve and verify its writable disk, machine identity, Node UID and journal ownership before accepting replacement. Require target recovery within 60 seconds and zero failures on unaffected workload and management paths. | A Ready Node with a new boot ID does not establish recovered controller membership, API service or target workloads. Verify the controller lease has renewed after boot, direct API readiness and usable target workloads; observe subsequent membership reconciliation on surviving controllers. A successful memory change does not waive failed continuity checks. If preparation fails after stopping the VM, retain observations until the original VM recovers; recovery outside that window is incomplete evidence. |
| Konnectivity discovery preparation and lease-mode migration | First deploy continuous-discovery agents through the elected runtime owner on every controller; verify effective arguments before enabling lease-mode servers. Exercise leadership changes, management streams through every surviving API endpoint, and resource rollback. Keep workloads and unaffected APIs available. | Preparation can replace agent pods and count-driven servers on other nodes. A local endpoint or worker maintenance allowance does not authorize cross-node exec/log interruption. Record any separately approved bound before injection. Verify embedded runtime hashes: a supervisor build can also replace the API server, kubelet or containerd. A restored executable does not establish restored DaemonSets, leases, RBAC, static-pod identity or stream availability. |
| Kubelet or container-runtime maintenance | Exec/log access on the maintained worker may be interrupted for at most 60 seconds; API health, other nodes and existing workload traffic remain uninterrupted. | A running workload does not establish exec/log continuity. This exception does not apply to agent upgrades or ordinary reconciliation. |
| Agent upgrade | Observe a distinct immutable predecessor becoming the candidate on every intended node with zero management and workload failures. | Same-version restarts, desired image references and rollout status alone do not prove an upgrade or its continuity. |
| Agent upgrade with retired-node pods | Leave terminating DaemonSet-owned pods on nodes that no longer match the desired placement, then upgrade all desired agents under traffic observation. Require bounded rollout progress while retaining the configured availability budget for still-desired nodes. Check selector exclusion, taint exclusion and a deleted Node. | Do not force-delete retained pods or increase the disruption budget to make the test pass. Controller-accounting unit tests do not qualify the installed runtime. A partial rollout with uninterrupted traffic still fails the upgrade gate; record the running versions and retain a compatible recovery path. |
| Agent upgrade packet-loss investigation | Preserve the failed run and its original stream identities. Capture traffic, route events and network counters across the implicated path during a repeat, including rollback. Verify capture hashes, capture drop counters and restored controller configuration and retained authority independently. | Receiver-log absence alone does not prove packet non-arrival. A passing repeat does not explain or erase an earlier failure. Record snapshot length, capture lifetime and any unobserved recovery interval; small-packet captures do not qualify the full traffic matrix. |
| Host provisioning service upgrade | Verify the running predecessor executable, activate the candidate sequentially, and verify the new process and executable identity while retaining zero management and workload failures. Declare abort and rollback deadlines before activation and verify recovery before advancing. | An installed candidate file or successful agent rollout does not prove that the host service runs the candidate. Restoring a binary through console access does not qualify autonomous network rollback. Process identity alone does not establish forwarding, durable ownership recovery or compatibility with existing journals. |
| Compatible binary rollback with retained ownership | Select the declared compatible predecessor through the controller-owned image and bootstrap configuration. Verify controller and agent executable hashes, then activate the installed host services serially and verify distinct process IDs and running hashes. Require zero workload and management failures, unchanged retained API ownership records, and exact durable inventory convergence. Restore the current candidate through the same sequence and verify it independently. | Installed files alone do not prove a host-service downgrade. Current-boot service activation leaves native ownership with the live agent; it does not qualify offline reconstruction, stale-worker replay, address reuse, older incompatible schemas or autonomous network rollback. Record credential renewal separately. Preserve failed downgrade and restoration attempts; a successful return to the current version does not erase a rollback failure. |
| Worker reboot and idle boot recovery | Measure from reboot injection through usable networking and workload recovery; require recovery within 120 seconds and preserve unaffected site/API availability. Observe exec/log loss on the rebooted worker explicitly. | A network-namespace restoration test does not establish VM boot time, workload recovery or the reboot gate. Fresh traffic can hide idle recovery delays. Missing or stale endpoint hints and changed NAT mappings require separate cases; a hint alone cannot authorize routes without authenticated recovery. Same-boot host-service admission must leave native state to the live agent even when retirement is recorded; this no-op does not qualify survivor reconciliation or changed-boot recovery during retirement. Offline cloud recovery may use a withdrawal-bearing inventory only when its digest matches successful native readback for the surviving local journal. The recorded withdrawal supplies exclusions only; it cannot restore the retiring worker or release ownership. Completed ordinary-neighbor checkpoints must retain matching identity and private-file ownership, contain no pending retirement or candidate work, and remain unchanged during recovery; their peers are not replayed. Separate service shutdown, the unobserved shutdown-to-kernel interval, host network readiness, CNI discovery and workload recovery. Record cross-boot clock uncertainty and missing serial output; do not assign an unobserved interval to a component. A successful steady-state request with the CNI credentials does not prove API reachability during boot. |
| Endpoint failover | Inject loss of the selected endpoint; recover cloud traffic within 60 seconds with zero management or existing-site failures. | One endpoint placement or address family does not qualify another. Report changed addresses and NAT mappings separately. |
| API loss | Block the declared control-path access; retain established forwarding and recover affected cloud traffic within 60 seconds. Keep independent API health and site observations running. | Distinguish deliberately blocked client requests from API-server failures. Losing controller access is not evidence for losing every API endpoint or the management observer. |
| Controller leader failover | Run competing controller replicas against the same mesh lease; stop the elected leader and observe a different replica acquire leadership within 60 seconds. Require uninterrupted established workload and management traffic, resumed reconciliation, and preserved retirement evidence. | An isolated API election test establishes lease transfer and writer serialization only. A replacement Pod becoming Ready does not prove leadership or reconciliation. Verify the successor executable against the pinned candidate and measure lease renewal after transfer. Record image-pull credential renewal or other startup intervention separately; a healthy original replica does not establish that a replacement can pull its image. Retained finalization checks do not prove provisioning or receipt delivery after transfer. Test loss of lease renewal separately from graceful shutdown. For process suspension, arm recovery before injection, bind signals to the exact process identity, verify the successor renews while the old process remains paused, and verify the old process exits after resumption without taking the lease from the successor. Capture the exact former container status and logs before scale-down or Pod deletion removes them; PID disappearance alone does not establish the exit cause. Suspension does not model a partitioned process that continues executing, or cancellation of an in-flight cloud API mutation. Neither scenario establishes control-plane quorum recovery or version rollback. |
| Controller restart | Restart the declared controller and recover affected cloud traffic within 60 seconds without management or existing-site failures. | Reconciliation recovery does not establish host reboot recovery, leader failover or continuity during a binary upgrade. |
| Cloud network-manager restart | Restart the actual owner of cloud networking; recover cloud traffic within 60 seconds, preserving foreign routes, addresses and policy and zero management/site failures. | Restarting only the provisioning agent does not exercise the network manager. Qualification is specific to the tested manager and configuration. |
| Route collision | Introduce a declared foreign route or policy conflict; reject unsafe mutation and preserve existing connectivity and ownership. | One collision shape does not cover other tables, priorities, marks or either untested IP family. Rejection alone does not demonstrate continued forwarding. |
| MTU blackhole | Inject the declared path-MTU fault; recover cloud traffic within 60 seconds with zero management/site failures. Measure large traffic in both directions. | Small probes and warmed PMTU caches can conceal a first-packet failure. State payload sizes, DF behavior, cache state and each path's MTU; retries cannot erase the initial loss. |
| Autonomous migration rollback | Deny the declared management paths and let the local guard restore owned changes within five minutes, without console intervention. Maintain management/site observations. | Restored files or a successful guard receipt do not prove restored networking. Verify effective routes, CNI and workloads. Independent per-node guards do not establish safe multi-controller sequencing or quorum preservation. |
| Allocation churn, pod recreation and worker replacement | Observe allocation withdrawal and replacement through usable traffic, with no failures in required campaign lanes. Record intentional target lifecycle changes separately. | A replacement's readiness does not prove stale routes disappeared. Pin identities and use stable Services where testing continuity across pod-IP changes. |
| Unsupported source withdrawal followed by peer deletion | Reject source-range shrink for a retained ordinary peer before replacing durable ownership intent. Repeat the rejected reconciliation, then delete through the normal lifecycle and verify that retirement still receives the original permissions. | Admission tests with simulated native callbacks establish checkpoint preservation only. They do not qualify live source narrowing, forwarding continuity, route withdrawal or device retirement. Growth-only ordinary reconciliation still requires a separate transition for removing permissions. |
| Interrupted ordinary-peer source growth followed by deletion | Interrupt native installation before or after source permissions change, retry reconciliation, then withdraw the peer through normal publication. Preserve every exact admitted permission set until native readback succeeds; retirement must match one of those sets and preserve unrelated owners. | Simulated admission callbacks prove durable intent only. Isolated kernel retirement establishes exact permission matching and protected-peer traffic for that fixture, not reboot, same-key reconnection or an AWS lifecycle. The growth-variant kernel cases cover Linux IPv4 and IPv6 permissions, but the continuous protected stream is IPv4. They do not establish continuous IPv6 traffic or other operating systems. Successful native readback must discard obsolete candidates even if separate receiver preparation then fails. Earlier checkpoints that already lost an installed variant cannot reconstruct it from current intent. Pending candidate history requires a reader that understands it; an older binary must fail closed rather than discard it. This does not implement source narrowing or completed-journal release. |
| Receive-journal write fence | After establishing withdrawal authority, freeze the exact completed journal incarnation before participant retirement. Race freezing against successor creation in both orders; retain the winner and reject stale writes. Preserve the fence across checkpoint recovery, reject unfreezing and same-name recreation, and reject additive successor preparation from a frozen predecessor. | An immutable ConfigMap fences data writes, including older writers, but does not itself authorize withdrawal, stop native serving-state maintenance, retire pending devices or release an inventory reservation. Older agents can still prepare additive devices unless upgraded to enforce the predecessor fence; verify every participating agent before activation. Storage and predecessor-admission tests do not prove native retirement, authenticated retirement acknowledgements, traffic continuity or safe address reuse. Freezing is one-way for that API incarnation; deleting and recreating it is not rollback. |
| Receiver withdrawal admission | Bind the persisted withdrawal intent to the exact frozen, completed journal and its explicitly published inventory UID. Require published removal of the worker and retain every journal participant by Node name, UID and key. Reject new published consumers until the persisted withdrawal inventory is extended. Unavailable survivors remain required; their absence is never a retirement acknowledgement. | Admission tests cover synthetic forward, abort and rollback histories, identity changes, incomplete withdrawal and membership changes. They do not establish live cross-object consistency, archive authentication, native deletion, cleanup of separately prepared devices or authenticated retirement receipts. The executor must revalidate live authority under the native lock and preserve reservations until retirement is durably acknowledged. No interruption allowance follows from admission; the live deletion campaign still requires uninterrupted surviving workload and management paths. |
| Receiver withdrawal intent persistence | Persist the captured withdrawal intent in the existing receiver inventory before publishing peer removal. Bind it to the frozen journal UID and terminal history. Inject competing reservations, a lost write response and inventory recreation; retain other workers and reject replacement intent or checkpoint regression. | Isolated API tests establish storage compare-and-swap and replay behavior with synthetic terminal receipts. They do not establish atomicity across mesh, journal and inventory objects, live native retirement or acknowledgement delivery. Readers must revalidate current authority before mutation. Older strict inventory readers reject the new retirement field; upgrade every participant before activation. Persistence does not stop serving-state maintenance or release any reservation, and no native executor is activated by these storage tests. |
| Survivor admission after worker deletion | Load every reserved journal and its authenticated ancestry, the persisted withdrawal and current mesh. Match the surviving local Node UID, key, boot and readiness, then re-read journal, inventory, mesh and Node identities. Reject missing reserved journals and same-name survivor replacement. Allow kubelet heartbeat timestamps and informational transition timestamps on unchanged non-Ready conditions to advance without restarting admission. Continue rejecting condition-state changes, Ready transitions, boot changes, address or allocation changes and other Node source mutations. | The isolated API test injects a status heartbeat during admission and deletes the retiring worker Node and adoption Secret, then admits the retained survivor without discarding reservations. Terminal receipts are synthetic and no native cleanup callback runs. This establishes read admission only, not device retirement, pending-device discovery, checkpoint durability, authenticated cleanup receipts or continuity. Cross-object reads do not lock API writers; native execution must re-admit under the host lock and suppress receipts when authority changes. Within one fully admitted cleanup attempt, per-device guards may reuse authenticated history while re-reading every live reserved journal, inventory, mesh and local Node identity. Full history admission remains required before receipt emission. Real-API tests bound the guard to journal/inventory reads and reject changed source bindings, local identity, failed reads and cancellation; they do not establish live cleanup latency or receipt collection. |
| Retirement checkpoint and stale serving recovery | Under the shared host lock, re-admit withdrawal, validate retained inventory and local journal histories, and persist the inventory before journal checkpoints. Reject stale admission, corrupt history and replacement or removal of retirement intent. A recorded withdrawal must prevent ordinary serving reconciliation and offline recovery from restoring its old receivers. | Tests establish private durable intent, idempotent checkpointing and refusal before native callbacks; they do not establish completed kernel retirement or receipts. Published retirement permits live survivor phase admission and completed maintenance only after full withdrawal revalidation, retained-consumer authentication and checkpoint preservation. Unpublished retirement still blocks combined admission. Changed-boot offline cloud recovery requires the exact previously verified inventory and local journal, retained-consumer identity, and completed neighbor bookkeeping. Do not activate retirement publication until native survivor recovery and cleanup are qualified together. A checkpoint is not a native completion observation and cannot release reservations. |
| Direct receiver-route withdrawal after worker termination | Under admitted withdrawal and the host lock, remove only the explicit direct receiver destinations after validating both recorded generations, device aliases, keys, exact source permissions, route protocol, metric, source and policy ownership. A terminated worker need not supply a current handshake for withdrawal; forwarding remains handshake-strict. Verify route absence before using the existing dedicated-device retirement guard. | Isolated Linux kernel tests cover IPv4 and IPv6 route readback, an unchanged protected route, idempotent withdrawal and device retirement, and rejection of foreign routes, address dependencies and ownership changes. The fixture has no handshake or workload traffic sampler; unchanged routes do not prove uninterrupted traffic. Source exceptions require their own retirement path. Relay routes are removable only when their exact next hops are separately admitted from journal history. The test has a 45-second deadline inside disposable guest mount/network namespaces. No live journal integration, authenticated retirement receipt, AWS lifecycle, reboot, ARM64 or Windows qualification follows from this result. |
| Authenticated retirement completion commit | Re-admit controller source state, authenticate every retained consumer, re-read mesh/journal/inventory identity, recheck consumer incarnations and freshness, then CAS only public completion observations into the existing inventory. Never persist bearer tokens or automatically release reservations. | A real-API test combines persisted withdrawal binding, pod-bound TokenRequest/TokenReview and completion storage. Missing policy, failed full-state admission, concurrent inventory/mesh changes and a reboot during the post-authentication check must leave completion absent. Valid observations retain authenticated pod/boot identity and reject stale-binding retries after the write. Transport history and native absence are synthetic; the controller callback, network transport, live cleanup and release are not qualified by this fixture. |
| Durable retirement completion storage | Store controller-verified completion evidence in the existing inventory only at the exact revision used for authentication. Require the unchanged persisted withdrawal and frozen terminal journal. Preserve every reservation and other worker entry; reject conflicting evidence and stale revisions without automatic retry. | Real API storage tests use synthetic evidence to exercise conflict, lost-response, reload/idempotency and replacement rejection. They establish persistence behavior only, not authenticated completion or native absence. Completion storage does not release journal, listener or allocation reservations. Older strict readers reject the new optional field, so all inventory readers must be upgraded before activation. Transport and release remain separate requirements; the authenticated commit scenario covers authentication-to-storage integration. |
| Concurrent receipt delivery | Submit all retained consumers concurrently. Retry only a pre-admission busy response with the explicit one-second retry signal, using jitter within the original 20-second delivery deadline. Preserve the exact observation and recheck identity, source and freshness on every delivery. | TLS collector tests model seven simultaneous consumers and serialized admission with synthetic authentication callbacks. They do not prove real API throughput, native retirement or live completion. Cancellation, conflicts, unmarked unavailability and ambiguous network failures must not cause redelivery; waiting must honor the caller deadline. Busy retries cannot extend observation freshness or release reservations. A successfully re-admitted retirement collection refreshes its batch deadline and age policy from current authority while preserving individual observation timestamps. A fixed handover deadline must still expire; refreshing a collection cannot preserve stale participant credentials. Failure diagnostics must identify the fixed collector stage without exposing callback errors, credentials or arbitrary peer headers. Synthetic source, observation, completion and deadline failures verify this boundary; stage reporting does not establish successful authentication or live convergence. |
| Retirement observation identity and freshness | Bind observations to the frozen persisted withdrawal, exact inventory incarnation/revision and mesh incarnation/revision. Require one observation per retained consumer, including consumers without a tunnel key, reject handover-phase payload fields, and authenticate live pod/node/boot/container/image identity using the existing receipt authenticator. Recheck each authenticated node boot and agent container after the full batch authenticates, then recheck freshness. | Pure batch tests reject missing, duplicate, unknown, stale, future and differently bound observations; real API persistence tests check binding stability and revision changes. A real-API batch test uses pod-bound TokenRequest/TokenReview credentials to verify successful authentication, wrong-node/audience/unbound-token rejection, replaced boots/containers/images and boot/container changes during the second consumer’s authentication. Its receipt scope is constructed by the test; these checks do not prove native absence or an end-to-end persisted retirement submission. The caller must re-admit all source state before committing completion. Transport integration, durable completion and reservation release remain separate requirements; credentials must never be persisted. |
| Journal-owned relay destination withdrawal | Derive relay next hops from the withdrawn worker’s committed current and predecessor paths. Reuse route-generation preflight to check exact destination, underlay, gateway, source, protocol and metric before removing the scoped route. Preserve other workers sharing the underlay and verify target absence. | Isolated IPv4/IPv6 kernel tests cover repeated deletion, multiple recorded relay generations, unchanged surviving routes and rejection of unrecorded gateways, protocols, metrics and cancelled operations. They also rerun direct receiver withdrawal. Marked gateway resolution must still identify an active directly connected underlay; this does not qualify underlay loss, non-endpoint followers, packet continuity or the complete API-to-native lifecycle. The process deadline is 55 seconds. |
| Source-exception withdrawal after worker termination | Admit exact host destinations, source allocation and recorded receiver identities before removing source routes. Retain selectors needed by surviving routes, remove selectors with no surviving route, and verify final absence. Withdrawal requires no handshake from the departed worker; ordinary source-route inspection remains handshake-strict. | Isolated IPv4/IPv6 kernel tests cover three historical receivers, idempotency, preservation of legacy-device and dedicated-receiver survivors outside deletion scope, last-route selector cleanup and rejection without route changes for foreign protocol, alias/key changes, wrong selectors, uncommitted hosts and cancellation. The process deadline is 55 seconds. No workload stream, live API admission or complete worker lifecycle is measured. The direct-site withdrawal path derives exact source hosts and allocations from the complete journal set and imports recorded pending receiver candidates before source cleanup. Other journals supply candidates for preservation only. Pure direct-site projection tests check owner/node scope and allocation consistency; the full API-to-kernel sequence is still unqualified. An unadmitted receiver in the source table remains a blocking ownership conflict. |
| Direct-site forwarding-cache withdrawal | Validate the private cache and exact mesh/worker key, import its old permissions into the existing ordinary retirement sidecar, then retire dedicated receivers and ordinary peers using their native guards. After renewed authority, remove only the worker transport fields and its cached acknowledgement. | Filesystem tests verify preparation is read-only, preservation of surviving fields/observations, idempotent reload, and rejection of stale contents, wrong identity, modes, symlinks and duplicate cached identities. Transit caches are rejected; relay and source-exception retirement remain separate. The assembled direct-site path still needs live API-to-kernel and AWS lifecycle qualification. Cache cleanup does not issue a completion receipt or release a reservation. |
| Withdrawn ordinary-peer cache commit | Persist all selected historical permissions into the existing retirement sidecar before native cleanup. Revalidate live authority before and after cleanup; remove the withdrawn root from executable peers, pending candidates and retirement entries only after native success. Preserve every other owner and the original checkpoint identity. | Filesystem tests use simulated native callbacks. Native failure, authority loss and changed cache contents retain withdrawal evidence. Invalid identity, private-file mode or symlink inputs fail before mutation. A successful callback is not a kernel proof or authenticated completion receipt. Multi-file writes are not atomic; retries reload durable state, and a partial commit must retain the sidecar until executable references are removed. Legacy forwarding caches and controller lifecycle integration remain separate requirements. |
| Withdrawn ordinary-peer selection | Select only the explicitly withdrawn reserved root from recorded permission variants, retaining all other journal reservations and transport identities. Reject a still-published root, missing publication, invalid sources, transit paths, and overlap with surviving owners. Ordinary reconciliation must continue excluding reserved roots. | Pure planner tests cover historical variants and conflicting owners; they do not validate persisted withdrawal authority, kernel state, authenticated completion receipts, or traffic continuity. The caller must load the complete live inventory and persisted withdrawal under the reconciliation lock before using the result. Historical source variants are not proof that those permissions coexist in the kernel. |
| Recorded serving and pending site-receiver cleanup | Re-admit and checkpoint the persisted withdrawal under the host lock. Preserve committed serving/predecessor graphs alongside additive ownership records, then batch route withdrawal across every recorded generation before retiring devices. Retain explicit destination hosts as well as permission prefixes. Recheck authority before each mutation and remove ownership records only after verified device absence. | Ownership-selection tests use simulated native callbacks; kernel tests separately exercise three receiver generations and protected-route readback. Their combination does not establish live API-to-native integration or workload continuity. Failure retains ownership evidence for retry, including after partial native work. Both legacy and admitted receiver ownership directories and their executable receiver caches are covered. Filesystem tests reject corrupt, foreign-mesh, non-private, symlinked or changed caches before importing ownership or committing cache removal; mixed records retain surviving bindings and their previous documents. These tests do not simulate power loss or establish multi-file atomicity. A partial durable commit requires reloading current cache state before retry. Ordinary root peers and their caches, source exceptions, relays and full retirement receipts remain separate requirements. A successful receiver cleanup must not release inventory or be reported as complete worker retirement. |
| Follower forwarding with retained receiver history | Derive each worker's relay from its selected transport generation and accepted follower sources, including abort, rollback and a predecessor receiver. Keep worker destinations separate. Require live inventory, released-owner exclusions, Node and agent identity, relay forwarding evidence and durable route ownership before native changes. A follower with no local tunnel must be able to install explicitly scoped underlay relay routes. | Planner fixtures use synthetic transport histories; they do not authenticate live ownership or prove relay readiness. Follower switch admission must reuse the authenticated relay barrier, reject stale or expired observations, verify the relay incarnation, and reject absent or replaced worker publication; a handshake is insufficient. Synthetic switch-proof cases do not establish live relay readiness. Real-API observer tests recheck exact inventory and mesh revisions, journal continuity, Node source allocation and agent incarnation before a simulated write and on return; their bounded callbacks do not install routes or prove relay forwarding. Isolated Linux namespace tests inspect IPv4/IPv6 kernel routes, distinct per-worker gateways, rejection before mutation and preservation of unrelated routes. They do not measure traffic continuity or establish the combined live API-to-kernel path, cross-boot recovery, provider cleanup or deployment qualification. A current relay election cannot substitute for the source owner in a committed worker generation. |
| Follower route intent and retirement | Persist every gateway variant that an interrupted installation could have written, keyed by worker identity and bound to inventory, mesh, Node, local key, route table, mark and source addresses. Reject overlapping worker destinations, stale writes and attempts to replace an existing record as an initial write. Compact only after native verification; omitted workers require explicit retirement. Retirement must include recorded candidates and preserve survivor routes and records. | Filesystem tests establish private-file validation and intent preservation, not native installation or live authority. A previous-boot record is historical ownership evidence only and cannot authorize replay. The isolated guest test combines simulated API/terminal history with real IPv4/IPv6 routes and a local BGP RIB, exercises an interrupted gateway change, and rejects a stale pre-retirement record write after cleanup. It does not establish actual process-crash recovery, stale VM replay, address reuse, external BGP convergence, packet continuity or AWS cleanup. Live installation and boot recovery must still consume fresh admission and verify native state. |
| Follower serving route reconciliation | Under the observer host lock, bind current Node sources to the admitted mesh, select each active worker’s committed relay, renew live identity and switch evidence, and stage all possible route variants before installation. Preflight and read back only owned destinations; preserve omitted workers for explicit retirement. Update scoped BGP advertisements only after route verification. | The isolated guest serving test uses real IPv4/IPv6 kernel routes, durable files and a local BGP RIB. It covers failure before mutation, failed revalidation after installation, retry and compaction, a simulated new boot, preservation of omitted workers, foreign-gateway rejection, and verified removal of superseded destinations and their BGP announcements. A synthetic completed journal exercises committed-plan selection through kernel installation and rejects changed Node sources. Live observer API checks are tested separately. These cases do not qualify actual process-crash or reboot recovery, combined real-API-to-kernel operation, packet continuity, external BGP convergence or AWS deployment. Ordinary unreserved-worker forwarding requires separate coverage; initial observer admission and storage have their own cases below. |
| Initial follower admission | Select the phase-aware follower writer when the first active receiver journal exists, without requiring a prior retirement. Establish an initial inventory fence only from complete live journal, mesh, Node and agent admission. Reject missing inventory beside any other local recovery record; existing retirement and released-owner fences remain mandatory. | Real-API tests cover initial admission, repeat reconciliation, orphaned follower/journal/neighbor records, corrupt inventory, changed agent identity, cancellation and a mesh race before a simulated write. These tests do not install routes or validate packet continuity. Native serving must either validate existing forwarding ownership or initialize empty legacy destination metadata through the separately tested fresh-storage path. Ordinary unreserved-worker forwarding still needs separate qualification. |
| Initial follower forwarding storage | Initialize only missing forwarding metadata, recording zero legacy worker destinations; actual routes require the admitted per-worker writer. Reuse existing BGP peer selection and bind the metadata pair before native installation. Recover an interrupted metadata pair only before any per-worker record exists. Retirement must verify committed destination absence even after compaction and without a legacy relay. | Isolated guest tests use real IPv4/IPv6 routes and local BGP with synthetic committed history and simulated live admission. Fresh installation starts with the target routes absent. Cases cover a missing legacy endpoint, interruption between metadata writes, retry, orphaned route or BGP records, receiver residue, changed live authority and symlink rejection. Empty-legacy retirement preserves survivor routes and rejects a reappearing withdrawn route after compaction. These tests do not establish real-API-to-kernel operation, actual process or VM crash recovery, external BGP convergence, packet continuity, provider cleanup or deployment qualification. |
| Ordinary follower lifecycle beside reserved workers | Select ordinary relay routes only from the worker’s acknowledged adoption document. Withhold whole-site acknowledgement until all current destinations match verified native routes and the global site projection. Remove unpublished unreserved workers through scoped native cleanup; retain their withdrawn keys while permitting address reuse by a different key. | Isolated IPv4/IPv6 guest tests combine synthetic API state, real routes and local BGP. They cover pending adoption, installation, withdrawal acknowledgement, stale-key rejection and address reuse. Filesystem tests reject stale replacement, forgotten withdrawal markers and invalid versions. Real-API observer tests cover the final acknowledgement boundary and reject a mesh race after simulated native work; their acknowledgement callback performs a metadata CAS, not kernel verification. These do not establish combined real-API-to-kernel operation, external BGP convergence, packet continuity, provider deletion or actual reboot. Whole-site acknowledgement is withheld during mixed relay generations or outstanding reserved retirement. The first ordinary withdrawal writes follower-record version 2; older readers reject this format. Downgrade to those binaries is unqualified and must not erase the marker to restore operation. |
| Claim ownership changes before drain | Create the Machine through the claim controller, then replace its claim owner reference with a Cluster owner reference while retaining the creation-time claim namespace/name and UID annotations. Verify that the same claim can still drain and retire it. Reject partial bindings, a different claim UID, conflicting claim owners, and same-name objects in another namespace. | The durable binding is association evidence, not cleanup authorization. API tests simulate owner-reference changes; they do not establish live CAPI behavior or protect against a principal authorized to rewrite all ownership metadata. Existing Machines without this binding require an exact claim owner reference; missing historical ownership is not reconstructed from names or claim status. Native retirement, compute deletion and continuity remain separate requirements. |
| Mesh metadata changes during captured retirement | Capture withdrawal, change only Secret metadata before reservation and again before publication, then resume through authenticated completion. Bind new captures to the complete Secret data hash and preserve the original consumer set. Continue using the live resource version for publication CAS. Reject changed data, invalid hashes and metadata revision changes on legacy captures without a hash. | This permits metadata changes only: even acknowledgement changes inside Secret data still require separate handling. It does not rebase network changes or extend captured membership. Older readers may reject the additional intent field; qualify mixed-version retirement and rollback rather than dropping persisted fields. API tests do not establish live native continuity. |
| Standalone claim deletion with receiver reservations | Delete a claim through its finalizer. Persist Machine, Node, mesh, inventory and gateway identities before cordoning; retain gateway requests and every captured survivor until retirement completes. Require the shared authenticated receiver-retirement sequence before committing compute removal. A valid unfinished transport transition must retain ownership and use the normal five-second pending retry instead of exponential error backoff, including after its round expires. Malformed or missing history must remain an error; time alone cannot authorize freeze or deletion. Reject changed identities, missing journals, malformed removal records and conflicting drain owners. Verify group-owned claims also require the matching parent's committed action. | Real-API standalone and group tests exercise persistence, blocked deletion, authenticated completion, stale-boot rejection, refreshed observations and the child-to-CAPI deletion request. Transport history, agent readiness and native cleanup observations are synthetic; no provider controller runs in these tests. They do not establish a live deletion campaign or native reboot recovery. PDB refusal and unavailable required identities remain blocking; no forced drain or elapsed-time bypass is authorized. Capture does not automatically rebase changed consumer membership or mesh revisions. Orphan recovery and reservation release remain separate requirements. |
| Orphan peer with receiver reservations | Remove compute independently of normal retirement, then reconcile orphan discovery. Preserve peer fields, adoption state and address reservations for every published journal and unfinished preparation. Reject unavailable or malformed reservation discovery before ordinary cleanup, and distinguish same-named Machines in different namespaces. | This exclusion prevents ordinary orphan pruning from substituting Machine absence for retirement. Orphan-intent capture tests require a frozen terminal journal, its exact reservation, an already absent peer publication, and successful authoritative NotFound reads for the old Machine, Node and adoption Secret. They retain published survivors and reject replacement objects, read errors, missing participants and changed captured mesh data. Capture tests use synthetic API objects and terminal history. Controller tests use real journal and inventory storage with synthetic owner absence and surviving Nodes: freeze completion, persist already-withdrawn intent on a later pass, retain reservations and reuse the recorded consumer set on retries. Unpublished preparations fence this path. These tests do not establish live owner observation, authenticated retirement completion, reservation release or native cleanup. Recovery currently requires the old peer publication to be absent; a still-published orphan needs a separate removal protocol. Changes to captured mesh data remain blocking rather than automatically rebasing consumers. API discovery and the later Secret write are separate operations, not a cross-object transaction. Require authenticated retirement and explicit release evidence before claiming eventual cleanup or safe address reuse. |
| Last-worker deletion | Delete through the provisioning lifecycle and verify final route withdrawal and resource release without existing-site regressions. Exercise deletion after a completed receive transition and while a successor has prepared devices but has not replaced the serving journal. Retire both serving and pending references before releasing their ownership reservations. | Empty Kubernetes listings or terminated instances do not prove network cleanup; inspect cloud resources, routes, durable checkpoints, WireGuard source permissions and pending devices on every participating node. A terminal journal is not a retirement acknowledgement, and its reserved peer can still block address reuse after its claim disappears. A comparison against only the serving graph can miss devices created during successor preparation. Record cloud resource cleanup, native retirement and inventory release as separate outcomes. Do not bypass finalizers, delete journals or manually remove peer state to make the test finish. |
| Overlapping historical ordinary-peer retirement intents | Record an interrupted replacement with source ranges also present in an older committed peer. Withdraw both through normal publication; retire exact installed owners, preserve absent attempts until route cleanup completes, and retry idempotently. Inject a broader competing live prefix with both recorded and foreign identities; reject before route or permission mutation. Require zero protected workload and management failures and recovery within 60 seconds in the live campaign. | Historical records do not prove simultaneous kernel ownership. Validate each record against reserved journals and check actual native ownership before deleting anything. Isolated Linux tests cover both IP-family permissions and an IPv4 protected stream, not AWS lifecycle, continuous IPv6 payloads, cold handshake timing or reboot. Preserve first-attempt failures even if a subsequent run passes; a retry does not qualify continuity. |
| Legacy cloud peers across reserved-receiver activation | Begin with previously installed ordinary cloud peers, activate reserved receivers, then delete and replace ordinary workers through the normal lifecycle. Verify ownership transfer, retirement and bidirectional traffic when replacement workers reuse source ranges. Preserve journal-owned peers and unrelated native state. | A new ordinary-neighbor checkpoint does not establish ownership of older peers already installed on the device. Refusing a source-authorization collision protects the old peer but does not qualify replacement-worker connectivity. Test workers joining both an existing cloud node and a fresh cloud node; Ready Nodes and site-to-cloud traffic alone do not cover cloud-to-cloud forwarding. A legacy cache alone must not authorize deletion without validating its ownership and native state. |
| Receive-transition interruption, expiry and rollback | Inject the declared interruption at the named transition phase; verify authenticated forwarding, durable recovery and preservation of journal-owned and foreign state. Observe renewal, expiry, rollback and inventory release as separate operations. | A terminal journal or an isolated route test does not prove live forwarding or recovery after a full reboot. Whole-peer retirement does not cover source narrowing on a retained peer, independently prepared receivers or the next publication generation. Name every unexercised phase and operation in the individual description. |
| Retirement from an expired unfinished transition | Require persisted drain authorization, the exact published worker, a valid captured consumer set and unchanged source data. Fence the expired journal and its exact withdrawal-intent digest in one immutable API update. Keep its actual phase and all possible native generations; never synthesize transport completion. Reject stale freezes after renewal, changed drain authority and marker regression. Publish withdrawal through the existing inventory and mesh CAS path; retain reservations until authenticated native absence and finalization permit release. | Real-API cases use synthetic transport histories and simulated lifecycle authorization at prepare, switch, drain, retire and abort. They verify fencing, publication and retained reservations without native receipts. Separate real TokenRequest/TokenReview cases exercise authenticated completion, finalization, retained-release validation and rejected stale or changed evidence for each interrupted phase; their native absence observations are simulated. Unexpired rounds remain pending on the normal bounded retry. Isolated guest cases use real IPv4/IPv6 routes, WireGuard and local BGP with synthetic API state; they do not prove combined real-API-to-kernel operation, packet continuity, unavailable-consumer recovery, provider teardown, actual reboot or production deployment. Older retirement readers reject unfinished history; mixed-version cleanup and downgrade remain unqualified. An expired round or missing compute is never evidence of consumer cleanup. |
| Sibling withdrawals captured from the same mesh | Preserve the exact public peer fields removed by each new hash-bound withdrawal in its existing inventory record. Validate a later sibling’s original source by reconstructing only recorded absent peer fields from the same capture hash, without writing them back to the mesh. Reject unrelated data, partial peer remnants, replacement keys and fields outside the recorded worker. Keep captured consumers and reservations unchanged. | Real-API tests exercise both serial publication orders for two expired workers under simulated drain authorization, including freezing the second worker after the first publication. They do not supply native receipts or prove consumer cleanup, cohort completion, provider deletion or packet continuity. Separate real-token completion, finalization and retained-release cases exercise version 2 records with simulated native absence. Publication must reject recorded field values that differ from the still-published peer. Schema tests retain exact-revision semantics for legacy captures and reject malformed or broader reconstruction. New records use withdrawal version 2; existing version 1 records are retained without recapture or upgrade. Older readers reject version 2, so mixed-version cleanup and downgrade remain unqualified. This removes a source-validation conflict; mutually unavailable consumers still require a separate verified retirement mechanism. |
| Allocation change after a completed receive transition | After each terminal outcome (forward completion, abort and rollback), allocate a new workload range and verify its publication, authenticated permissions, routes and bidirectional traffic through the normal ownership lifecycle. Preserve existing traffic and foreign state while advancing ownership; exercise interruption and restart at the generation boundary. | A completed journal can retain an older source set even when the current publication contains the new allocation. Ready nodes and working exec/log requests do not prove workload connectivity. Controller rejection of source drift only exposes the mismatch; it does not activate a successor or restore workload traffic. Storage tests must verify that rejection preserves the exact serving journal and plan. Ordinary-peer growth tests do not cover journal-owned peers. Deleting a journal, manually widening permissions or excluding the affected worker cannot qualify generation advancement. Test source growth, withdrawal and address reuse separately; success in one direction or terminal outcome does not establish the others. |
| Survivor route projection during worker withdrawal | Validate the complete reserved journal set before excluding a published withdrawal from serving intent. Require its exact frozen terminal journal incarnation; retain its destinations against reassignment. Preserve survivors in forward, abort and rollback histories. Captured but unpublished withdrawals remain serving. | Planner tests use synthetic histories and compare both IPv4 and IPv6 route intent; real-API publication tests distinguish prepared and published withdrawal. Real-API agent tests additionally exercise survivor phase execution and completed maintenance after the retiring Node and adoption Secret disappear, preserve both checkpoints, and reject concurrent withdrawal changes before issuing receipts. Those real-API native callbacks are simulated. A separate isolated Linux guest test combines synthetic API/history, actual cloud-neighbor cleanup, authenticated loopback receiver transport and completed-state maintenance after deleting each survivor route in turn. It requires recovery within ten seconds per injection, checks IPv4 and IPv6 route readback, retains an unrelated root peer, and verifies the withdrawn peer stays absent. The fixture also rebinds actual native observations and endpoint hints to a simulated previous boot, deletes the survivor receiver, and exercises offline recovery with all emitted bookkeeping retained. This does not measure payload continuity, actual VM reboot time, real-API-to-kernel integration or an AWS lifecycle. Withdrawn identities remain explicit in the serving projection: stale site advertisement caches must reject them, and cloud policy restoration must omit their remote destinations. These planner checks do not prove BGP convergence or actual rule restoration. The serving projection does not authorize clearing shared tables or releasing reservations. Live qualification still requires zero interruption on surviving workload and management paths. |
| Pinned target journal with multiple reserved workers | Pin the target journal incarnation, admit all other reserved journals under their own identities, and retain those identities through rechecks and local checkpoint writes. Reject a mismatched target before native execution. Require zero interruption on existing workload and management paths in live qualification. | Real-API tests cover local site membership in both journals and cloud membership in only one, with simulated native verification. They do not establish traffic continuity, retirement coexistence, or reboot recovery. A target UID must not be reused as the expected UID of another reserved worker. |
| Missing predecessor history or lost archive read access | Exercise both a fresh agent without a checkpoint and an agent that missed multiple generations, then deny a required archive read, remove an archive or recreate it with identical bytes and a different UID. Reject incomplete ancestry before replacing the checkpoint. Preserve named read-only grants through chart upgrades; never grant agent mutation or namespace-wide listing. Restore authorized access before retrying admission. | API ancestry and RBAC tests do not establish native forwarding continuity, offline boot recovery or usable successor routes. Include nonlocal journals whose destinations participate in shared routing; successful local admission alone does not validate the entire inventory. Read-time ancestry validation does not make journal and archive reads atomic. Record the denial window and recovery deadline in the live test; no interruption allowance follows from an admission rejection. Recreated history must remain rejected even when its bytes match. A chart test without a running controller proves grant preservation only, not controller publication ordering. |
| Allocation changes while an old site path remains available | Bind previous site permissions separately from desired permissions. Verify preparation preserves the exact old peer, rollback retains its destinations, and other journals and ordinary peers cannot claim either generation's ranges. Exercise growth and withdrawal for both address families. | Planner and peer-admission tests do not prove native switching, rollback timing or final removal of obsolete receive permissions. Advertisement planning must select the committed serving destinations from either valid cached generation; planner success does not establish native route readback or BGP convergence. Transitional source unions must not be reported as completed source withdrawal. Site preparation, readiness, durable replay and retirement must agree on the predecessor and desired documents for each receiver. Cache round trips only prove retained inputs; they do not prove kernel restoration, authenticated traffic or final removal of old permissions. Existing journals without an explicit previous site document retain their original single-document semantics; they do not supply missing historical allocation evidence. |
| Successor archive access or journal publication interrupted | With prepared cloud and site receivers, fail the agent Role update or the live journal write. Require unchanged serving history and zero interruption before commit. Grant only named archive reads before replacing the journal; retain the same journal UID, inventory reservation and archive identity across retries. Resume a committed successor without rewriting it. | Controller tests use real archive/journal/inventory storage but simulated Role/Secret storage and readiness. A concurrent Role update must conflict with an older grant derivation rather than losing the newer archive grant. Missing Role access, a simulated concurrent Role update and an injected journal write failure do not establish live authorization-cache propagation, native preparation or traffic continuity. Before commit, retry additive preparation without rollback; after commit, use the configured round deadline and expiry recovery policy. Synthetic test times do not qualify an operational recovery deadline. Separate API objects cannot be committed atomically; agents must reject unavailable or mismatched ancestry and preserve serving state. |
| Successor receiver preparation interrupted before activation | Commit a distinct receiver generation before preparation. Interrupt and resume preparation while keeping the predecessor devices, keys, permissions and forwarding intact; reject bindings from another generation. Reserve separate listeners before creating the successor devices. Require zero interruption on existing workload and management lanes. | Deterministic interface, alias and key tests do not establish simultaneous native listeners, authenticated traffic or crash-safe device cleanup. Reusing stable mesh or node identity does not authorize reuse of a serving receiver. Agents must restore the committed generation rather than minting a new one after an observation timeout. Controller proposal retries and boot-identity refreshes must preserve the generation and listener reservations for unchanged desired transport; refresh readiness without changing the serving document or journal. Proposal tests with simulated Secret writes and real journal storage do not establish agent admission, native preparation or cross-object atomicity. Completed-maintenance admission tests use real API storage for both cloud and site identities; cloud readiness uses simulated native preparation. They reject inventory or proposal changes before readiness and skip absent, original or already-consumed requests. They do not establish site handshake readiness, native coexistence or traffic continuity. Controller plan-building tests add a worker destination after forward, abort and rollback completion: preserve the serving generation and its old relay destinations while admitting the new destination separately. These use simulated readiness and Secret storage with a real predecessor journal; they do not establish successor publication or activation. Preparation shares the maintenance lock and its 30-second maximum deadline (shortened by any unfinished local round); failure leaves serving forwarding selected and requires fresh admission on retry. Bind successor preparation to the exact completed journal identity and terminal history; reject changed or missing predecessors before device preparation and before readiness publication. Reject generation or listener reuse against both receiver graphs recorded by a terminal successor, including the older graph still serving after abort or rollback. Admission rejection must precede native work and leave forwarding unchanged; simulated journal tests do not establish packet continuity. Validation tests do not prove atomicity across the journal and readiness Secret, inventory admission, or safe offline replay; retain the journal reservation fence until those paths are qualified. State the recovery deadline and rollback conditions in the live test. |
| Receipt refresh misses the freshness window | Configure receipt agents with a target start interval no greater than one third of the receipt lifetime, capped at the ordinary 30-second interval. Count reconciliation work toward that interval; after an overrun, start the next serial pass without an additional idle interval. Preserve explicit boot-recovery backoff. Retain the configured receipt-age and round-expiry checks; missing current observations must prevent advancement and follow the declared expiry recovery policy. | Generated-configuration and synthetic elapsed-time tests cover only scheduling. They do not prove convergence when one pass itself exceeds the receipt lifetime, or adequate API capacity when overrunning passes run back-to-back. A responsive-server admission-burst test checks the agent's bounded API client budget; it does not establish API server capacity, sustained demand, archive-depth scaling or native completion. Explicit caller rate limits remain authoritative. Native verification, API throttling, delivery latency and independent participant timing can still exceed the freshness window. Measure all participants during live transitions; configuration tests do not establish completion within a deadline or zero workload interruption. Preserve expired-round and observation-timeout failures, and verify rollback separately. |
| Receipt agent release is mutable or malformed | Reject receipt-enabled controller startup before API discovery, agent rollout or journal publication when the configured agent image lacks a full digest, the runtime image ID is invalid, or cloud agents use a host-binary entrypoint. Correct the configuration and restart with the intended immutable release. | Configuration tests do not prove that the image exists on each node, matches the admitted runtime ID, or can submit authenticated receipts. Rejection permits no transition-induced interruption; it does not roll back a previously published transition. Recover existing transitions through their recorded phase and expiry policy, retaining failed observations and checking workload and management continuity. |
| Successor graph differs from archived serving transport | Bind starting member keys, devices, permissions, destinations and relay choices to the exact terminal predecessor. Preserve the selected receiver graph through preparation, abort and rollback, including a second successor. Reject changed identities, reused listeners and nested historical graphs. Verify allocation growth and withdrawal independently. | Pure graph and simulated-storage tests do not prove API atomicity, native execution or uninterrupted forwarding. Matching member keys alone does not authenticate source permissions. Advertisement and relay dependency tests with synthetic views prove graph selection and receipt ordering only; they do not prove BGP convergence, live relay continuity or native retirement. Rollback to a predecessor receiver must retain receiver routing and use that graph's destinations and direct relay owner, rather than select the original tunnel. A decoded version-two plan is not native execution authority; live admission, history validation and host locking remain required. Isolated native phase tests cover direct cloud and site paths, forward completion, abort, rollback, idempotent retries, foreign-route retirement rejection and completed-state restoration of a deleted route without reviving retired receivers. Recovery must select the journal's serving receiver graph after abort or rollback as well as forward completion. Their synthetic views and already-prepared authenticated peers do not establish controller publication ordering, cold preparation, source exceptions, relay changes, reboot recovery or workload continuity. Both IP-family routes are checked, but the cryptographic transport is IPv4 within one namespace and no IPv6 payload stream is measured. |
| Successor receiver route selection and rollback | Admit predecessor and successor device identities independently for each destination. Before switching, verify the installed route belongs to an admitted generation with exact keys, source permissions and a current handshake. Permit the original tunnel only for destinations explicitly assigned to it in the admitted graph; a first-transition worker sharing the route table must not authorize root fallback for a successor worker. Exercise both address families with one root destination and one receiver destination, and verify that a rejected mixed graph leaves routes unchanged. Switch and roll back both address families; rejected foreign state must remain unchanged. Exercise source-specific exceptions separately: admit both receiver generations, keep their selectors bound to the allocated source address, and preserve the actually installed receiver during withdrawal staging. Source-route inspection must reject conflicting candidate identities before mutation. Require zero workload and management interruption in the live campaign. | Native route readback and loopback handshakes do not establish workload delivery, remote transport, journal publication ordering or source retirement. State whether the test carries IPv6 payloads or merely installs IPv6 routes, and whether it measures traffic during switching. Two accepted candidate graphs do not authenticate their history; the executor must independently validate their journals and participants. |
| Listener pool exhaustion or conflicting persisted reservations | Retain every generation's occupied ports. Reject exhausted pools, duplicate ownership and malformed reservation records without partial assignments or changing active forwarding. Reload committed reservations after a write conflict; release a listener only through verified retirement. | Serialization and allocator tests do not establish API atomicity, socket availability or safe listener retirement. A smaller new pool must not make predecessor ports available for reuse. Versioned reservation records must reject readers that would discard other generations. Live testing must specify the recovery deadline and confirm existing traffic remains uninterrupted during rejection. |
| Credential expiry and producer outage | Cross a real expiry boundary; verify projected credentials and actual observation requests. On renewal failure, preserve previous allocations and report observation failure. | Successful Secret publication does not prove projection or AWS authorization. A shorter run does not establish 24-hour credential continuity. |
| Cloud bootstrap and stale ownership | Create workers through provisioning and CAPI, then verify authenticated site connectivity, Node registration, native CNI readiness and usable pod traffic. For obsolete owners, use normal lifecycle deletion and verify withdrawal before accepting reused addresses. Preserve existing-site connectivity. | A running instance, Ready infrastructure object or downloaded agent is not a working Kubernetes worker. Cleanup followed by recovery does not prove the original bootstrap met its deadline. Distinguish stale ownership from a live address collision; never remove a live owner's state merely to make provisioning pass. |
| Direct cloud edge reconciliation with reserved journals | With both cloud workers owning journals, add, restore and withdraw their direct peer publication through normal reconciliation. Retain the complete inventory, local boot and agent identity checks. Require zero interruption on existing workload and management lanes; measure new-edge convergence within 60 seconds. On rejection, preserve serving transport and retry only against freshly admitted publication. | Graph tests distinguish cloud edges from site-facing receivers and reject competing owners, site permissions and receiver identities. Admission tests simulate native execution and do not establish kernel forwarding, restart continuity or the live convergence deadline. Test both traffic directions, journal phases, address families and withdrawn-owner address reuse explicitly. Cloud reconciliation does not replay the site's legacy forwarding cache; unrecorded old cloud peers are not inferred to be safe to delete. |
| Credential expiry and renewal | Exercise renewal and actual requests across expiry, including producer failure. No timed soak is required. | Short runs do not establish long-term reliability. Removing the soak requirement does not waive credential-expiry or fault-injection tests. |
| Resource pressure during rollout | Retain the same management and workload availability requirements while measuring controller CPU, memory and storage pressure through activation and rollback. Before starting, check disk-pressure conditions and budget guest space above eviction thresholds for the full capture and sample window. Archive and hash-verify completed evidence outside the guest before removing only its redundant guest copies. | Ready nodes and unchanged process IDs do not prove API availability. A short healthy baseline does not establish rollout headroom. Record actual VM resources; changing capacity requires a fresh observation and cannot turn the earlier failure into a pass. An evicted source container may lose its original sample files: retain all recoverable streams and explicitly mark missing lanes, never substitute a replacement container's samples. Replacing a probe requires new identity receipts and a fresh baseline. Correlation with pressure does not establish the failure's cause; retain kubelet eviction reasons and resource measurements. |

Node image-cache additions, removals and reordering must not invalidate retirement
admission. Cached images do not identify the running agent: retain independent
Pod, container and runtime-image authentication. A source-comparison regression
does not qualify cleanup during a live image pull or garbage-collection cycle.

After full retirement admission, checkpoint, pre-cleanup and final survivor
admission boundaries reuse the live-source recheck instead of reconstructing
immutable ancestry. Recheck every reserved journal, inventory, mesh and local
Node/boot identity, and reject changed checkpoint scope. Retain initial full
admission and full admission after native cleanup, plus local checkpoint history
validation. A ConfigMap read-budget
regression establishes reduced repeated reads only; it does not establish live
latency, native absence or completion within the receipt lifetime. Inject an
inventory revision change at the final survivor boundary and require rejection;
this API race test does not establish live cross-object atomicity.

With multiple withdrawals, survivor admission authenticates the shared reserved
histories and route ownership once per call. Bind each withdrawal to that same
inventory, journal incarnation and mesh snapshot, then independently recheck its
captured consumer and live sources. Never cache this admission across calls.
The two-retirement API fixture bounds root reads and rejects missing or replaced
sibling consumers, malformed records and concurrent inventory changes. It uses
synthetic terminal proofs and no predecessor ancestry; it does not establish
actual owner deletion, native cleanup, live latency or authenticated completion.

Before native phase execution and during neighbor reconciliation, intermediate
guards may reuse fully admitted retirement histories while re-reading every
reserved journal, inventory, mesh
and retained local identity. Serving journals still require live adoption,
allocation and participant admission. Retain full set admission at operation
entry and after native phase execution or final neighbor verification. Test
concurrent inventory mutation during a guard, changed scope, node, boot and key,
and cancellation. The read-budget fixture has
one retirement and no successor ancestry; fewer reads do not establish live
latency, address release or completion within the receipt lifetime.

Receipt collection may reuse the current request's fully admitted retirement
source. Recheck consumer policies and orphan-owner absence, then every reserved
journal, inventory and mesh around authentication and completion. Never carry
admission across requests or replace exact revision checks with receipt digests.
The standalone constructor still performs full source admission. Real-API tests
bound repeated reads and reject changed scope, withdrawal, source revisions,
policy failures, unavailable reads and concurrent journal or inventory changes.
TLS tests retain authentication and completion-race checks. Synthetic native
proofs and reduced reads do not establish live throughput or safe allocation
release; the original freshness and connectivity gates still apply.

Retirement receipt delivery may overlap subsequent serial native reconciliation,
with at most two HTTP submissions in flight per agent. Dispatch must join all
submissions before returning or closing its client, including after an unrelated
admission failure. Preserve each failure and the original observation timestamp;
one rejected request must not cancel other submissions. Serving-phase delivery
remains serial. Blocked-callback and synthetic TLS tests exercise backpressure,
joining, cancellation and timestamp preservation, not native cleanup, controller
authentication or freshness acceptance. Existing dispatch fixtures use simulated
native callbacks. Re-run authenticated source-race checks and observed multi-node
cleanup: overlapping completion writes may invalidate another operation's exact
source checks, and lower delivery latency alone does not qualify cleanup.

Only retirement collections explicitly declare observation authentication
read-only. Those callbacks may overlap within the collector's existing eight
admission slots; mutating handover observation and all completion writes remain
serialized. Recheck shutdown, capacity, collection identity and original receipt
expiry after reacquiring the collector gate. A slower request must not erase a
newer observation or replace a collection installed during its authentication.
Retirement callbacks retain source checks around authentication, and completion
reauthenticates the whole batch with exact source checks and inventory CAS.
After a failed retirement completion, retain independently admitted observations
only within their original freshness bounds. A later authenticated submission
must resolve current authority and reauthenticate the complete batch before
another write; do not retry writes automatically or renew retained timestamps.
Changed collection bindings and incarnations must discard the old batch.
Successful completion clears credentials; rejected mutating handover batches
retain their existing discard behavior. Exercise a real authenticated sibling
commit racing the inventory CAS, then complete through a single-consumer
resubmission. Synthetic native observations in that API test do not establish
live cleanup, reboot recovery, provider deletion or release.
Synthetic transport tests cover overlap, serialized commits, queued expiry,
cancellation and concurrent collector changes; they do not establish native
absence, real API throughput or production freshness. Real authenticated source
and commit races and observed multi-node cleanup remain required. The real-token
TLS concurrency fixture overlaps two requests for one consumer against the same
inventory revision and requires one commit and one stale-source rejection. Its
synthetic native proof does not qualify multi-consumer aggregation or cleanup.

Reservation finalization must consume the exact source returned by
`ReadVerifiedReceiveRetirementCompletion`. A boolean verification followed by
an unrelated newer inventory read cannot authorize a conditional write. Preserve
the verified inventory UID/resourceVersion, captured withdrawal and journal/mesh
identities; recheck owner absence, consumer identity and observation freshness at
the final write boundary. The real-API source-binding fixture changes inventory
metadata after verification and requires a conflict when its old revision is
used. This proves conditional-write binding only, not finalization or release.

The storage-only finalization writer conditionally records a finalization object
beside its exact consumed completion, preserving all journal and allocation
reservations. It rejects replacement finalization and later completion refresh,
and invokes a required caller check immediately before its inventory CAS.
Identical retries require a fresh admitted revision and the same final check.
Real-API storage tests cover conflicting writers, cancelled and rejected checks,
lost responses, stale retries and preservation by ordinary publication writers.
Their synthetic records do not authenticate consumers or define finalization
semantics. The writer is not connected to live reconciliation; older strict
readers reject its new field, so activating writes requires compatible readers
and rollback qualification first. Existing completion authority hashes are not
reinterpreted to accommodate finalized records.

The authenticated finalization layer consumes verified completion and rechecks
caller-supplied ownership/policy, current receipt incarnations, source revisions
and observation age at the conditional write boundary. Its typed record binds
the consumed evidence, captured inventory metadata, mesh/journal identities and
observation policy. The record time is controller verification time, not an API
server transaction timestamp; observation age is checked again before submission.
The historical reader validates receipt age at that recorded time. It does not
extend receipts into current absence authority, and intentionally does not treat
a subsequent consumer boot as erasing historical consumption. Agents still need
local durable-fence admission and current native checks; release needs separate
live ownership checks. Controller activation requires compatible readers and
rollback targets throughout the retained consumer set.

Real-token finalization tests reject owner/source changes, a consumer reboot,
expiry during the final check and a racing API write. Lost responses retain
verifiable evidence; altered records and incomplete identity/binding data fail.
A deterministic future-time projection is not an elapsed-time soak. Native
cleanup is synthetic, the fixture has one consumer, and metadata or simulated
boot changes do not qualify real reboot recovery or address reuse.

Agent inventory checkpoint admission preserves any saved finalization and its
consumed completion bytes across subsequent reconciliations and boot identities.
Removing or replacing either record fails before checkpoint replacement. Initial
finalization, completion refresh before finalization, and independent retirement
progress remain permitted by this fence; their authority requires separate live
admission. Filesystem tests use synthetic evidence and simulated boot identities.
They establish regression rejection and read-only admission, not authenticated
finalization, native absence, actual reboot recovery, or reservation release.

For a finalized withdrawal, the agent validates historical evidence against the
frozen journal's observation policy before local execution and again afterward.
It still admits the current agent incarnation, holds the host lock, persists the
retirement fence, performs native cleanup/readback and re-admits current source
state. Success suppresses receipt renewal; it does not skip local cleanup or
release reservations. A policy mismatch or changed evidence fails closed.
Real-API execution tests use synthetic legacy finalization with old timestamps
and boot identities, reject malformed evidence before native execution, and
reject native errors or agent/inventory changes afterward. A TLS dispatch test
checks that successful finalized maintenance sends no receipt. Native callbacks
and finalization authentication in these agent fixtures are synthetic; actual
token authentication is covered separately. These tests do not qualify live
cleanup latency, reboot recovery, mixed-version rollout or rollback.

The receipt policy's `finalizeOrphanRetirements` setting defaults to false.
Enabling it consumes authenticated completion for orphaned withdrawals only;
ordinary deletion continues to require renewable completion until its compute
removal protocol has finished. The finalizing collection requires its observation
lifetime to match the frozen journal and rechecks owner absence and consumer
policy after completion, then uses the existing authenticated finalization CAS.
A failed or ambiguous response must not be treated as successful finalization:
read and validate the historical record before deciding whether to retry. Neither
completion nor finalization releases reservations. Enable this setting only
after all readers and the rollback candidate understand finalized inventories.
The broader `finalizeRetirements` setting also defaults to false. It includes
normal withdrawals only after fresh reads establish that their claim, Machine,
Node and adoption Secret are absent. A remaining claim may still be removing
provider resources; existing or replacement owners retain renewable completion.
Disabling finalization and release does not disable authorized withdrawal
publication or native cleanup. A controller upgrade may therefore fence and
publish pending deletions. Assess those mutations against their retained
authority before rollback; unchanged policy flags do not prove unchanged state.
Read errors must fail admission rather than count as absence. Recheck absence
and consumer policy at finalization boundaries. The orphan-only setting must
not change the behavior of normal withdrawals.

Controller lifecycle tests reuse completed preparation journals in separate
real API storage. They must retain renewable completion while each owner exists,
reject owner-read failures, and reject replacements after admission without
changing inventory revisions. Require separate fresh observations for
finalization and release. These tests use synthetic owner/consumer objects,
TokenReview answers and native observations; they do not establish provider
termination, token cryptography, native cleanup or live continuity.

Real-token collection tests cover invalid credentials, observation-policy
mismatch, owner rejection after completion, successful consumption and rejected
replay. They use synthetic native observations and do not establish controller
orphan discovery, live cleanup latency, rollout compatibility or release.

For configuration rollback after finalization, disable new finalization writes
on a compatible binary while retaining the populated records. Require unchanged
agent Pod identities, verified running executable hashes, successful historical
finalization and current orphan-absence checks, and unchanged reservations and
consumed evidence. Observe baseline and operation-wide workload, API and
management traffic with zero failures. This scenario covers the policy change
and resulting controller restart only. It does not qualify an older binary,
agent restart or reboot, allocation release, address reuse, another platform,
or a different network profile. Missing receipt progress logs alone do not prove
that agents stopped submitting; verify retained state and local maintenance
independently. A historical finalization is not a renewed absence observation.

Finalization remains a separate protocol: persist an irreversible retirement
record before removing active reservations, retain durable host fences against
old boot recovery, and admit new ownership only after matching release evidence.
Test crashes between those steps, competing reservations, stale/replaced owners,
consumer reboot, mixed-version readers and rollback. Current completion records
and the source-returning verifier do not implement this protocol. Do not shrink
inventory, delete journals or discard host checkpoints to bypass it.

The storage release primitive atomically moves an exact finalized publication
from active inventory membership into `released`, retaining withdrawal,
completion, finalization and journal incarnation as a permanent retired-owner
record. Reservation and publication must reject that Machine UID thereafter.
The frozen journal remains stored. Release requires the exact admitted inventory
revision and a caller-supplied check of live release authority immediately before
CAS; storage does not authenticate that authority. Conflicts are not retried.
An ambiguous response requires re-admission before an identical retry, which
must repeat the authority check and cannot replace the retained evidence.

Real-API storage tests exercise invalid or changed evidence, denied authority,
concurrent reservation, cancellation, lost response, identical retry, attempted
resurrection and preservation of unrelated ownership. Their finalization and
release authority are synthetic. They do not prove consumer fencing, native
absence, safe address reuse, reboot recovery or live release. The controller's
release caller is disabled by default. Released-inventory admission and durable agent recovery must be completed and qualified before
activation; unadmitted checkpoint
shrinkage remains rejected. Older strict readers reject `released`,
so compatible deployment and rollback are prerequisites.

Agent checkpoint admission must preserve every previously saved released-owner
record, including across a boot identity change. Reject dropped or changed
evidence and attempts to put that identity back into active reservations or
publications. Filesystem tests exercise these regressions and confirm that
validation never rewrites the saved checkpoint. They use synthetic evidence and
modeled boot identities, not authenticated release or actual reboot recovery.
They permit unrelated retired records only as a monotonic history change;
separate source admission must validate those records. A matching retired record
alone still cannot authorize shrinkage of a saved active reservation set.

The released-journal reader verifies retained frozen history and ancestry,
withdrawal identity and captured consumers, and historical finalization under
the supplied observation policy. It rechecks exact inventory, journal and mesh
revisions before returning. The retired owner remains outside active membership
in its result. Later mesh data or consumer boot changes do not erase historical
consumption; this reader does not establish current owner absence or authorize
native cleanup, restoration, reservation release or address reuse.

Real-token fixture tests first establish completion and finalization, then use
synthetic release admission to exercise this reader. An active finalization is
not a release. Later mesh changes invalidate an earlier read's revision check
but permit a new historical read. Changed inventory/journal identity, observation
policy, consumed evidence, withdrawal history and a missing retained journal
must fail with no admitted result. These tests do not qualify a live release
controller, native recovery or allocation of a replacement worker.

The agent's guarded checkpoint path admits released records together through the
historical reader and pins the complete inventory revision. Shared cohort journals
are loaded once per admission, with each record checked under its frozen
observation policy. A fresh recheck reads every retained journal, inventory and
mesh; it must not reuse cached API responses. Read-count tests must also reject
changed revisions and damaged cohort history. Reduced reads alone do not prove
that a live retirement meets its receipt deadline. A captured consumer
must retain the same private local withdrawal fence; any locally saved
finalization must also match. Renewable completion need not match if finalization
was not yet saved locally, because the original native retirement receipt already
required durable withdrawal intent. Local participant journals remain present and
must match retained history. The retired worker cannot admit survivor release.
Only this admission permits the corresponding active membership to shrink; the
ordinary checkpoint validator still rejects shrinkage. Bind admission to the exact
proposed checkpoint and recheck sources and current Node/boot at the write boundary.
Within one locked set checkpoint, nested retirement checkpoints may reuse that
released-history admission only when its binding matches the exact proposed
inventory checkpoint. Each nested write still rechecks current sources and
Node/boot identity. Do not retain this admission across reconciliation passes.
The mixed serving/retiring/released regression uses real API storage and
synthetic legacy finalization, with no native callback. It checks bounded
released-history reads and rejects empty or mismatched admission, changed
membership, cancellation and changed released journals without rewriting the
inventory. It does not establish live cleanup, release policy, reboot recovery
or production convergence latency.

Real-API/filesystem tests cover finalized and withdrawal-only fences, simulated
previous boots, missing or changed local records, non-private files, missing
journals, transferred admission and a changed boot at the write boundary. Native
cleanup, legacy finalization and release authority in this fixture are synthetic.
The guarded phase and retirement checkpoint paths retain retired journal files;
this does not qualify offline recovery, an all-retired inventory, a newly joined
consumer, native address reuse or a live release controller. Those paths remain
separate activation requirements.

Agent reconciliation timing diagnostics report fixed operation/stage labels,
elapsed milliseconds and success only, with at most sixteen stages per record.
They must not include configuration, object identities, credentials or error text.
Emission follows release of the operation's host lock. Timings include nested
operations: do not sum parent and child records. A stage can include API reads,
local computation and storage; its elapsed time alone does not identify the
cause of delay. Failed or missing records are not cleanup evidence. Preserve
independent receipt freshness, native readback and continuous traffic gates.

Exercise repeated agent admission reads against a responsive API, not only an
initial request burst. By default the agent relies on Kubernetes API Priority
and Fairness, matching the controller, while preserving explicit caller QPS,
burst and limiter settings. Request and phase deadlines remain mandatory.
A local HTTP regression proves only that default client throttling does not
consume the admission deadline. Live qualification must still measure API
availability, resource pressure and receipt freshness under all consumers' load.

Consumer-policy admission reads each shared DaemonSet owner once per admission,
while independently validating every retained Node. Re-admission must read the
owners again and detect deletion or replacement; owner snapshots must never be
cached across requests. Fake-API tests verify the read budget and replacement
visibility, not API latency, live authentication or cleanup convergence.

Receipt collection permits at most eight concurrent read-only source resolutions.
Authentication, batch changes and completion remain serialized and must recheck
the resolved source; concurrent reads do not authorize stale writes. Saturated
admission returns a retryable busy response before callbacks run. Waiting for the
mutation gate consumes the same request deadline and cancellation must suppress
callbacks. Synthetic concurrency tests establish overlap, bounded admission and
error behavior only, not live API throughput or authenticated retirement within
the observation lifetime.

A rejected consumer observation must not discard other consumers' independently
admitted receipts. Retained receipts keep their original timestamps and expiry;
completion must still authenticate every consumer and recheck current source
and identities. Exercise a rejected submission between two valid consumers and
verify that its credential is never retained. Also inject a source lookup
failure, an unknown member, duplicate submitted membership and invalid resolved
membership between valid consumers. These failures must neither invoke mutation
callbacks nor erase or refresh earlier authenticated observations. A subsequent
valid submission must still pass full completion authentication. Synthetic
accumulation tests do not prove that live consumers can deliver within the shared freshness window.

At debug logging level, receipt collection reports authenticated consumer ages,
missing consumers, and source, queue, observation and completion timings. Reports
use hashed collection/consumer identities, include at most sixteen consumer
entries, and exclude credentials, request bodies, source objects and callback
errors. Logging runs after collector locks and admission slots are released.
An accepted report proves partial admission only; even a completed report is not
reservation-release authority. Missing reports can reflect rejected admission,
logging configuration or delivery gaps and must not be interpreted as an absent
consumer. Synthetic diagnostic tests cover bounds, redaction and lock release,
not live timing or full lifecycle qualification.

Version-three and version-four retirement completion records exclude all
completion evidence from their inventory authority hash, so refreshing a sibling
completion does not invalidate an otherwise current proof. Journal identities,
withdrawal intents, membership and every other inventory field remain bound.
Version-two records retain their original own-record-only exclusion and must be
refreshed through complete authentication to acquire the new semantics.
Version-four receipts bind this authority hash and inventory metadata, excluding
only resource version and managed-field bookkeeping. A sibling completion must
not reset collection when withdrawal authority is unchanged. The final write
still binds and compares the exact admitted inventory revision, including after
authentication; source races and CAS conflicts must fail. Versions two and three
keep their original revision-bound receipt interpretation. Older readers reject
version four; retain a compatible runtime after such writes. Completion still
requires current identities, fresh observations and an uncontested write. Scope
hash tests and synthetic evidence updates do not prove two live workers can
finish retirement within the required freshness window.

For simultaneous withdrawals, require independent production verification of
each completion against the configured receipt lifetime after both records
exist. Record presence or a successful rollout is not a passing deletion gate.
Record verification failures separately from packet and management continuity:
uninterrupted forwarding does not establish that cleanup can safely finish.
Diagnostic verifiers must use the trusted policy's receipt lifetime and a reader
compatible with the candidate's completion format; a relaxed diagnostic lifetime
can isolate a binding defect but cannot qualify retirement.

Dispatch all local withdrawal targets before other receiver maintenance,
preserving their discovery order. A later background read failure must not
prevent an earlier cleanup attempt merely because opaque machine IDs sort in
a different order. Synthetic dispatch tests establish ordering and error
retention only; live multi-worker retirement must still meet the shared receipt
freshness limit and maintain forwarding throughout cleanup.

When multiple receivers fail in one reconciliation pass, retain earlier execution
or delivery errors alongside any later API or identity-read failure. Exercise this
with a rejected cleanup followed by a failed read for another receiver. A synthetic
dispatch test establishes error retention only; it does not establish native
cleanup, authenticated completion, or recovery within the live deadline.

For every row, name untested combinations explicitly, including IP family,
architecture, CNI, operating system, endpoint placement and site profile. Keep
run timestamps, actual identities, measurements and failure narratives in private
evidence and commit messages, rather than turning these descriptions into a run
log. Missing observations, interrupted samplers and recovery beyond the stated
deadline remain incomplete or failed even if a later manual check succeeds.

When receive journals reserve part of a site mesh, ordinary peer retirement
requires both a complete current publication and durable prior ownership.
Journal-owned peers remain excluded. Changed kernel source permissions or
foreign routes depending on the retiring peer block withdrawal. A successful
whole-peer deletion does not qualify source narrowing on a still-published peer,
allocation churn, or retirement of independently prepared receiver devices.
The isolated native neighbor test measures direct IPv4 and IPv6 address reuse,
shared connected-subnet preservation, foreign-route rejection and an unaffected
IPv4 peer's packet stream. Its WireGuard transport is IPv4; it does not measure
IPv6 transport, an unaffected IPv6 stream, CAPI cleanup, live-site management
continuity or AWS bootstrap. Those remain separate cases.

For a reserved cloud worker, the admitted journal's committed base supplies
legacy ordinary-peer retirement intent. The agent transfers that intent into
the existing retirement sidecar before writing a base-hash-specific
`.cloud-legacy-*.json` marker and attempting native changes. The marker means
intent was transferred, not that deletion succeeded. Preserve the neighbor
checkpoint, retirement sidecar and import markers together during backup or
rollback; deleting a marker can replay old intent after address reuse. Import
admission still requires current journal and node identities, and native
retirement still rejects changed permissions or foreign route dependencies.
Admission tests with interrupted callbacks and isolated kernel tests do not
establish a successful live upgrade of a worker carrying legacy peers.

Site receiver preparation preserves validated public ownership snapshots in a
separate `.owners` directory before replacing a cache or preparing a device.
These snapshots are not offline recovery inputs, installation receipts, or
deletion authorization. Test crash recovery and live withdrawal separately:
retirement must verify current authority, exact device ownership and absence of
route or address dependencies before removing a device and its sysctl policy.
Under an admitted journal inventory, withdrawn ordinary receiver owners use
the dedicated-device retirement guard. Both journal generations and published
owners remain protected. Ownership records are removed only after guarded
retirement verifies device absence under the host lock; failed retirement
retains them for retry. Local record cleanup can finish after the admission
deadline, but every native deletion still requires fresh live admission.
Legacy preparation without an admitted journal inventory does not use this
cleanup path.
Retaining a snapshot alone does not satisfy last-worker deletion. Records from
earlier binaries may already be missing, and retained snapshots require pruning
only after verified retirement; neither case is qualified by cache unit tests.
The isolated native tests cover IPv4/IPv6 permission matching, foreign
dependencies and idempotent device deletion. They do not prove live mesh
withdrawal, workload continuity or recovery of already-lost ownership records.

For k0s controller maintenance, observe exec and logs through the controllers
that remain running as well as the endpoint being restarted. Count-driven
Konnectivity reconciliation can restart otherwise healthy proxy servers and
agent pods when the active controller count changes. Healthy `/readyz`
responses on those controllers do not establish uninterrupted kubelet access.
The local API-endpoint maintenance allowance does not waive these other
management failures. Qualify the existing [two-stage discovery
migration](../providers/k0s/README.md#konnectivity-lease-discovery), including
agent-first ordering and resource rollback, before relying on isolated
controller maintenance. Enabling the new mode only on the restarted controller
does not prove that the remaining controllers have stopped count-driven churn.

Runtime maintenance is distinct from a cloud-provisioning agent upgrade.
An explicitly scoped kubelet or container-runtime restart may interrupt exec
and logs on the maintained worker, with recovery within 60 seconds. API health,
management access to other nodes, and existing workload traffic must remain
uninterrupted. Record the affected Node UID, operation window, every failed
request, and the first successful recovery for each affected management lane.
Observe activation and rollback as well as the candidate restart, and retain
the autonomous rollback limit of five minutes.

API readiness can precede recovery of the local CRI socket and workload log
draining. Record these recovery points separately. Retained container identities
do not establish responsive workloads: synchronous stdout or stderr writes can
block application replies while the runtime is unavailable. Preserve workload
logging and request deadlines during this test. Correlate application timestamps,
runtime log timestamps and failed requests; a client timeout alone does not
measure the duration or establish the cause of a network outage. If observation
fails before all planned controllers are activated, report the partial scope and
retain the original observers through autonomous restoration.

Keep runtime-maintenance evidence separate from campaign rows requiring zero
management failures. This allowance does not apply to agent upgrades, endpoint
changes or ordinary reconciliation, and does not turn a failed continuous
management observation into a passing `agent-upgrade` result. A maintenance
rehearsal alone does not qualify deployment.

`harness/e2e/observe/management.py` samples each explicitly named API directly,
plus Kubernetes exec and logs to each bound ordinary probe pod. Supply repeated
`--server` and `--target pod=NodeUID` arguments, an explicit bastion and namespace,
and a fresh private `--output` directory. Probe pods and matching Services must
already exist. The probe container must serve the netexec HTTP endpoint and
provide `wget`.

Wait for `READY.json` before starting an operation. Record its timezone-aware
start and finish times outside the observer, then create `OUTPUT/STOP`. Wait for
the original observer process to terminate and require a successful result.
Call `management.assess_window(output, started_at, finished_at)` to verify raw
sample hashes, process identity, ordering, sampling gaps, and samples bracketing
the operation. Deadline expiry, failed samples, and changed pod/container/Node
identities fail closed. A readiness receipt alone is insufficient. These are
sampled availability measurements; they do not establish traffic delivery
between samples or substitute for continuous ordinary-pod workload probes.

Offline cloud boot recovery carries released-worker withdrawals forward as
exclusions only when the full inventory digest matches prior successful native
readback. Released records must remain outside active ownership, retain their
finalization, and identify the captured surviving consumer. The retired worker
cannot restore itself. Recovery rechecks the exact saved files before and after
native work. Synthetic boot tests cover exclusion retention, changed files,
identity mismatch, active/released overlap and missing readback; they do not
prove actual reboot, authenticated release, all-retired recovery, retained local
participant-journal recovery or native address reuse.

Finalized reservation release must read and verify the retained finalization
before invoking live release policy for that exact admitted source. Repeat the
policy and full source checks immediately before the inventory conditional
write. A policy denial, cancellation, source change or conflict must preserve
active ownership. A lost response requires fresh historical readback; an already
released record cannot re-enter active release admission. Real-token/API tests
cover these boundaries with synthetic native cleanup and live-policy callbacks.
They do not qualify consumer recovery, compatible rollout/rollback, current
native absence, live controller release or address reuse; no live release caller
is enabled by these tests.

When all receiver reservations have been released, dispatch must retain the
phase-aware path instead of falling back to legacy cache replay. A directly
published site may maintain ordinary neighbors through the existing reconciler
after admitting all released histories, its current key/address/Node/boot, and
its durable local retirement records. Preserve retired journal files, reject
republishing a retired worker key, and recheck mesh, inventory and agent identity
through native execution. Cloud workers still need an active local journal;
this path does not admit relayed sites or followers without tunnel ownership.
The isolated API fixture exercises a replacement key with the old address,
previous-boot admission, missing local records, withdrawn site ownership,
republication of the old key, native failure and in-flight source/agent changes.
Its native callback and legacy finalization are synthetic: it does not qualify
kernel address reuse, actual reboot, live controller release, a newly joined
consumer, or mixed-version rollback.

Released worker keys also remain exclusions while other journals are active.
Exclusion planning alone cannot authorize execution: checkpoint admission must
verify retained finalization and local records before native work. A replacement
key may reuse released addresses, but an active plan or ordinary peer cannot
restore the retired key. Tests distinguish this negative planning authority
from authenticated executable checkpoints; synthetic finalization may produce
exclusions but must not pass the execution boundary.

Active maintenance and overlapping retirement must pin released journal
incarnations throughout execution, including final admission. Released keys
remain excluded in both reserved and serving route views. Isolated API tests
mutate a released journal during active maintenance and during another worker's
retirement; either operation must stop. Their finalization records are synthetic
and do not establish native cleanup or controller release safety.

The isolated native neighbor scenario retires a peer, installs a different key
at its old address, verifies encrypted traffic from the replacement and rejection
of the old sender, then reapplies recovery. Stale cleanup must reject the new
owner's routes while an unrelated peer's stream continues. This uses guest mount
and network namespaces and real WireGuard devices; it does not exercise API
release authorization, durable checkpoint admission, actual VM reboot, or
mixed-version rollback.

After reservation release, controller reconciliation must retain named, read-only
agent access to each released journal and its immutable ancestry. Released owners
do not acquire pending successor archive grants. A changed retained incarnation
must stop Role reconciliation before removing existing grants. An authoritative
empty active inventory is a no-op for orphan discovery, allowing ordinary mesh
reconciliation to continue without mutating retained release records. Controller
fixtures exercise empty and all-released membership, archive retention, repeat
reconciliation, and incarnation mismatch. Release evidence and Role writes are
synthetic in these fixtures; they do not qualify RBAC propagation, authenticated
release, actual agent recovery, or live address allocation.

The receipt policy's `releaseOrphanRetirements` setting defaults to false and
requires `finalizeOrphanRetirements` plus authenticated agent receipt delivery.
Enable it only after release-aware readers, durable recovery, native reuse and
the rollback candidate are qualified. It opts agents into fresh native-absence
observations after finalization; default finalized maintenance still suppresses
receipts. Every observation requires the existing host lock, durable withdrawal
checkpoint, native cleanup, current agent/boot identity and post-cleanup source
checks. The post-finalization digest cannot reuse consumed completion receipts.
The broader `releaseRetirements` setting requires `finalizeRetirements` and
authenticated delivery, and opts agents into the same fresh observations.
For joint normal withdrawals, use the existing atomic cohort release collection,
checking every member's owner absence and shared consumer policy at each
boundary. All members must already have compatible consumed finalization.
Neither the orphan-only release setting nor one owner's absence authorizes a
joint release. Keep these settings disabled until the corresponding live
recovery, continuity and rollback gates are qualified.

With normal finalization enabled, reconciliation must remove the captured
adoption Secret after verified cleanup completion and live absence of the
Machine, claim and Node. Peer withdrawal can already have removed the name
from ordinary pruning, while its routing reservation remains intentionally
retained. Require the journal's exact adoption UID, current receipt incarnation
and age, live source rechecks, and UID/resource-version deletion preconditions.
Never delete a replacement Secret, bypass its finalizers, or release the routing
reservation as part of adoption cleanup. Missing or expired evidence and failed
owner reads must leave the Secret intact.

The normal lifecycle regression exercises reconciliation, source and owner
races, disabled policy, invalid evidence, replacement identity and cancellation.
Its journal storage is real; owner objects, TokenReview and native observations
are synthetic. Separate real-API deletion cases verify rejection of changed
resource versions and replacement UIDs, plus successful and already-absent
deletion. These tests do not prove provider termination, native absence or
workload continuity; require the live retirement/finalization campaign as well.

For finalized orphan withdrawals, the controller reuses the bounded receipt
collector and current image/consumer policies. Completion authenticates every
current consumer, pins the exact inventory/mesh/journal source, rechecks owner
absence, and checks observation incarnation and age again at the release write
boundary. The exact finalized publication moves to retained released history;
consumed completion and finalization are not renewed. An ambiguous response
requires historical readback; the old collection cannot perform a second release.
Ordinary deletion does not use this release path. Disabling the policy stops new
release observations and writes; it does not erase existing released records.

Real-token/API tests exercise missing finalization or policy, invalid credentials,
wrong image policy, consumed receipt replay, incomplete/stale batches, owner and
inventory changes, boot replacement, expiry/cancellation at the write boundary,
CAS conflicts, lost responses and successful release. Agent tests exercise fresh
post-finalization observation and suppression on native, identity or source
failure. Native observations and rollout policy are synthetic in these fixtures;
they do not establish live controller routing, multi-consumer release timing,
RBAC propagation, actual reboot recovery, or version rollback. Activation remains
subject to those separate lifecycle checks.

History replay may reuse a canonical round's digest and member lookup within
that replay, but must rebuild them whenever the round changes. Validate every
event's timestamp, receipt binding, membership, freshness and chronology, and
revalidate history on each load. Corruption after a successful replay must still
be rejected. Synthetic replay benchmarks measure local validation cost; they
do not establish live receipt convergence, API capacity or finalization latency.

A dispatch pass may clean an admitted retirement cohort once and issue separate
member-bound receipts from that execution. Preserve every member's admission,
checkpoint, native readback and post-cleanup validation under one lock and
deadline. Every receipt retains the earliest native observation timestamp;
partial failure suppresses all receipts. Reuse is limited to that dispatch pass.
Finalized members remain subject to their own observation policy. The isolated
API/native fixture checks cleanup counts, member bindings, failure suppression
and authenticated release; it does not qualify live convergence or reboot.

Joint retirement must preserve each withdrawal's captured consumers. Group only
published version-two withdrawals from the same exact captured mesh when every
member names the other departing workers by committed identity and all members
have identical surviving consumers. An unpublished sibling, missing journal,
changed identity or differing capture must not excuse a required observer.
Every surviving observer must checkpoint and clean every member under one host
lock and one execution deadline before issuing an observation. Partial cleanup,
source changes or agent replacement must suppress that observation. Version-five
completion binds the ordered group separately from individual-worker evidence;
older completion versions keep their existing interpretation. Historical
finalization must validate its explicit members rather than expanding them to
include later withdrawals. Individual release of a joint completion is forbidden;
atomic release must preserve all member history and recovery fences together.

Joint-execution API tests use synthetic serving histories and native callbacks.
They must cover a failed second cleanup, retry of all members, shared deadlines,
and source changes between members. These fixtures do not qualify actual kernel
absence, authenticated joint completion, atomic release, stale-worker replay,
address reuse, live continuity or rollback to readers predating version five.
Those gates remain mandatory before deployment or resource release.

Native joint fixtures must run each exact case in a fresh guest mount/network
namespace with the existing per-case deadline. Exercise site receivers, follower
routes and surviving cloud neighbors for completed and interrupted prepare,
switch, drain, retire and abort histories, alongside individual-worker regression
cases. Keep real IPv4/IPv6 forwarding, WireGuard identity, source rules, surviving
receivers, local BGP preferences and executable caches intact for unrelated owners.

A foreign dependency on the second site receiver must suppress a joint receipt
after the first cleanup succeeds. Partial cloud adoption publication must likewise
block completion until every retiring peer is absent. Retrying verified cleanup
must remain idempotent. Follower cleanup must reject stale pre-retirement ownership
writes for either member. Cloud survivor route recovery and simulated prior-boot
checkpoint recovery must not revive withdrawn destinations or peers.

These native tests use synthetic Kubernetes objects and serving histories. Their
previous-boot case edits fixture checkpoints rather than rebooting a VM. They do
not establish the combined real-API-to-kernel path, token authentication, packet
continuity, external BGP convergence, actual reboot, cloud lifecycle, live release
or version rollback. Those remain separate deployment gates.

The combined native/API fixture must start its own API server and datastore in
a fresh guest mount/network namespace and reuse the site retirement backend.
Require real dual-stack receiver, route, peer, local advertisement and executable
cache cleanup while preserving an unrelated route and the local WireGuard
identity. Include legacy-only and mixed legacy/receiver route placement:
preparing a receiver does not prove that switching moved every destination.
Reuse ordinary-neighbor admission to validate exact legacy device identity,
recorded peer permissions and absence of competing owners before allowing those
routes to remain during receiver-device cleanup. Re-admit and remove them in the
ordinary-peer cleanup step. Neither step alone permits a completion receipt.
A failed second cleanup, cancellation, agent replacement or source
change must suppress receipts; retry must remain idempotent. Authenticate fresh
native observations with a Pod-bound TokenRequest/TokenReview, finalize both
members, and atomically release the group using another fresh observation.
Verify retained history, reject re-reserving either retired identity, and reject
replaying the consumed collection.

This combined fixture uses synthetic Pod/Node status and completed serving
history, one surviving site observer, and absent target Node/adoption objects.
It does not qualify kubelet-attested execution, multiple live observers, provider
deletion, workload traffic, actual reboot, address reuse or rollback compatibility.
Those require the live deployment scenarios; fixture release is not deployment
approval.

Real-token joint fixtures cover two surviving consumers for completed and
interrupted histories. Require rejection without inventory mutation for missing
consumers, individual-worker and wrong-member digests, expired observations,
invalid tokens, old containers and cancellation. Finalizing one member changes
receipt authority; another member must obtain fresh authenticated observations
before finalizing. An earlier batch must not be silently rebased.

Retained joint history must load and verify every member journal and consumed
finalization, require identical explicit member sets, and pin their revisions.
Historical readback must survive later mesh ownership without granting cleanup
or active allocation authority over a replacement. Reject missing members,
replaced journal incarnations and mismatched groups. Reader fixtures use the joint release writer to move authenticated records into
retained storage. Native observations and owner/rollback policy remain synthetic;
these tests do not qualify native absence, reboot recovery or live address reuse.
Live recovery checks must use readers compatible with every retained record
format and verify both ordinary and orphan retirements when they coexist.
A reader decode failure leaves the scenario unverified; do not omit records
or treat a failed verifier as evidence of successful recovery.

Joint release requires an explicit per-member ownership check and fresh joint
observations from every surviving consumer after every member finalized. An
ordinary single-owner release policy must not authorize another owner. Admit all
members, check each owner at both release boundaries, and move the exact consumed
records in one inventory compare-and-swap. Reread the shared source once per
boundary instead of rebuilding each member's ancestry. Reject partial prior
release and mismatched evidence. Retain every frozen journal and retired-owner
record; old owners must not reserve or republish themselves.
Exercise reservation rejection against retained live identities with current
mesh metadata and valid API credentials. Require the retired-identity rejection
and unchanged inventory data and revision, rather than accepting an
authentication failure as proof. This direct API check does not replace
restored-worker execution or packet-level address-reuse tests.
The optional `receipts.releaseObservationMaxAge` bounds fresh release observations
independently of the journal's historical receipt age. Zero retains the journal
age; an explicit value must be positive and at most two minutes. Select it from
measured observation cadence, delivery and authentication time. It does not
renew observation timestamps, change consumed finalization evidence, extend
native execution deadlines or relax incarnation and source checks. Test expiry
at both authentication and the release write boundary, and reject consumed
receipt replay even with the longer window. Real-token fixtures use synthetic
observations and agent status; live convergence remains a separate gate.
Rollback to a controller that does not recognize this option must restore its
supported receipt policy as well as its image. Retained historical evidence
continues to use the original journal age.
Release currently requires every member to have finalized the same explicit
joint member set. Later overlapping groups with different consumed member sets
are rejected; they must not silently rewrite historical evidence. Qualify that
recovery separately before allowing overlapping retirement campaigns.

Real-token release fixtures must exercise missing finalization and policy,
incomplete, stale or replayed observations, invalid tokens, second-owner denial,
boot replacement, expiry/cancellation at the write boundary, source mutation,
concurrent reservation conflicts and lost responses. Failed admission must leave
all members reserved. A lost response must permit historical readback of the
whole committed group and reject a retry through the old collection. Assert one
inventory write for success and preserve unrelated concurrent reservations on
conflict. Live controller integration and compatible recovery/rollback remain
separate activation gates.

Finalized agent execution verifies historical finalization against its freshly
admitted retirement set instead of reconstructing the same ancestry again.
Initial and post-native admission still read and validate the full history;
historical verification rereads every live reserved journal, inventory and mesh,
and the agent separately rechecks retained released journals. Real-API tests
cover the live-read budget, changed scope and journal incarnation, cancellation,
and inventory, mesh and journal mutation during verification. The journal race
is injected before the first authority read, independent of map traversal order;
sequential rechecks do not provide an atomic cluster snapshot. These tests use
synthetic finalization and native callbacks and do not establish live latency,
fresh multi-consumer quorum, release, reboot or rollback qualification.

Within one locked retirement cohort, reuse the existing released-history
checkpoint admission across member checkpoints and native backends. Each write
must still match the exact inventory and local identity binding and recheck live
released sources. Reject member source changes before sharing that admission.
Admit the full reserved set once before cleanup and again afterward; select each
member's withdrawal from the corresponding admitted set, then independently
recheck its live sources and retained-consumer identity. Do not carry this
admission into another reconciliation or skip post-native history validation.
Real-API tests bound repeated journal reads and exercise invalid bindings,
missing records, cancellation and source replacement; native callbacks in this
fixture only checkpoint intent. These tests do not establish live retirement
latency, network continuity or freshness convergence.

Receipt binding within an admitted retirement set must reuse the cohort's
validated histories and produce the same digest and surviving consumers as the
standalone constructor. Require one live read per reserved journal and inventory
when constructing a collection. Missing siblings, changed source revisions,
withdrawal changes, cancellation and denied reads must fail closed. Authentication
still rechecks current agent identities and freshness; joint timestamps remain
bound to the earliest native observation. Real-API read-budget tests use synthetic
native observations and do not establish loaded execution time or timely quorum.

Measure joint retirement under the declared workload and management observation
load. Its execution deadline includes admission, checkpoints, every member's
backend and final revalidation; it is distinct from receipt freshness, which
starts at the earliest native observation. Backend duration includes live source
checks and must not be reported as kernel mutation time. Report claim removal,
provider termination, completion finalization and reservation release separately.
Cleanup after the original deadline does not pass that deadline, and completion
after observers stop does not prove convergence under the original load. Before
cost-control termination, inspect the recorded instance identities again:
already terminated instances need no further mutation. Preserve failed and
post-deadline observations separately.

Declare the AWS lifecycle observation budget before starting an operation.
`awsremove --timeout` bounds the complete command and its shared cleanup wait;
provider polling cannot renew that deadline. The default command budget is
20 minutes. The shared remover retains its ten-minute default for other callers.
A longer test budget does not extend retirement evidence freshness or turn a
post-deadline completion into a pass. Timeout tests use simulated provider state
and do not predict EC2 termination latency.

Separate retirement authorization from provider deletion progress. Before the
claim's durable removal commit, expired or incomplete observations must preserve
the reservation and prevent a Machine deletion request. After the commit,
observation expiry must not restart retirement collection. While CAPI holds a
Machine in deletion, retain the provider object and claim finalizer regardless
of Ready condition values. After the Machine disappears, wait for any remaining
provider object's finalizer before completing claim teardown. Repeat these
transitions across fresh reconciler instances and duplicate completion events.
Fake-client state tests and real-API deletion tests establish this ordering only;
they do not establish that CAPA status matches EC2 or bound AWS latency. Live AWS
qualification must correlate the captured Machine/AWSMachine identity and
conditions with direct EC2 state reads for that same instance, retaining the
original operation deadline and reporting the phase that remains pending.

Historical admission may reject a snapshot when the mesh or inventory changes
during its API reads. Discard that snapshot; a later successful read does not
validate the earlier observation. Exercise fresh admission after an unrelated
source revision and require identical retained journals and consumed evidence,
unchanged storage, and continued rejection of the old snapshot. This real-API
consistency test does not establish a live verifier's convergence deadline.
Report compute termination, finalization/release and historical verification
separately so a verifier failure cannot erase evidence of the earlier stages
or be silently treated as a successful end-to-end removal.
Revision changes on the same live source return a Kubernetes conflict. A
verifier may retry fresh shared-history admission for that classified conflict
within the original operation deadline. Identity replacement, deletion,
incomplete metadata and invalid consumed evidence must fail rather than enter
that retry path. Check both sources before classifying a conflict so a revision
change on one cannot mask replacement of the other. Use the shared historical
reader for qualification; individual-versus-shared read benchmarks are separate
measurements and must not add repeated admission passes to the cleanup gate.

Retirement freshness is recorded in each immutable journal. The configured
receipt age is an admission ceiling: increasing it admits older journals with
their original age, while decreasing it below a journal's recorded age rejects
that journal. Test expired evidence that falls inside an increased ceiling but
outside the journal's bound; both receipt collection and claim removal must
reject it without changing storage or requesting Machine deletion. These
synthetic-timestamp tests do not establish live quorum convergence. Changing
configuration does not repair an existing short-lived journal. Rollback to a
controller without this policy selection requires restoring its matching
configuration; mixed receipt-age journals are not qualified for that rollback.

An expired completion must keep standalone claim deletion pending on the
existing five-second retry cadence, without accepting the expired evidence or
accumulating reconciliation error backoff. Future or missing observation times,
changed identities and malformed authority remain errors. Exercise repeated
expiration against real API storage and require unchanged claim, Machine and
inventory revisions, then verify that fresh authenticated evidence permits the
normal deletion path. This tests retry selection and fencing, not workqueue
scheduling under load or live provider termination.

Execution rechecks may avoid replaying a previously validated immutable
snapshot only after a fresh GET returns an exactly matching journal object.
Compare content and metadata, not resource version alone. Changed objects must
still pass full decoding and execution-continuation checks; only appended
observations may preserve authority. Missing objects, access denial,
cancellation, replacement and scope changes remain failures. This optimization
does not cache API reads, extend deadlines, change receipt age or replace
initial and post-native admission. Measure loaded retirement separately from
read-only history benchmarks.

A fresh cloud Node may reuse a fully released participant's name without
inheriting its ownership. Dispatch must distinguish the new Node UID, key and
adoption identity while preserving saved local ownership fences. Active
reservations still reserve their participant names. Exercise real Node
recreation and require ordinary dispatch for a fresh identity; reused keys,
retired adoption names and saved fences must remain blocked. This admission
test does not qualify native forwarding, same-adoption-name reuse, replay of an
old worker disk or provider address reuse; verify those scenarios separately.
Repeat name reuse at released-history checkpoint admission, after the worker
has acquired its own active journal. Cover both a retired owner and a captured
consumer with a new Node UID and key; require retained history without inherited
local ownership. Old UIDs, retained keys and saved inventory fences must still
fail. Real-API tests use synthetic historical finalization and release; they do
not establish live serving-round completion or joint removal. A passing ordinary
dispatch or packet matrix alone does not qualify these later admission paths.
Normal provisioning must reach a completed serving journal without transport
rollback within its original bootstrap deadline, in addition to Node readiness
and binary identity checks. If this gate fails, keep the failed bootstrap verdict
and classify subsequent deletion as cleanup of an incomplete handover. Measure
the entire serial reconciliation cycle against receipt freshness, including
completed-worker maintenance and neighbor reconciliation between submissions.
An immutable snapshot may retain its validated phase view to avoid repeated
history replay within that snapshot. Each API load and checkpoint restore must
still validate the complete persisted history, and returned views must not expose
mutable snapshot state. Unit tests and synthetic replay benchmarks establish
these boundaries, not live convergence or the freshness of native observations.
Fresh-worker dispatch must use the shared retained-history reader and its live
recheck, while preserving retired UID, key, adoption and durable-file fences.
The real-API dispatch test bounds redundant retained-journal reads; it does not
establish bootstrap latency under a growing live history.

Unpublished site observers with no local active receiver can advance released
inventory checkpoints without ordinary-neighbor routing authority. Admission
validates every active journal and its ancestry, rejects local active or pending
retirement obligations, authenticates released history and the saved withdrawal
fences, and rechecks node boot, agent incarnation, journal and mesh authority
before the durable write. Unknown local journals, pending neighbor cleanup and
retained ordinary-neighbor peers prevent this observer-only path from succeeding.
No native callback, receipt or journal deletion is permitted by that operation.

The real-API observer scenario retains an unrelated active receiver and uses
synthetic finalized/released evidence. It exercises checkpoint advancement,
previous saved boot identity, missing or changed fences, unexpected local
journals, retained neighbor ownership, replaced agent identity, cancellation,
changed active-journal incarnation and inventory/mesh mutation during admission.
Failed admission must preserve checkpoint bytes. This proves local checkpoint
handling and callback suppression, not actual reboot, live API partition recovery,
kernel absence, stale-worker execution or address reuse.

New cloud workers unrelated to retained release history must still reach ordinary
adoption. Dispatch authenticates the released journals with the existing history
reader and checks active and retired node, key and adoption identities, including
captured consumers. Local phase files still prohibit ordinary reconciliation;
failed API reads never authorize bootstrap fallback. Active withdrawal, historical
identity overlap and name reuse remain on the phase-aware path.

The real-API dispatch scenario uses one unrelated active journal and synthetic
released evidence. It checks fresh-worker selection, active-key reuse, retired
adoption identity, retained consumer identity, saved local fences, missing nodes
and cancellation. It does not prove native adoption or packet continuity. Require
a fresh provisioned worker to acknowledge its peer list and pass the traffic
matrix with retained history present; separately qualify reboot and address reuse.

Offline cloud boot admission retains a completed legacy-cloud import marker when
its inventory, mesh, node, public key and committed base hash match the admitted
local journal. The marker must be a private regular file and accompany completed
neighbor bookkeeping. It supplies no forwarding or deletion authority: boot
recovery keeps it byte-for-byte in its source rechecks and never imports its old
peers again. Unknown markers and pending neighbor cleanup still reject recovery.
The regression scenario uses a synthetic terminal journal and previous-boot
observation. It checks valid and absent markers, identity/base mismatches, missing
or pending neighbor state, public files, symlinks, unknown markers, trailing JSON
and a marker changed during the native callback. Rejected initial admission must
not invoke native recovery; the mutation case must fail the source recheck. This
establishes admission behavior only, not VM reboot timing, kernel reconstruction,
live API recovery or safety after reuse of a retired worker's address.

Automatic Linux tunnel sizing uses the same 1420-byte ceiling as the Windows
backend and reduces that budget for a smaller known egress interface. A jumbo
underlay does not authorize a jumbo tunnel budget; explicit configured MTUs
remain explicit overrides. IPv4 default-route discovery accepts both an omitted
destination and an explicit `0.0.0.0/0` destination. The native ceiling scenario runs in isolated guest
mount and network namespaces and checks the initial WireGuard device MTU for
9001-, 1500-, 1450- and 1360-byte dummy underlays, repeated reconciliation,
explicit overrides, and unchanged underlying links. It does not qualify an
actual AWS path, IPv6 route selection, CNI configuration or workload delivery.

A workload's initial packet budget must also fit the tunnel and any additional
CNI encapsulation. A smaller tunnel device alone can still reject the first
oversized DF reply from a pod with a larger interface MTU. A later fragmented
reply after an ICMP MTU update does not turn that initial loss into a pass.
Configure the owning CNI before workload creation and verify recreated pods;
existing pods can retain their old MTU. Record request and response sizes
separately, including application command prefixes and IP/UDP headers.
Read the interface MTU inside every existing probe pod before a large-packet
or reboot baseline; the rendered CNI configuration is not a substitute.
After deliberately recreating probes, retain the earlier failed observations
and bind subsequent tests to the replacement pod and container identities.

Derive the pod MTU from the complete forwarding path. For example, a 1420-byte
tunnel budget with 50 bytes of additional CNI encapsulation leaves a 1370-byte
pod budget. Verify the actual encapsulation and address families before using
that value. Apply it through the CNI's owning configuration on every controller
that can reconcile the network, then verify installed configuration and fresh
pod interfaces. Keep physical interface MTUs unchanged. A successful source
configuration update alone does not establish workload convergence.

Apply the same path budget to AWS VPC CNI pods. Its `POD_MTU` is independent of
Calico's setting and of `AWS_VPC_ENI_MTU`; changing the site CNI does not change
AWS pod interfaces. For a verified 1370-byte pod budget, render the AWS CNI with
`aws/vpc_cni.py --pod-mtu 1370` and recreate measured AWS pods before testing.
Leave the physical ENI MTU unchanged and retain the original failed sweep.

Ordinary-neighbor reconciliation must preserve a recently authenticated
WireGuard endpoint when NAT has changed the published address or port. Test
both ordinary reconciliation and recovery with a stale configured endpoint,
and verify that traffic and the learned endpoint survive. An absent, future,
or stale handshake must still permit the configured fallback. This check does
not qualify recovery after a NAT mapping expires or a peer is offline; those
cases require separate packet observations. Endpoint preservation must never
retain withdrawn keys or source permissions.
