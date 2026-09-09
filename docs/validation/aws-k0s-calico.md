# k0s and bundled Calico on AWS

The real CAPA integration passes **1,184 network checks across nine gates**,
including two worker replacements and four tunnel-endpoint placements.
[Structured evidence](aws-k0s-calico-results.json) records the measured matrices'
hashes, instance and Node identities, observed images, coverage and cleanup.
[Setup and IAM policies](../aws.md) describe how to reproduce the run.

## Tested configuration

| Component | Configuration |
| --- | --- |
| Kubernetes distribution | k0s v1.34.1+k0s.0 |
| Network | Bundled Calico v3.29.6-0, default VXLAN |
| Cluster API | v1.11.1 |
| AWS infrastructure provider | CAPA v2.12.1 |
| Site | Five Ubuntu 22.04 QEMU/KVM guests; three control planes and two workers |
| AWS workers | Two Ubuntu 22.04 t3.large EC2 instances in us-west-2a |
| Physical networking | One Ethernet NIC per site guest; one ENI and one guest physical NIC per EC2 worker |
| Management | Serial QGA for site VMs; SSM over the existing ENI for EC2 |
| Bootstrap | CAPA private Secrets Manager transport; prepared cloud-init AMI |
| Endpoint retirement | Default three-minute retention plus remote acknowledgements |

The AMI includes AWS CLI, compatible cloud-init include handling and direct file
logging for CAPA's cloud-init restart. Capture syncs writes and waits for the
builder to stop. Worker instances join through the product's rendered native
k0s configuration; the harness does not repair bootstrap or synthesize CAPI
Machine-to-Node association.

The harness imports its measurement image through private S3 and SSM. Large
transfers execute inside probe pods and return a byte count through SSM, so its
inline-output limit cannot truncate the measured 1 MiB transfer into a false pass.

## Matrix and lifecycle assertions

Each full row checks every ordered pair by pod address, Service address and
1 MiB transfer, plus DNS and external reachability from every node.

| Gate | Passing checks |
| --- | ---: |
| Both initial AWS workers | 140 |
| First worker removed; survivor remains | 102 |
| First claim re-added | 140 |
| Second worker removed; survivor remains | 102 |
| Second claim re-added | 140 |
| Endpoints on one control-plane node | 140 |
| Endpoints on one worker | 140 |
| Endpoints on two workers | 140 |
| Endpoints on all five site nodes | 140 |
| **Total** | **1,184** |

Each removal waits for the claim, Machine, referenced AWSMachine, Node,
bootstrap/adoption Secrets and peer fields to disappear, and separately confirms
EC2 termination. Each replacement must have a new instance ID and Node UID.
The other worker must keep its identity through removal and readdition.

Each placement changes Helm values and verifies both published endpoint
membership and actual WireGuard devices before measuring traffic. Retained
devices from the preceding placement cannot satisfy the gate.

## Regression tests and coverage

The observed AWS behavior supplies these regression cases:

- CAPA reports an accepted stop before EC2 reaches stopped; AMI capture must
  wait for the latter to avoid an incomplete snapshot.
- An AWSMachine can remain while EC2 is shutting down; removal must inspect its
  actual infrastructure reference rather than a local-provider resource.
- Surviving peer data prevents removal from reporting success.
- IMDS-derived provider IDs must match the real CAPI and k0s Node identity.
- Sanitized Calico IPPools and BlockAffinities confirm VXLAN detection without
  native pod-prefix advertisements for AWS workers.
- Truncated or failed transfers and duplicated matrix paths must not pass.

The [measured `make coverage` run](go-short-coverage-results.json) passes with
**61.6% controller** and **28.3% harness** statement coverage. This uses Go short
tests and disables the controller's native network-namespace tests; it does not
include Python observers or establish native integration coverage. The real AWS
runs additionally instrument the harness;
the structured evidence contains the per-package runtime percentages. These are
distinct measurements and are not added together. Kubernetes, Calico and CAPA
binaries are not instrumented.

The whole-harness percentage includes distribution builders and failure modes
not exercised by this run. Low coverage alone does not establish dead code.
[Code ownership and supersession](code-ownership.md) identifies the historical
paths and retained unique assertions.

## Scope and cleanup

This evidence covers AWS provisioning, native k0s joining, removal, replacement,
survivor connectivity and the listed endpoint placements. It does not establish
AWS stop/start recovery, security-group partitions, physical NIC cuts, other
AWS distributions, or other cloud providers. SSM is not a network-independent
guest console.

Final workers are removed through CAPA before infrastructure cleanup. The AWS
read-back verification in the structured evidence confirms terminated instances
and absence of the test VPC, recorded AMIs and snapshots, IAM roles/profile and
asset bucket. Raw observation snapshots remain private because deployment
arguments can contain expiring download URLs.
