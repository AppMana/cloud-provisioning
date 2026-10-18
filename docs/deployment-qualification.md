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
before relying on this mechanism for the 24-hour soak.

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
   60 seconds, autonomous rollback within five minutes, and a 24-hour soak.
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
| Control-plane VM capacity maintenance | Change one VM at a time through the existing VM lifecycle. Preserve and verify its writable disk, machine identity, Node UID and journal ownership before accepting replacement. Require target recovery within 60 seconds and zero failures on unaffected workload and management paths. | A Ready Node with a new boot ID does not establish recovered controller membership, API service or target workloads. Verify the controller lease has renewed after boot, direct API readiness and usable target workloads; observe subsequent membership reconciliation on surviving controllers. A successful memory change does not waive failed continuity checks. If preparation fails after stopping the VM, retain observations until the original VM recovers; recovery outside that window is incomplete evidence. |
| Konnectivity discovery preparation and lease-mode migration | First deploy continuous-discovery agents through the elected runtime owner on every controller; verify effective arguments before enabling lease-mode servers. Exercise leadership changes, management streams through every surviving API endpoint, and resource rollback. Keep workloads and unaffected APIs available. | Preparation can replace agent pods and count-driven servers on other nodes. A local endpoint or worker maintenance allowance does not authorize cross-node exec/log interruption. Record any separately approved bound before injection. Verify embedded runtime hashes: a supervisor build can also replace the API server, kubelet or containerd. A restored executable does not establish restored DaemonSets, leases, RBAC, static-pod identity or stream availability. |
| Kubelet or container-runtime maintenance | Exec/log access on the maintained worker may be interrupted for at most 60 seconds; API health, other nodes and existing workload traffic remain uninterrupted. | A running workload does not establish exec/log continuity. This exception does not apply to agent upgrades or ordinary reconciliation. |
| Agent upgrade | Observe a distinct immutable predecessor becoming the candidate on every intended node with zero management and workload failures. | Same-version restarts, desired image references and rollout status alone do not prove an upgrade or its continuity. |
| Host provisioning service upgrade | Verify the running predecessor executable, activate the candidate sequentially, and verify the new process and executable identity while retaining zero management and workload failures. Declare abort and rollback deadlines before activation and verify recovery before advancing. | An installed candidate file or successful agent rollout does not prove that the host service runs the candidate. Restoring a binary through console access does not qualify autonomous network rollback. Process identity alone does not establish forwarding, durable ownership recovery or compatibility with existing journals. |
| Worker reboot and idle boot recovery | Measure from reboot injection through usable networking and workload recovery; require recovery within 60 seconds and preserve unaffected site/API availability. Observe exec/log loss on the rebooted worker explicitly. | A network-namespace restoration test does not establish VM boot time, workload recovery or the reboot gate. Fresh traffic can hide idle recovery delays. Missing or stale endpoint hints and changed NAT mappings require separate cases; a hint alone cannot authorize routes without authenticated recovery. Same-boot host-service admission must leave native state to the live agent even when retirement is recorded; this no-op does not qualify survivor reconciliation or changed-boot recovery during retirement. Offline cloud recovery may use a withdrawal-bearing inventory only when its digest matches successful native readback for the surviving local journal. The recorded withdrawal supplies exclusions only; it cannot restore the retiring worker or release ownership. Completed ordinary-neighbor checkpoints must retain matching identity and private-file ownership, contain no pending retirement or candidate work, and remain unchanged during recovery; their peers are not replayed. Separate service shutdown, the unobserved shutdown-to-kernel interval, host network readiness, CNI discovery and workload recovery. Record cross-boot clock uncertainty and missing serial output; do not assign an unobserved interval to a component. A successful steady-state request with the CNI credentials does not prove API reachability during boot. |
| Endpoint failover | Inject loss of the selected endpoint; recover cloud traffic within 60 seconds with zero management or existing-site failures. | One endpoint placement or address family does not qualify another. Report changed addresses and NAT mappings separately. |
| API loss | Block the declared control-path access; retain established forwarding and recover affected cloud traffic within 60 seconds. Keep independent API health and site observations running. | Distinguish deliberately blocked client requests from API-server failures. Losing controller access is not evidence for losing every API endpoint or the management observer. |
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
| Claim ownership changes before drain | Create the Machine through the claim controller, then replace its claim owner reference with a Cluster owner reference while retaining the creation-time claim namespace/name and UID annotations. Verify that the same claim can still drain and retire it. Reject partial bindings, a different claim UID, conflicting claim owners, and same-name objects in another namespace. | The durable binding is association evidence, not cleanup authorization. API tests simulate owner-reference changes; they do not establish live CAPI behavior or protect against a principal authorized to rewrite all ownership metadata. Existing Machines without this binding require an exact claim owner reference; missing historical ownership is not reconstructed from names or claim status. Native retirement, compute deletion and continuity remain separate requirements. |
| Mesh metadata changes during captured retirement | Capture withdrawal, change only Secret metadata before reservation and again before publication, then resume through authenticated completion. Bind new captures to the complete Secret data hash and preserve the original consumer set. Continue using the live resource version for publication CAS. Reject changed data, invalid hashes and metadata revision changes on legacy captures without a hash. | This permits metadata changes only: even acknowledgement changes inside Secret data still require separate handling. It does not rebase network changes or extend captured membership. Older readers may reject the additional intent field; qualify mixed-version retirement and rollback rather than dropping persisted fields. API tests do not establish live native continuity. |
| Standalone claim deletion with receiver reservations | Delete a claim through its finalizer. Persist Machine, Node, mesh, inventory and gateway identities before cordoning; retain gateway requests and every captured survivor until retirement completes. Require the shared authenticated receiver-retirement sequence before committing compute removal. Reject changed identities, missing journals, malformed removal records and conflicting drain owners. Verify group-owned claims also require the matching parent's committed action. | Real-API standalone and group tests exercise persistence, blocked deletion, authenticated completion, stale-boot rejection, refreshed observations and the child-to-CAPI deletion request. Transport history, agent readiness and native cleanup observations are synthetic; no provider controller runs in these tests. They do not establish a live deletion campaign or native reboot recovery. PDB refusal and unavailable required identities remain blocking; no forced drain or elapsed-time bypass is authorized. Capture does not automatically rebase changed consumer membership or mesh revisions. Orphan recovery and reservation release remain separate requirements. |
| Orphan peer with receiver reservations | Remove compute independently of normal retirement, then reconcile orphan discovery. Preserve peer fields, adoption state and address reservations for every published journal and unfinished preparation. Reject unavailable or malformed reservation discovery before ordinary cleanup, and distinguish same-named Machines in different namespaces. | This exclusion prevents ordinary orphan pruning from substituting Machine absence for retirement. Orphan-intent capture tests require a frozen terminal journal, its exact reservation, an already absent peer publication, and successful authoritative NotFound reads for the old Machine, Node and adoption Secret. They retain published survivors and reject replacement objects, read errors, missing participants and changed captured mesh data. Capture tests use synthetic API objects and terminal history. Controller tests use real journal and inventory storage with synthetic owner absence and surviving Nodes: freeze completion, persist already-withdrawn intent on a later pass, retain reservations and reuse the recorded consumer set on retries. Unpublished preparations fence this path. These tests do not establish live owner observation, authenticated retirement completion, reservation release or native cleanup. Recovery currently requires the old peer publication to be absent; a still-published orphan needs a separate removal protocol. Changes to captured mesh data remain blocking rather than automatically rebasing consumers. API discovery and the later Secret write are separate operations, not a cross-object transaction. Require authenticated retirement and explicit release evidence before claiming eventual cleanup or safe address reuse. |
| Last-worker deletion | Delete through the provisioning lifecycle and verify final route withdrawal and resource release without existing-site regressions. Exercise deletion after a completed receive transition and while a successor has prepared devices but has not replaced the serving journal. Retire both serving and pending references before releasing their ownership reservations. | Empty Kubernetes listings or terminated instances do not prove network cleanup; inspect cloud resources, routes, durable checkpoints, WireGuard source permissions and pending devices on every participating node. A terminal journal is not a retirement acknowledgement, and its reserved peer can still block address reuse after its claim disappears. A comparison against only the serving graph can miss devices created during successor preparation. Record cloud resource cleanup, native retirement and inventory release as separate outcomes. Do not bypass finalizers, delete journals or manually remove peer state to make the test finish. |
| Overlapping historical ordinary-peer retirement intents | Record an interrupted replacement with source ranges also present in an older committed peer. Withdraw both through normal publication; retire exact installed owners, preserve absent attempts until route cleanup completes, and retry idempotently. Inject a broader competing live prefix with both recorded and foreign identities; reject before route or permission mutation. Require zero protected workload and management failures and recovery within 60 seconds in the live campaign. | Historical records do not prove simultaneous kernel ownership. Validate each record against reserved journals and check actual native ownership before deleting anything. Isolated Linux tests cover both IP-family permissions and an IPv4 protected stream, not AWS lifecycle, continuous IPv6 payloads, cold handshake timing or reboot. Preserve first-attempt failures even if a subsequent run passes; a retry does not qualify continuity. |
| Legacy cloud peers across reserved-receiver activation | Begin with previously installed ordinary cloud peers, activate reserved receivers, then delete and replace ordinary workers through the normal lifecycle. Verify ownership transfer, retirement and bidirectional traffic when replacement workers reuse source ranges. Preserve journal-owned peers and unrelated native state. | A new ordinary-neighbor checkpoint does not establish ownership of older peers already installed on the device. Refusing a source-authorization collision protects the old peer but does not qualify replacement-worker connectivity. Test workers joining both an existing cloud node and a fresh cloud node; Ready Nodes and site-to-cloud traffic alone do not cover cloud-to-cloud forwarding. A legacy cache alone must not authorize deletion without validating its ownership and native state. |
| Receive-transition interruption, expiry and rollback | Inject the declared interruption at the named transition phase; verify authenticated forwarding, durable recovery and preservation of journal-owned and foreign state. Observe renewal, expiry, rollback and inventory release as separate operations. | A terminal journal or an isolated route test does not prove live forwarding or recovery after a full reboot. Whole-peer retirement does not cover source narrowing on a retained peer, independently prepared receivers or the next publication generation. Name every unexercised phase and operation in the individual description. |
| Allocation change after a completed receive transition | After each terminal outcome (forward completion, abort and rollback), allocate a new workload range and verify its publication, authenticated permissions, routes and bidirectional traffic through the normal ownership lifecycle. Preserve existing traffic and foreign state while advancing ownership; exercise interruption and restart at the generation boundary. | A completed journal can retain an older source set even when the current publication contains the new allocation. Ready nodes and working exec/log requests do not prove workload connectivity. Controller rejection of source drift only exposes the mismatch; it does not activate a successor or restore workload traffic. Storage tests must verify that rejection preserves the exact serving journal and plan. Ordinary-peer growth tests do not cover journal-owned peers. Deleting a journal, manually widening permissions or excluding the affected worker cannot qualify generation advancement. Test source growth, withdrawal and address reuse separately; success in one direction or terminal outcome does not establish the others. |
| Survivor route projection during worker withdrawal | Validate the complete reserved journal set before excluding a published withdrawal from serving intent. Require its exact frozen terminal journal incarnation; retain its destinations against reassignment. Preserve survivors in forward, abort and rollback histories. Captured but unpublished withdrawals remain serving. | Planner tests use synthetic histories and compare both IPv4 and IPv6 route intent; real-API publication tests distinguish prepared and published withdrawal. Real-API agent tests additionally exercise survivor phase execution and completed maintenance after the retiring Node and adoption Secret disappear, preserve both checkpoints, and reject concurrent withdrawal changes before issuing receipts. Those real-API native callbacks are simulated. A separate isolated Linux guest test combines synthetic API/history, actual cloud-neighbor cleanup, authenticated loopback receiver transport and completed-state maintenance after deleting each survivor route in turn. It requires recovery within ten seconds per injection, checks IPv4 and IPv6 route readback, retains an unrelated root peer, and verifies the withdrawn peer stays absent. The fixture also rebinds actual native observations and endpoint hints to a simulated previous boot, deletes the survivor receiver, and exercises offline recovery with all emitted bookkeeping retained. This does not measure payload continuity, actual VM reboot time, real-API-to-kernel integration or an AWS lifecycle. Withdrawn identities remain explicit in the serving projection: stale site advertisement caches must reject them, and cloud policy restoration must omit their remote destinations. These planner checks do not prove BGP convergence or actual rule restoration. The serving projection does not authorize clearing shared tables or releasing reservations. Live qualification still requires zero interruption on surviving workload and management paths. |
| Pinned target journal with multiple reserved workers | Pin the target journal incarnation, admit all other reserved journals under their own identities, and retain those identities through rechecks and local checkpoint writes. Reject a mismatched target before native execution. Require zero interruption on existing workload and management paths in live qualification. | Real-API tests cover local site membership in both journals and cloud membership in only one, with simulated native verification. They do not establish traffic continuity, retirement coexistence, or reboot recovery. A target UID must not be reused as the expected UID of another reserved worker. |
| Missing predecessor history or lost archive read access | Exercise both a fresh agent without a checkpoint and an agent that missed multiple generations, then deny a required archive read, remove an archive or recreate it with identical bytes and a different UID. Reject incomplete ancestry before replacing the checkpoint. Preserve named read-only grants through chart upgrades; never grant agent mutation or namespace-wide listing. Restore authorized access before retrying admission. | API ancestry and RBAC tests do not establish native forwarding continuity, offline boot recovery or usable successor routes. Include nonlocal journals whose destinations participate in shared routing; successful local admission alone does not validate the entire inventory. Read-time ancestry validation does not make journal and archive reads atomic. Record the denial window and recovery deadline in the live test; no interruption allowance follows from an admission rejection. Recreated history must remain rejected even when its bytes match. A chart test without a running controller proves grant preservation only, not controller publication ordering. |
| Allocation changes while an old site path remains available | Bind previous site permissions separately from desired permissions. Verify preparation preserves the exact old peer, rollback retains its destinations, and other journals and ordinary peers cannot claim either generation's ranges. Exercise growth and withdrawal for both address families. | Planner and peer-admission tests do not prove native switching, rollback timing or final removal of obsolete receive permissions. Advertisement planning must select the committed serving destinations from either valid cached generation; planner success does not establish native route readback or BGP convergence. Transitional source unions must not be reported as completed source withdrawal. Site preparation, readiness, durable replay and retirement must agree on the predecessor and desired documents for each receiver. Cache round trips only prove retained inputs; they do not prove kernel restoration, authenticated traffic or final removal of old permissions. Existing journals without an explicit previous site document retain their original single-document semantics; they do not supply missing historical allocation evidence. |
| Successor archive access or journal publication interrupted | With prepared cloud and site receivers, fail the agent Role update or the live journal write. Require unchanged serving history and zero interruption before commit. Grant only named archive reads before replacing the journal; retain the same journal UID, inventory reservation and archive identity across retries. Resume a committed successor without rewriting it. | Controller tests use real archive/journal/inventory storage but simulated Role/Secret storage and readiness. A concurrent Role update must conflict with an older grant derivation rather than losing the newer archive grant. Missing Role access, a simulated concurrent Role update and an injected journal write failure do not establish live authorization-cache propagation, native preparation or traffic continuity. Before commit, retry additive preparation without rollback; after commit, use the configured round deadline and expiry recovery policy. Synthetic test times do not qualify an operational recovery deadline. Separate API objects cannot be committed atomically; agents must reject unavailable or mismatched ancestry and preserve serving state. |
| Successor receiver preparation interrupted before activation | Commit a distinct receiver generation before preparation. Interrupt and resume preparation while keeping the predecessor devices, keys, permissions and forwarding intact; reject bindings from another generation. Reserve separate listeners before creating the successor devices. Require zero interruption on existing workload and management lanes. | Deterministic interface, alias and key tests do not establish simultaneous native listeners, authenticated traffic or crash-safe device cleanup. Reusing stable mesh or node identity does not authorize reuse of a serving receiver. Agents must restore the committed generation rather than minting a new one after an observation timeout. Controller proposal retries and boot-identity refreshes must preserve the generation and listener reservations for unchanged desired transport; refresh readiness without changing the serving document or journal. Proposal tests with simulated Secret writes and real journal storage do not establish agent admission, native preparation or cross-object atomicity. Completed-maintenance admission tests use real API storage for both cloud and site identities; cloud readiness uses simulated native preparation. They reject inventory or proposal changes before readiness and skip absent, original or already-consumed requests. They do not establish site handshake readiness, native coexistence or traffic continuity. Controller plan-building tests add a worker destination after forward, abort and rollback completion: preserve the serving generation and its old relay destinations while admitting the new destination separately. These use simulated readiness and Secret storage with a real predecessor journal; they do not establish successor publication or activation. Preparation shares the maintenance lock and its 30-second maximum deadline (shortened by any unfinished local round); failure leaves serving forwarding selected and requires fresh admission on retry. Bind successor preparation to the exact completed journal identity and terminal history; reject changed or missing predecessors before device preparation and before readiness publication. Reject generation or listener reuse against both receiver graphs recorded by a terminal successor, including the older graph still serving after abort or rollback. Admission rejection must precede native work and leave forwarding unchanged; simulated journal tests do not establish packet continuity. Validation tests do not prove atomicity across the journal and readiness Secret, inventory admission, or safe offline replay; retain the journal reservation fence until those paths are qualified. State the recovery deadline and rollback conditions in the live test. |
| Receipt refresh misses the freshness window | Configure receipt agents with an idle polling interval no greater than one third of the receipt lifetime, capped at the ordinary 30-second interval. Retain the configured receipt-age and round-expiry checks; missing current observations must prevent advancement and follow the declared expiry recovery policy. | Generated-configuration tests cover only idle scheduling. A responsive-server admission-burst test checks the agent's bounded API client budget; it does not establish API server capacity, sustained demand, archive-depth scaling or native completion. Explicit caller rate limits remain authoritative. Native verification, API throttling, delivery latency and independent participant timing can still exceed the freshness window. Measure all participants during live transitions; configuration tests do not establish completion within a deadline or zero workload interruption. Preserve expired-round and observation-timeout failures, and verify rollback separately. |
| Receipt agent release is mutable or malformed | Reject receipt-enabled controller startup before API discovery, agent rollout or journal publication when the configured agent image lacks a full digest, the runtime image ID is invalid, or cloud agents use a host-binary entrypoint. Correct the configuration and restart with the intended immutable release. | Configuration tests do not prove that the image exists on each node, matches the admitted runtime ID, or can submit authenticated receipts. Rejection permits no transition-induced interruption; it does not roll back a previously published transition. Recover existing transitions through their recorded phase and expiry policy, retaining failed observations and checking workload and management continuity. |
| Successor graph differs from archived serving transport | Bind starting member keys, devices, permissions, destinations and relay choices to the exact terminal predecessor. Preserve the selected receiver graph through preparation, abort and rollback, including a second successor. Reject changed identities, reused listeners and nested historical graphs. Verify allocation growth and withdrawal independently. | Pure graph and simulated-storage tests do not prove API atomicity, native execution or uninterrupted forwarding. Matching member keys alone does not authenticate source permissions. Advertisement and relay dependency tests with synthetic views prove graph selection and receipt ordering only; they do not prove BGP convergence, live relay continuity or native retirement. Rollback to a predecessor receiver must retain receiver routing and use that graph's destinations and direct relay owner, rather than select the original tunnel. A decoded version-two plan is not native execution authority; live admission, history validation and host locking remain required. Isolated native phase tests cover direct cloud and site paths, forward completion, abort, rollback, idempotent retries, foreign-route retirement rejection and completed-state restoration of a deleted route without reviving retired receivers. Recovery must select the journal's serving receiver graph after abort or rollback as well as forward completion. Their synthetic views and already-prepared authenticated peers do not establish controller publication ordering, cold preparation, source exceptions, relay changes, reboot recovery or workload continuity. Both IP-family routes are checked, but the cryptographic transport is IPv4 within one namespace and no IPv6 payload stream is measured. |
| Successor receiver route selection and rollback | Admit predecessor and successor device identities independently for each destination. Before switching, verify the installed route belongs to an admitted generation with exact keys, source permissions and a current handshake. Permit the original tunnel only for destinations explicitly assigned to it in the admitted graph; a first-transition worker sharing the route table must not authorize root fallback for a successor worker. Exercise both address families with one root destination and one receiver destination, and verify that a rejected mixed graph leaves routes unchanged. Switch and roll back both address families; rejected foreign state must remain unchanged. Exercise source-specific exceptions separately: admit both receiver generations, keep their selectors bound to the allocated source address, and preserve the actually installed receiver during withdrawal staging. Source-route inspection must reject conflicting candidate identities before mutation. Require zero workload and management interruption in the live campaign. | Native route readback and loopback handshakes do not establish workload delivery, remote transport, journal publication ordering or source retirement. State whether the test carries IPv6 payloads or merely installs IPv6 routes, and whether it measures traffic during switching. Two accepted candidate graphs do not authenticate their history; the executor must independently validate their journals and participants. |
| Listener pool exhaustion or conflicting persisted reservations | Retain every generation's occupied ports. Reject exhausted pools, duplicate ownership and malformed reservation records without partial assignments or changing active forwarding. Reload committed reservations after a write conflict; release a listener only through verified retirement. | Serialization and allocator tests do not establish API atomicity, socket availability or safe listener retirement. A smaller new pool must not make predecessor ports available for reuse. Versioned reservation records must reject readers that would discard other generations. Live testing must specify the recovery deadline and confirm existing traffic remains uninterrupted during rejection. |
| Credential expiry and producer outage | Cross a real expiry boundary; verify projected credentials and actual observation requests. On renewal failure, preserve previous allocations and report observation failure. | Successful Secret publication does not prove projection or AWS authorization. A shorter run does not establish 24-hour credential continuity. |
| Cloud bootstrap and stale ownership | Create workers through provisioning and CAPI, then verify authenticated site connectivity, Node registration, native CNI readiness and usable pod traffic. For obsolete owners, use normal lifecycle deletion and verify withdrawal before accepting reused addresses. Preserve existing-site connectivity. | A running instance, Ready infrastructure object or downloaded agent is not a working Kubernetes worker. Cleanup followed by recovery does not prove the original bootstrap met its deadline. Distinguish stale ownership from a live address collision; never remove a live owner's state merely to make provisioning pass. |
| Direct cloud edge reconciliation with reserved journals | With both cloud workers owning journals, add, restore and withdraw their direct peer publication through normal reconciliation. Retain the complete inventory, local boot and agent identity checks. Require zero interruption on existing workload and management lanes; measure new-edge convergence within 60 seconds. On rejection, preserve serving transport and retry only against freshly admitted publication. | Graph tests distinguish cloud edges from site-facing receivers and reject competing owners, site permissions and receiver identities. Admission tests simulate native execution and do not establish kernel forwarding, restart continuity or the live convergence deadline. Test both traffic directions, journal phases, address families and withdrawn-owner address reuse explicitly. Cloud reconciliation does not replay the site's legacy forwarding cache; unrecorded old cloud peers are not inferred to be safe to delete. |
| Sustained operation | Observe the same candidate for at least 24 hours with continuous management and workload measurements, including credential renewal and actual requests across expiry. | Adding disconnected short runs does not establish a continuous soak. A successful soak does not substitute for fault injection, and an unobserved interval cannot be inferred healthy from later readiness. |
| Resource pressure during rollout | Retain the same management and workload availability requirements while measuring controller CPU, memory and storage pressure through activation and rollback. | Ready nodes and unchanged process IDs do not prove API availability. A short healthy baseline does not establish rollout headroom. Record actual VM resources; changing capacity requires a fresh observation and cannot turn the earlier failure into a pass. Correlation with pressure does not establish the failure's cause. |

Node image-cache additions, removals and reordering must not invalidate retirement
admission. Cached images do not identify the running agent: retain independent
Pod, container and runtime-image authentication. A source-comparison regression
does not qualify cleanup during a live image pull or garbage-collection cycle.

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
verify that its credential is never retained. Synthetic accumulation tests do
not prove that live consumers can deliver within the shared freshness window.

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
