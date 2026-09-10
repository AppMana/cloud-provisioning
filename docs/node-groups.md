# Provisioned node groups

Status: experimental API and claim reconciler; the current `ProvisionedNodeClaim`
provisions one machine. Group orchestration requires explicit enablement and
remains under development.

## Enable in an isolated harness

Install the experimental API contract, then enable the controller on the harness
release using its existing values and a newly built controller image:

```sh
kubectl apply -f controller/pkg/nodegroup/testdata/provisionednodegroupclaims.yaml
helm upgrade cloud-provisioning charts/cloud-provisioning \
  --namespace cloud-provisioning --reuse-values \
  --set experimentalNodeGroups=true \
  --set image.tag=YOUR_TESTED_BUILD_TAG
```

Confirm the chart's image repository and tag match the build available to the
VMs. The group controller shares the existing mesh leader election and watches
its release namespace. CAPI and workload Nodes must be in this cluster API;
separate management/workload cluster registration remains to be implemented.
The value adds group status and child creation/deletion permissions in the
release namespace, plus pod listing and eviction permissions for workload drain.
It defaults to `false`; the group CRD is installed separately for this experiment.
KEDA integration and complete VM qualification remain pending.

## Pooled VM provider

Create an isolated VM lab with `cmd/lab -remote-slots 3`, then run the provider
from `harness/e2e` using the same capacity:

```sh
go run ./cmd/vmprovider -work-dir /work -remote-slots 3 \
  -namespace cloud-provisioning
```

The provider requires an existing site, installed CAPI controllers, an imported
CAPI Cluster, and `/work/binaries/wg-dialer-linux-amd64`. It holds the lab process
lock and serves that binary at the existing bootstrap URL. Keep it running while
claims are provisioning or terminating.

Use a `ContainernetMachineTemplate` with an empty `spec.template.spec` for a
pool. A template with `containerName: remote1` describes a single fixed VM and
cannot supply multiple replicas. `Controller.Slots` enables allocation for
generated group child names. CAPA provisions independently from its shared
`AWSMachineTemplate`.

The VM provider applies these lifecycle rules:

- Reserve fixed bindings first, then assign free slots to generated names.
- Bind each reservation and provider ID to the infrastructure Machine UID.
- Persist reservations with ConfigMap UID and resource-version checks before
  publishing addresses or starting a guest. New reservations also verify the live
  infrastructure Machine UID after reading the pool and reject deleting owners.
- Continue reconciling reserved Machines when additional requests exceed capacity.
- Record a durable launch receipt before waiting for guest readiness. Retries
  observe the same running VM; a stopped predecessor is required for slot reuse.
- Hold an interrupted launch with an unresolved outcome for inspection.
- Record VM stop before removing the provider finalizer, then release the slot
  after the infrastructure Machine UID disappears.
- Release only the provider finalizer for a deleting infrastructure Machine that
  has neither a reservation nor a bootstrap receipt. This provider operation
  does not implement group cancellation. Before releasing the finalizer, it writes
  a cancellation fence to the pool so an allocator using an older snapshot cannot
  reserve the deleted Machine.

The offline lab also needs the remote dialer DaemonSet images imported into each
joined VM's distribution runtime before adoption can complete. The pooled command
currently requires this image-loading step separately. Workload probes require
the harness's pinned `crictl` tool inside each newly provisioned guest.

## Tested scenarios

The native group campaign uses k0s `1.36.2+k0s.0`, containerd `2.3.2`, and bundled
Calico `3.32.0-0`. Its eight KVM guests each have one physical NIC. Guest-agent
access uses virtio-serial. These results qualify the linked scenarios for this
configuration; other distributions, Windows groups, and CAPA groups require
separate runs.

| Scenario | Recorded result | Evidence |
| --- | --- | --- |
| Single-NIC topology | ✅ Eight guests; site-to-cloud reachability and remote isolation from direct site access | [Topology](validation/node-group-vm-topology-results.json) |
| Three workers to one | ✅ Two claims, CAPI and infrastructure Machines, Nodes, VMs, and reservations removed; survivor identities retained | [Scale-down](validation/node-group-vm-scale-down-results.json) |
| Disruption budget and scale-to-zero | ✅ A Ready pod blocks eviction until its budget permits it; all three workers subsequently removed | [PDB and zero](validation/node-group-vm-pdb-results.json) |
| Slot reuse | ✅ Zero to three reuses names with new Machine, provider, and Node identities | [Reuse](validation/node-group-vm-reuse-results.json) |
| Workload networking | ✅ 184 pod-IP, Service-IP, 1 MiB transfer, DNS, and external checks across eight Nodes | [Pod matrix](validation/node-group-vm-pod-matrix-results.json) |
| Workload continuity during removal | ✅ 2,880 checks across six survivors, including a controller restart during drain; survivor Node and pod UIDs retained | [Continuity](validation/node-group-vm-continuity-results.json) |
| UDP pod traffic | ✅ 600 exact responses across transit site, tunnel endpoint, and cloud worker | [UDP](validation/node-group-vm-udp-results.json) |
| UDP Services | ✅ 600 exact responses; each Service resolves to its intended Ready pod | [UDP Services](validation/node-group-vm-udp-service-results.json) |
| UDP between cloud workers | ✅ 1,200 exact pod and Service responses across same-cloud and cross-cloud pairs | [Cloud UDP](validation/node-group-vm-udp-cloud-results.json) |
| Recovered capacity workload | ✅ Three adopted remote dialers and 184 workload checks after allocation starvation recovered | [Capacity workload](validation/node-group-vm-capacity-workload-results.json) |
| Bootstrap observation retry | ✅ Same VM start time, guest boot ID, and single cloud-init execution across observer reconstruction | [Native retry](validation/vm-bootstrap-retry-native-results.json) |
| Ready replica aggregation | ✅ Three adopted workers; cordon/uncordon changes Ready count 3 → 2 → 3 while total remains four | [Readiness](validation/node-group-vm-readiness-results.json) |
| Group workload selection | ✅ Three ordinary pods on three group VMs; a different group UID remains unscheduled; `/scale` selects the worker pods | [Group labels](validation/node-group-vm-labels-results.json) |
| Capacity exhaustion | ✅ Three reserved Machines progress; the excess request is cancelled before bootstrap publication | [Cancellation](validation/node-group-vm-bootstrap-cancellation-results.json), [capacity recovery](validation/node-group-vm-capacity-results.json) |
| Group deletion before claim admission | ✅ Absent-child reservation and unadmitted child removed; eight Node identities and VM start times retained | [Creation cancellation](validation/node-group-vm-creation-cancellation-results.json) |

UDP sweeps cover both directions for each pair, fresh and reused sockets, and
64, 1280, 1340, 1400, and 1800-byte bodies plus the five-byte echo prefix.
The continuity observer pauses one second between matrices and measures sampled
request success. UDP during removal and high-rate packet-loss behavior remain
unverified. Bootstrap interruption during disk reset or network plumbing also
requires separate validation.

The [cancellation fence checks](validation/vm-slot-cancellation-fence-results.json)
exercise competing allocation and cancellation writes against a real API,
including first-pool creation. Native provider cancellation of an unallocated
infrastructure Machine preserves the three running VMs, eight Node UIDs, and
slot reservations. Combined provider statement coverage is 71.8%; this evidence
covers the provider operation separately from group cancellation.

[Real API slot tests](validation/vm-slot-reservation-api-results.json) cover
reservation contention, restart recovery, exhaustion, and provider finalizer
handling. Site peer publication uses a mesh resource-version check and requires
an allocated tunnel address; API tests cover retirement racing publication and
reselection of an endpoint with its existing key.

The [image-import recovery check](validation/vm-image-import-memory-results.json)
records a runner OOM and read-only guest filesystems, including a Ready Node that
could not create a new pod. Existing-disk recovery preserved all Node UIDs; serial
image import, new pod creation, and a [184-check workload matrix](validation/node-group-vm-post-memory-results.json)
then passed. This verifies recovered connectivity; traffic continuity through
that failure is unqualified.

## Pending-worker cancellation

Deleting a group cancels a reserved creation when its child is absent. A child
that has not acquired the claim controller's lifecycle finalizer is deleted with
UID and resource-version preconditions. Installing that finalizer first makes
the competing deletion fail, preserving the admitted claim's lifecycle.

Before admitting a new child, the claim controller reads its group directly from
the API and checks its UID and deletion state. A late creation from a stale
reconciler cannot start compute after its parent is deleting or gone. Already
admitted claims continue provisioning so that removal can resolve and drain
their Nodes. Real API tests cover late creation and concurrent admission. The
[native VM checks](validation/node-group-vm-creation-cancellation-results.json)
delete groups at both pre-admission stages and verify removal of the fixtures,
unchanged existing Node/Machine identities, VM start times, and slot data.

Group scale-down drains registered Nodes and can cancel a pending worker before
the join controller reserves its WireGuard address or publishes userdata.
Cancellation compares the Machine's resource version, records a marker that
blocks join publication, then persists the bootstrap Secret and mesh identities.
It deletes the exact child claim and waits for claim/CAPI/provider teardown.
Provider finalizers and other controllers' deletion hooks remain in force.
Real API tests cover both orders of the address-reservation race. The
[native CAPI VM test](validation/node-group-vm-bootstrap-cancellation-results.json)
removes the excess request from a three-slot pool, preserving the three allocated
workers, all eight Node identities, VM start times, and slot data. The subsequent
[184-check workload network matrix](validation/node-group-vm-post-cancellation-network-results.json)
passes across all eight guests; it measures connectivity after cancellation.

An existing address reservation, bootstrap Secret, peer entry, or claimed Node
requires the remaining full cancellation lifecycle. Those workers retain their
claim and drain action.
The action records the owned Machine UID while Node registration is pending,
and retains a provider ID once observed. Late registration fills the Node fields;
a replacement Machine, changed provider ID, or mismatched Node reference UID
cannot replace the recorded target. This journal update does not authorize
compute termination. The [native pending-target check](validation/node-group-vm-pending-target-results.json)
records the fourth Machine UID while all eight Nodes and three workload pods
remain Ready, with unchanged Node identities and slot data. Real API tests cover
late registration and conflicting identity updates.
In the native four-request, three-slot case, the excess Machine already has a
bootstrap Secret reference despite having no `providerID` or `nodeRef`. The
referenced Secret and peer entries are absent. The reference alone does not
establish that userdata exists, and the missing status fields alone cannot
establish that a provider has never launched compute.

Cancellation needs its own persisted, UID-bound lifecycle. It must fence further
bootstrap and peer publication, retire any published tunnel membership, and
coordinate termination through CAPI's provider lifecycle. If a Node appears while
cancellation is pending, removal must account for that Node and its workloads.
The [CAPI deletion lifecycle](https://cluster-api.sigs.k8s.io/tasks/automated-machine-management/machine_deletions)
provides pre-drain and pre-terminate hooks before infrastructure deletion. The
cancellation implementation must retain its own hook ownership while preserving
hooks held by gateway attachments or other controllers. A persisted Machine UID
identifies the target; it does not prove that compute has stopped.
Completion requires evidence that the original compute and its associated
resources are gone. The group implementation must use this contract across
providers, rather than treating a VM slot observation as proof for AWS. Existing
workload eviction and peer-withdrawal gates continue to apply to joined workers.

## API and ownership

Introduce `ProvisionedNodeGroupClaim` with:

- `spec.replicas`: desired integer machine count, default 1, minimum 0.
- `spec.template.spec`: the existing single-node claim specification.
- `status.replicas`: live children, including provisioning and draining machines.
- `status.readyReplicas`: children whose owned CAPI Machine is Ready, whose matching
  workload Node is Ready and uncordoned, and whose peer document matches the current
  mesh render with an applied acknowledgement. Draining and terminating children
  are excluded. Multiple children resolving to the same Node are excluded.
- `status.selector`: workload pod selector used by the scale API.
- Conditions for capacity pending, draining, blocked deletion, and readiness.

A group owns individual claims by UID. Each child keeps its own CAPI Machine,
bootstrap credential, WireGuard identity, and lifecycle finalizer. Group names and
child ordinals provide stable reconciliation identities; owner UIDs prevent adoption
of children belonging to a deleted/recreated group. Group template changes affect
new children. Rolling replacement requires an explicit later policy.

Use a separate resource to preserve existing single-claim names, status fields,
finalizers, and one-machine semantics. Implement the group controller against
claims so VM, AWS, Windows, and future providers share the same orchestration.

Readiness is an API observation of capacity and applied configuration. Native
workload tests qualify continued connectivity separately. A pending Machine with
no resolved Node is polled every five seconds while its group action remains
reserved, allowing readiness updates without an increasing error backoff.

## Selecting a worker group

The controller labels each associated Node with
`cloud-provisioning.appmana.com/node-group-uid`. It verifies the claim and CAPI
ownership chain and the Node's provider ID before applying a UID/version-checked
patch. Existing unrelated labels are preserved. A Node already labeled for another
group is retained for investigation. Labels remain through drain until the Node
is removed by the claim lifecycle.

Read the group UID after creating the group:

```sh
kubectl -n cloud-provisioning get provisionednodegroupclaim render-workers \
  -o jsonpath='{.metadata.uid}{"\n"}'
```

Set the worker Deployment's pod template before enabling its autoscaler. Replace
`GROUP_UID_FROM_COMMAND` with that UID:

```yaml
spec:
  template:
    spec:
      nodeSelector:
        kubernetes.io/os: linux
        cloud-provisioning.appmana.com/node-group-uid: GROUP_UID_FROM_COMMAND
      tolerations:
        - key: cloud-provisioning.appmana.com/internet-facing
          operator: Exists
          effect: NoSchedule
```

Set the group's `spec.workloadSelector.matchLabels` to the worker pods' labels,
for example `app: render-workers`. This selector is exposed through `/scale`.
Keep the pods and group in the same namespace. Recreating a group gives it a new
UID; update the pod template's
`nodeSelector` to target that replacement group. The broad `role: cloud-worker`
Node label continues to identify cloud workers across individual claims and groups.

The [native scheduling check](validation/node-group-vm-labels-results.json) uses
required pod anti-affinity to place three pods on three group workers. A pod
selecting a different group UID remains unscheduled. Native Windows and KEDA
capacity-scaling checks remain separate acceptance work.

## Scaling and KEDA

Expose `/scale` with `.spec.replicas`, `.status.replicas`, and `.status.selector`.
KEDA supports custom resources with a scale subresource. Use external metrics such
as queue backlog for machine capacity; CPU-based pod averages are unsuitable for
counting machines. See [KEDA custom-resource scaling](https://keda.sh/docs/2.20/concepts/scaling-deployments/)
and [Kubernetes scale fields](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#scale-subresource).

A workload Deployment and a node group need separate scaling targets. For example,
a worker consumes one job at a time, with four workers per machine:

- Workload target: one pod per queued job, capped by the workload budget.
- Capacity target: one machine per four desired workers, capped by the cloud budget.
- Count provisioning children toward capacity to avoid duplicate launches during boot.
- Keep KEDA, metrics, queue services, and provisioning controllers on permanent nodes.
- Use the group UID in the pod template’s Node selector and set the group’s
  workload selector to the pods’ labels. The scale selector describes pods.

The example below is a design preview; install it only after the group CRD and
controller are implemented and validated. The queue metric must remain available
when worker replicas reach zero.

```yaml
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeGroupClaim
metadata:
  name: render-workers
  namespace: cloud-provisioning
spec:
  replicas: 0
  workloadSelector:
    matchLabels:
      app: render-workers
  template:
    spec:
      clusterName: my-cluster
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: AWSMachineTemplate
        name: render-worker
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: render-capacity
  namespace: cloud-provisioning
spec:
  scaleTargetRef:
    apiVersion: cloud-provisioning.appmana.com/v1alpha1
    kind: ProvisionedNodeGroupClaim
    name: render-workers
  minReplicaCount: 0
  maxReplicaCount: 10
  cooldownPeriod: 900
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring.svc:9090
        query: sum(render_jobs_pending)
        threshold: "4"
```

## Safe removal

The controller's claim and attachment paths use CAPI `v1beta2`. Attachment API
validation serves only that version, including deletion-hook retention and
request retirement. The live harness also serves `v1beta1` for compatibility.


Select excess children deterministically, one at a time. Cordon the chosen Node,
evict workloads through the eviction API respecting disruption budgets, wait for
active work, withdraw tunnel/attachment ownership, then delete the child claim.
Timeouts retain a blocked condition and resource ownership; forced deletion needs
an explicit policy. Terminating children occupy their slots until disappearance.
Group deletion follows the same sequence under a finalizer. The existing single
claim configures a 120-second CAPI drain timeout; group scale-down must complete
its own eviction and withdrawal gates before triggering that teardown.

## Implementation and acceptance plan

- [x] Inspect the single-claim lifecycle and define a separate group API.
- [x] Add a deterministic replica planner with ownership and terminating-slot tests.
      See `controller/pkg/nodegroup`; it proposes one create or drain at a time.
- [x] Add API types and an experimental CRD with scale and status subresources.
      An isolated API test verifies defaulting, scale-to-zero, status preservation,
      and resource-version conflicts. The CRD is in `controller/pkg/nodegroup/testdata`.
- [x] Register the experimental controller behind an explicit flag, with namespace
      filtering, child ownership watches and opt-in Helm RBAC. The real-manager
      API test observes group creation and creates the owned child through its
      watch loop. Helm rendering verifies enabled and disabled configurations.
- [ ] Promote the group CRD from manual experimental installation after lifecycle
      and VM acceptance.
- [x] Add deterministic child construction and ownership validation. Child names
      include a hash of the group UID; a recreated group cannot adopt its predecessor’s
      claims. Template copies are independent, and terminating children retain slots.
- [x] Persist a pending action with a resource-version check before changing children.
      Creation intent freezes the template; removal intent records the child UID.
      The real API test verifies competing reservations conflict and scale updates
      preserve an in-flight action. Pending actions survive controller restarts.
- [x] Resume reserved creation through direct API reads. A retry returns the same
      claim UID and uses the frozen template after scale or template updates.
      The real API test rejects mismatched group/action identities and preserves
      conflicting claims for investigation.
- [x] Complete creation after observing the reserved child UID. Status writes use
      resource-version checks; stale completion cannot clear a later reservation.
      The API test verifies the next replica uses the current template. Creation
      completion records a claim, while Node readiness remains a separate gate.
- [x] Connect planning, reservation, creation, and completion in an experimental
      reconciler. A real API test reaches three claims while recreating controller
      state between passes. Scale-down reserves one child and holds removal while
      drain and withdrawal integration is pending.
- [x] During group deletion, complete a pending creation for an admitted matching
      child, then reserve it for draining. A reconciler that observes deletion
      does not create an absent child; late writes are fenced at claim admission.
- [x] Cancel absent-child creation reservations during group deletion and remove
      unadmitted children with UID/version preconditions. Real API tests cover
      stale creation after parent deletion and admission winning the deletion race.
- [x] Validate creation cancellation in the single-NIC VM lab with both an absent
      child and an unadmitted child. Existing workers retain their identities.
- [x] Fence cancellation against the join controller's address reservation and
      cancel admitted claims whose bootstrap data has not been published. Real
      API tests verify the race, retained deletion gates, and changed-proof holds.
- [x] Validate pre-bootstrap cancellation through the native CAPI VM provider.
      The four-request, three-slot group returns to three Ready replicas with
      the excess claim, CAPI Machine, and infrastructure Machine gone.
- [ ] Cancel admitted workers after address reservation or userdata publication,
      including failed joins and Nodes that register during cancellation.
- [x] Publish observed replica counts before advancing pending actions and expose
      the workload selector through `/scale`. Provisioning and draining claims
      remain counted while their lifecycle work is pending.
- [x] Publish group UID labels on associated Nodes with ownership and UID/version
      checks. Native scheduling selects the three group VMs; real API tests
      reject concurrent updates and replacement Nodes.
- [x] Aggregate CAPI/Node readiness and current peer-document acknowledgements.
      The native check verifies 3 → 2 → 3 Ready workers during cordon/uncordon,
      retaining four total children and the pending cancellation action. Unit
      tests cover Linux/Windows and both CAPI object versions, including identity
      and receipt changes during observation.
- [x] Add a workload API drain operation with Node UID checks, cordon, and
      UID-preconditioned pod eviction. Real API tests verify PDB blockage,
      subsequent eviction, and retention of network DaemonSet pods.
- [x] Resolve and persist the Machine UID, Node UID, and provider ID for a drain
      reservation. Resolver tests cover CAPI `v1beta1` and `v1beta2` object shapes
      and reject replacement Nodes; the real API test verifies schema retention.
      A [live identity audit](validation/node-group-capi-identity-observations.json)
      found matching claim owner UIDs and Node provider IDs for eight existing
      Machines, including local harness and AWS Windows/GPU workers. Group drain
      execution against those workers remains part of VM acceptance.
- [x] Connect target binding and drain to the experimental reconciler with explicit
      management/workload clients and the served CAPI version. Tests verify that
      a drained worker retains its claim while withdrawal integration is pending.
- [x] Add retirement of an exact gateway request UID through the existing
      attachment controller. The helper checks worker identity and waits for
      both the durable `Complete` record and request disappearance. Unit tests
      verify that pending hook cleanup and replacement requests retain the gate.
      The registered-controller API test retires two successive same-name requests,
      rejects the old UID against its replacement, and requires both API removal
      and a durable completion record. This test makes zero cloud calls.
- [x] Discover gateway requests by worker identity within a mesh, retaining
      requests after selector-label removal. Tests check stable name/UID inventory,
      replacement identities, mesh scope, and shared gateway rejection; the real
      API retirement test verifies discovery of each current request UID.
- [x] Persist the discovered gateway request set in the drain action, then retire
      those requests serially. The real API test verifies schema retention and
      blocks retirement when an additional request appears after capture.
- [x] Verify serial retirement of two gateway attachments against the real API
      and attachment state machines. Controlled native acknowledgements keep the
      first request pending across a group reload; the second remains untouched
      until the first completes. The worker claim survives both retirements.
- [x] Persist an action-owned drain marker on the CAPI Machine after workload
      drain and gateway inventory capture. Attachment lifetime checks retire
      requests for marked participants and preserve existing deletion hooks.
      Unit tests cover retries, competing markers, and protected/unprotected leases.
- [x] Prepare direct peer withdrawal from a mesh snapshot using the expected
      public key. Unit tests preserve survivor entries and address reservations,
      reject changed or missing key identities, and verify repeatable removal.
- [x] Publish prepared direct peer removal with mesh UID/resource-version checks.
      The API test rejects concurrent survivor updates and same-name replacement
      peers while preserving address reservations.
- [x] Fence bootstrap, endpoint, and pod-prefix peer writes with a fresh Machine
      UID/drain check followed by an optimistic-lock mesh update. The real API
      test covers a writer paused before withdrawal and a stale Machine snapshot
      after the drain marker. These checks supersede the unconditional peer
      patches introduced in `f6e3d797`; surviving peer updates remain intact.
- [x] Prepare public direct-peer withdrawal identities using the existing CAPI
      consumer resolver. Preserve unavailable survivors, reject missing consumers
      and concurrent mesh changes, and exclude only the bound retiring worker.
      Serialization tests retain the complete recipient set.
- [x] Persist direct-peer identities in the pending group action after gateway
      retirement. The real API test verifies CRD retention across reloads, deep
      copies, stale-writer rejection, and preservation of the peer and child claim.
- [x] Publish the captured direct-peer withdrawal through the mesh version check
      and gate completion on every retained consumer receipt. The real API test
      withholds site and remote acknowledgements independently, reloads the group,
      and verifies that missing recipients and new membership block completion.
      These controlled receipts test controller behavior, not native application.
- [x] Commit removal after withdrawal receipts, delete the recorded child claim
      with UID/version preconditions, and wait for claim, Machine and Node absence.
      The real API test preserves finalizers, rejects a replacement Machine,
      resumes across reloads, and rejects replay of an old completed action.
      CAPI teardown is simulated by deleting test resources at each boundary.
- [x] Extend the saved withdrawal inventory for newly published peers before
      accepting acknowledgements. The real API test retains the original prefix,
      reloads the extension, rejects stale updates and changed keys, and preserves
      recipients that disappear from current publication.
- [ ] Exercise direct withdrawal, concurrent membership changes and claim teardown
      on VMs. Concurrent retirement of a required consumer needs explicit lifetime
      evidence before its acknowledgement requirement can be released.
- [ ] Verify native retirement acknowledgements and preparation races around the
      drain marker, withdraw direct mesh peers, and serialize claim removal.
      Gateway retirement alone leaves the claim retained.
- [ ] Verify real API scale updates and KEDA external-metric behavior, including zero.
- [ ] Run single-NIC VM 0→3→1→0, controller restart, failed boot, PDB blockage,
      group recreation, and survivor-traffic tests.
- [ ] Repeat supported Linux/Windows cases with real CAPA and prepared GPU images.
