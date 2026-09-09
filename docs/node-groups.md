# Provisioned node groups

Status: experimental API and claim reconciler; the current `ProvisionedNodeClaim`
provisions one machine. Group orchestration is separate from the production
reconciler and remains under development.

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
- [ ] Ship the CRD with controller RBAC, watches, and lifecycle orchestration.
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
