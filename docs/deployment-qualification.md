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
- The product keeps every site network DaemonSet off AWS VPC CNI nodes
  (`siteCNI.exclusion`); the site is built without any placement patch.
- `awsrow -pod-family IPv4|IPv6` measures separate single-family campaigns
  for nodes the product registered on a native network. IPv6-only probes
  include TCP, Service, DNS, large transfers and UDP; missing requested
  families fail instead of being skipped.
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

## BGP migration observations

Stage replacement peer configuration before withdrawing the existing peering
source. During overlap, inspect established adjacency to every expected peer in
both address families. Calico can render separate `Node_` and `Mesh_` protocol
entries for the same peer; an idle duplicate does not by itself prove that
adjacency is lost. Conversely, counting established sessions without checking
their peer identities can conceal a missing route to another node.
Overlap alone does not establish uninterrupted traffic: withdrawing an active
peering source can still lose packets while its replacement takes over.

Require the final protocol inventory to match the intended configuration, with
all expected peers established. Record bounded convergence separately from
continuous workload and management observations: waiting for BGP to converge
must not erase packet failures, API failures, or sampling gaps. Retain the
original configuration identities and unrelated settings through restoration.

A site-only rehearsal does not establish compatibility with external routers,
cloud-node exclusion, or the target's Calico build. Exercise those profiles
before deployment. Restoration performed by the test process also does not
establish autonomous rollback after that process or the API becomes unavailable.
BGP observations do not qualify a pod-MTU change; that requires its own CNI
configuration ownership and management-availability evidence.

## AWS observer configuration

The controller observes the AWS VPC CNI's allocations in the scope of each
Machine's AWSCluster: its region, VPC and subnets, the subnets' ranges from
EC2, the account from STS, and CAPA's ownership tag on the instance's primary
interface. There is no scope to configure; `platforms.aws.vpcCNI.family`
chooses IPv4 or IPv6 pods.

Its AWS calls use the SDK's standard credential chain. Name a Secret in
`aws.credentials.secretName` holding either `AWS_*` environment keys
(`mount: env`) or the files `config` and `credentials` (`mount: files`). A
profile with `role_arn` and `source_profile` has the SDK assume that role and
refresh it before expiry, so nothing outside the controller renews credentials.
Grant the identity [controller.json](iam/controller.json):
`ec2:DescribeNetworkInterfaces` and `ec2:DescribeSubnets` in the region. The
observer does not allocate addresses or change AWS routes. CAPA provisioning
credentials and AWS-node IPAM credentials remain separate.

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

Native AWS nodes must not retain `projectcalico.org/IPv4Address` or
`projectcalico.org/IPv6Address` annotations. The controller rejects these
conflicting BGP addresses before publishing allocations, including conflicts
introduced during an EC2 read. It does not delete another CNI's configuration.
Resolve the ownership conflict before retrying; a successful retry alone does
not prove that Calico stopped running on the worker.

To qualify native AWS workers with an existing full mesh, observe the site's
BGP peer identities before registration, while workers run, and after removal,
in both address families. Require unchanged site adjacency and no cloud peers,
alongside the CNI ownership and continuous traffic checks above. A synthetic
Node with InternalIP addresses can isolate peer-discovery behavior; adding BGP
annotations provides a negative control that the observation detects unwanted
peers. This diagnostic does not provision a worker or qualify EC2, external
routers, another Calico build, or migration availability. It cannot replace the
live AWS campaign or justify relaxing the deployment preconditions on its own.

The AWS workload matrix gives a complete pass its ten-minute matrix window,
bounded by the row's overall deadline. SSM delivery and observation count
against that budget; a ready Machine does not prove timely command delivery.
Retain every unsuccessful pass even if a later pass succeeds. For an observation
timeout, inspect the original SSM command ID to distinguish delayed delivery,
guest execution failure and an unobserved result. Later command success does
not convert the failed campaign into a pass. This matrix checks convergence;
separate continuous observers establish existing-site availability.

For a separate native IPv6 campaign, use `aws/bootstrap.py --ipv6` when
creating its infrastructure and `aws/native_access.py --family ipv6` to attach
the IPv6 VPC CNI node policy. The recorded VPC and subnet IPv6 ranges feed
the existing CAPA claim's IPv6 configuration and primary-address assignment.
Set `platforms.aws.vpcCNI.family=ipv6` and use `awsrow -pod-family IPv6` for
that campaign. Changing the family of an existing native CNI campaign is rejected.

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

Every failure test description must state the injected fault, affected resources
and identities, observation lanes, recovery deadline, allowed interruption, and
limitations of the evidence. Record these before injection. Retain every failed
request and incomplete attempt; an expected interruption remains a measured
interruption. Report maintenance acceptance separately from release eligibility.
Do not remove failed samples or relax a gate to represent an exception.

Use these fields in the private test artifact before starting a failure test:

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
Keep run timestamps, actual identities, measurements and failure narratives in
private evidence and commit messages, rather than turning this document into a
run log.

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
identities fail closed.

## MTU sizing and endpoint preservation

Automatic Linux tunnel sizing uses the same 1420-byte ceiling as the Windows
backend and reduces that budget for a smaller known egress interface. A jumbo
underlay does not authorize a jumbo tunnel budget; explicit configured MTUs
remain explicit overrides. IPv4 default-route discovery accepts both an omitted
destination and an explicit `0.0.0.0/0` destination.

A workload's initial packet budget must also fit the tunnel and any additional
CNI encapsulation. A smaller tunnel device alone can still reject the first
oversized DF reply from a pod with a larger interface MTU. Derive the pod MTU
from the complete forwarding path (for example, a 1420-byte tunnel budget with
50 bytes of additional CNI encapsulation leaves a 1370-byte pod budget). Apply
it through the CNI's owning configuration on every controller that can reconcile
the network, then verify installed configuration and fresh pod interfaces.
`platform-tuple check`, `endpoint-controller check`, and `acceptance preconditions --tuple=...`
inspect both `k0s.yaml` (`spec.network.calico.mtu`) and live cluster state
(`kube-system/calico-node` `CNI_NETWORK_CONFIG` `mtu`, `FELIX_*MTU`,
`calico-windows-config-actual` `CNI_MTU` / `veth_mtu`, `IPAMConfig/default`
`strictAffinity`, site CNI / wildcard-toleration DaemonSet `nodeAffinity`
exclusion, and `hostPort` `Recreate` rollout strategies). Running `platform-tuple fix`
(or the live `endpoint-controller` reconciler) automatically aligns those
settings, clamps existing live host `cali*` veth and container `eth0` MTUs
in-place without pod restarts, and propagates DaemonSet exclusions and rollout
patches to owning Flux `HelmRelease`, Flux `Kustomization`, and Argo CD
`Application` resources.

Apply the same path budget to AWS VPC CNI pods. Its `POD_MTU` is independent of
Calico's setting and of `AWS_VPC_ENI_MTU`; changing the site CNI does not change
AWS pod interfaces. The product sets it to the site's Calico pod MTU when that
is below the tunnel's 1420 bytes, so a verified 1370-byte site budget reaches
AWS pods too; recreate measured AWS pods before testing.

Ordinary-neighbor reconciliation must preserve a recently authenticated
WireGuard endpoint when NAT has changed the published address or port. An
absent, future, or stale handshake still permits the configured fallback, while
endpoint preservation must never retain withdrawn keys or source permissions.
