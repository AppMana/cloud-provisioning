# Provisioned node groups

Status: experimental API and claim reconciler; the current `ProvisionedNodeClaim`
provisions one machine. Group orchestration requires explicit enablement and
remains under development.

## Enable in an isolated harness

The chart installs the experimental API contract. Enable the controller on the
harness release using its existing values and a newly built controller image:

```sh
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
It defaults to `false`; the group CRD is installed with the chart either way.
KEDA-driven scaling has native Linux k0s/Calico VM coverage. Broader distribution,
Windows, and real-cloud group qualification remain pending.

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

Use a `LabMachineTemplate` with an empty `spec.template.spec` for a
pool. A template with `nodeName: remote1` describes a single fixed VM and
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

## Pending-worker cancellation

Deleting a group cancels a reserved creation when its child is absent. A child
that has not acquired the claim controller's lifecycle finalizer is deleted with
UID and resource-version preconditions. Installing that finalizer first makes
the competing deletion fail, preserving the admitted claim's lifecycle.

Before admitting a new child, the claim controller reads its group directly from
the API and checks its UID and deletion state. A late creation from a stale
reconciler cannot start compute after its parent is deleting or gone. Already
admitted claims continue provisioning so that removal can resolve and drain
their Nodes. Real API tests cover late creation and concurrent admission.

An existing address reservation, bootstrap Secret, peer entry, or claimed Node
requires the remaining full cancellation lifecycle. Those workers retain their
claim and drain action. The action records the owned Machine UID while Node registration is pending,
and retains a provider ID once observed. Late registration fills the Node fields;
a replacement Machine, changed provider ID, or mismatched Node reference UID
cannot replace the recorded target. This journal update does not authorize
compute termination. Real API tests cover late registration and conflicting identity updates.

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

The [VM scenario fixtures](../harness/e2e/runner/keda/README.md) use KEDA `2.20.0`
and a Redis list. Nine queued items request three Machines; reducing the list to
three items drains and removes two Machines. Restoring nine items creates two
new Machine and Node identities. A queue depth requesting four Machines remains
capped at the configured maximum of three. Emptying the queue removes all three
workers and releases their slots. Three new items activate a fresh worker from
zero, with a new Machine and Node identity. The five permanent site Nodes retain
their identities throughout.

The example below uses Prometheus for an application queue. Its metric must remain
available when worker replicas reach zero. The Redis VM result does not qualify
this Prometheus or AWS configuration; validate the queue metric and cloud image
for the intended deployment.

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
