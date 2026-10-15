# Deployment qualification

A release requires fresh measurements of one immutable candidate. Historical
results do not qualify changed binaries or a different cloud CNI.

## Safety controls

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
review the raw artifacts for the stated scope. It is a standalone read-only
gate, not yet a deployment controller or CI enforcement hook.

Run artifact verification on Linux. The verifier streams regular files beneath
the evidence JSON directory and rejects paths or symlinks that escape it.
Symlinks resolving within that directory are allowed. Keep the evidence
directory immutable during verification and deployment; a matching hash does
not authenticate the producer or prove that an artifact measures its claimed
scenario.

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
