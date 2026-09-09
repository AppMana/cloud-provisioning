# Provisioned node groups

Status: proposed API; the current `ProvisionedNodeClaim` provisions one machine.
The initial planner implementation is separate from the production reconciler.

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

Select excess children deterministically, one at a time. Cordon the chosen Node,
evict workloads through the eviction API respecting disruption budgets, wait for
active work, withdraw tunnel/attachment ownership, then delete the child claim.
Timeouts retain a blocked condition and resource ownership; forced deletion needs
an explicit policy. Terminating children occupy their slots until disappearance.
Group deletion follows the same sequence under a finalizer.

## Implementation and acceptance plan

- [x] Inspect the single-claim lifecycle and define a separate group API.
- [ ] Add a deterministic replica planner with ownership and terminating-slot tests.
- [ ] Register API types, CRD schema, scale subresource, status, RBAC, and watches.
- [ ] Implement child creation and Ready aggregation with conflict-safe updates.
- [ ] Implement cordon, eviction, withdrawal, and serialized removal.
- [ ] Verify real API scale updates and KEDA external-metric behavior, including zero.
- [ ] Run single-NIC VM 0→3→1→0, controller restart, failed boot, PDB blockage,
      group recreation, and survivor-traffic tests.
- [ ] Repeat supported Linux/Windows cases with real CAPA and prepared GPU images.
