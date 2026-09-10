# KEDA node-group VM scenario

These fixtures target an existing isolated VM lab with a `pooled-workers`
`ProvisionedNodeGroupClaim` in `cloud-provisioning`, three remote slots, and the
experimental node-group controller enabled. `w2` is a permanent site worker.
Run the commands through the lab's bastion and Docker daemon as described in the
[isolated runner documentation](../README.md).

KEDA 2.20.0 and Redis run on `w2`. The Redis list contains controlled test demand;
probe pods verify placement and connectivity separately. Redis persistence is
disabled for this disposable test.
Production queues need their own durability and authentication configuration.

## Setup

Preload these images into `w2` through the VM harness image importer:

- `ghcr.io/kedacore/keda:2.20.0`
- `ghcr.io/kedacore/keda-metrics-apiserver:2.20.0`
- `ghcr.io/kedacore/keda-admission-webhooks:2.20.0`
- `redis:8-alpine`

Record the resolved image digests for each campaign. `imagePullPolicy: Never`
uses those local images. Newly created remote guests also need the campaign's
built dialer image and `busybox:1.37` probe image, imported through QEMU Guest
Agent. This is harness fixture setup; cloud deployments use published images or
baked image caches.

Install the fixtures with the repository files available to the bastion:

```sh
helm install keda keda --repo https://kedacore.github.io/charts \
  --version 2.20.0 --namespace keda --create-namespace \
  -f values.yaml --wait --timeout=4m
kubectl apply -f redis.yaml
kubectl -n cloud-provisioning rollout status deployment/cldt-keda-redis
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli RPUSH cldt-capacity job1 job2 job3 job4 job5 job6 job7 job8 job9
kubectl apply -f scaledobject.yaml
```

The list must be empty before seeding it. Nine items request three Machines;
`listLength: "3"` sets three queued items per Machine. The fixture caps capacity
at three Machines and allows zero. The shortened polling, cooldown, and HPA
stabilization settings accelerate this test; production settings should account
for boot time and workload disruption.

## Observe scaling

Inspect the actual scaler and group state:

```sh
kubectl -n cloud-provisioning get scaledobject cldt-node-capacity
kubectl -n cloud-provisioning get hpa keda-hpa-cldt-node-capacity
kubectl -n cloud-provisioning get provisionednodegroupclaim pooled-workers -o yaml
```

Reduce demand to three items, then restore it to nine:

```sh
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli LTRIM cldt-capacity 0 2
# Wait for one Ready Machine and completion of the group's removal action.
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli RPUSH cldt-capacity job4 job5 job6 job7 job8 job9
```

For each transition, record the HPA metric and desired replica count, the group's
pending action and Ready count, CAPI and infrastructure Machine UIDs, Node UIDs,
and VM slot ownership. Scale-down must stop the excess VMs and remove their
claim, Machine, infrastructure object, and Node. Scale-up must create new
Machine and Node identities while retaining survivor identities. Wait for the
current peer-document acknowledgements and run the workload network matrix.

After the group returns to three Ready Machines, add three more items to test
the capacity limit:

```sh
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli RPUSH cldt-capacity job10 job11 job12
```

Twelve items request four Machines. Verify that the HPA reports
`ScalingLimited=True` with reason `TooManyReplicas`, while the group stays at
three desired, total, and Ready Machines.

To test zero, remove the list and wait for the cooldown and teardown. Then enqueue
three items to test activation from zero:

```sh
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli DEL cldt-capacity
# Wait for zero claims, Machines, remote Nodes, and slot owners.
kubectl -n cloud-provisioning exec deployment/cldt-keda-redis -- \
  redis-cli RPUSH cldt-capacity restart1 restart2 restart3
```

Deleting the ScaledObject stops automatic capacity changes. Keep the queue and
scaler active until the intended final capacity is verified.

See [node groups](../../../../docs/node-groups.md) for the supported lifecycle
boundaries and recorded validation. KEDA's [Redis-list scaler documentation](https://keda.sh/docs/2.20/scalers/redis-lists/)
explains the queue metric; [custom-resource scaling](https://keda.sh/docs/2.20/concepts/scaling-deployments/)
explains the scale-subresource target.
