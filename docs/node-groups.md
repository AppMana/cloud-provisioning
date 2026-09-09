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
Ready aggregation, KEDA integration and complete VM qualification remain pending.

The fake CAPI provider currently binds each template to a fixed remote VM slot.
A template naming `remote1` cannot supply multiple group replicas: the provider
rejects duplicate bindings before reporting addresses or starting a guest.
Generated group child names use explicit slot allocation when the harness
provider is configured with `Controller.Slots`. The [slot reservation store](validation/vm-slot-reservation-api-results.json)
now reserves one remote slot per infrastructure Machine UID with ConfigMap
UID/version checks. Its real API test covers restart recovery, contention and
capacity exhaustion. The provider now reserves fixed bindings first and assigns unused slots to
generated names, persisting annotations before address publication or bootstrap.
Pooled provider IDs include the infrastructure Machine UID. The provider records successful VM stop before removing its finalizer and
releases the reservation after the old infrastructure UID disappears. API tests
use a controlled stop callback; native pooled boot/teardown remains to be validated in the group VM campaign. The provider installs its finalizer before reserving capacity. A deleting
Machine that has no reservation and no bootstrap receipt releases only the
provider finalizer; it allocates no slot and never enters the compute lifecycle.
API tests cover cancellation while the pool is full and preservation of another
controller's finalizer. This allocation is
separate from CAPA, where AWS provisions each Machine from the shared template.


## Pooled VM provider loop

Create a fresh isolated VM lab with `cmd/lab -remote-slots 3` for the three-worker
campaign. The added `remote3` has one NIC on cloud A with its own address. Use
the same capacity when starting the provider loop from `harness/e2e`:

```sh
go run ./cmd/vmprovider -work-dir /work -remote-slots 3 \
  -namespace cloud-provisioning
```

The command uses the existing VM topology and holds the lab process lock. It
serves `/work/binaries/wg-dialer-linux-amd64` at the harness's existing bootstrap
URL while reconciling infrastructure Machines. Keep it running throughout the
campaign; it does not build the site, install CAPI, or create group claims.
Install the group controller and prepare the imported CAPI Cluster and a
`ContainernetMachineTemplate` with an empty slot binding separately. Fixed
`containerName` templates still describe one particular VM, so replicas require
the unbound template. Use this mode on a fresh pool; existing legacy provider IDs
do not contain an infrastructure UID.

The command and topology tests pass, and slot transactions have real API
coverage. The [three-slot VM topology check](validation/node-group-vm-topology-results.json)
verifies one physical NIC on each of eight guests, site-to-cloud reachability,
and isolation of the site from direct remote access. A completed pooled VM
lifecycle run is still pending.

The [initial pooled launch observation](validation/node-group-vm-launch-observation.json)
created three claims with distinct VM bindings. The provider pass expired while
waiting for the third VM after launch. That run used a bootstrap retry path that
reset the VM disk. The provider now supplies the infrastructure UID to the VM
bootstrap implementation, which records a durable launch receipt before waiting
for guest readiness. Retries verify the wrapper and boot identities and observe
the existing boot. A stopped predecessor is required before a new UID can reuse
its slot. An interrupted launch with an unresolved outcome remains held for
inspection. Regression tests cover observer reconstruction after a timeout;
native verification of the updated implementation remains pending.

The [native three-to-one scale-down](validation/node-group-vm-scale-down-results.json)
removed two claims, their CAPI and infrastructure Machines, and their Nodes.
Both VM wrappers stopped and their slot reservations were released. The remaining
Node identities were unchanged. A 121-second host-tunnel probe during removal
received all 600 packets; pod and Service continuity and disruption-budget
behavior remain to be tested. The offline lab requires the exact remote dialer
DaemonSet image to be loaded into each joined VM's distribution runtime before
adoption can complete, as in the single-node harness. The pooled provider command
does not yet automate this image-loading step.

The [native disruption-budget check](validation/node-group-vm-pdb-results.json)
held a Ready pod and its Machine while `minAvailable: 1` prohibited eviction.
Changing the budget to zero allowed eviction. Final scale-to-zero is still
held at withdrawal: three control-plane consumers retained their Node UIDs but
changed published keys while reporting the transit role. The group rejects the
changed identities. This transition requires investigation before scale-to-zero
can be marked validated.

## API and ownership

Introduce `ProvisionedNodeGroupClaim` with:

- `spec.replicas`: desired integer machine count, default 1, minimum 0.
- `spec.template.spec`: the existing single-node claim specification.
- `status.replicas`: live children, including provisioning and draining machines.
- `status.readyReplicas`: children with verified Ready Nodes and healthy attachment.
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
- Define a stable workload selector and propagate a group label to Nodes before
  enabling scheduling. The scale selector describes pods, not child claim objects.

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
- [x] During group deletion, complete a pending creation only when its matching
      child already exists, then reserve that child for draining. The API test
      verifies an absent child is never created after deletion begins.
- [ ] Cancel unfulfilled creation reservations during group deletion and connect
      removal actions. Until cancellation is implemented, these reservations
      retain the group finalizer.
- [x] Publish observed replica counts before advancing pending actions and expose
      the workload selector through `/scale`. Provisioning and draining claims
      remain counted while their lifecycle work is pending.
- [ ] Aggregate Ready Nodes and attachment health.
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
