# AWS IPAM node selection

`node-selector.patch` extends AWS's VPC resource controller at commit
`ca7792b0485c7972af0d03af4a71b912c3a70add` with an optional `--node-selector`.
Use a positive selector when AWS workers share a cluster with site nodes:

```text
--node-selector=cloud-provisioning.appmana.com/cni=aws-vpc,cloud-provisioning.appmana.com/role=cloud-worker
```

The controller retains its existing allocation, cooldown and release logic.
An empty selector preserves upstream behavior. Nodes outside the selector are
unmanaged and do not receive CNINode resources. Keep selection labels stable
while workloads run: removing a label from a managed node invokes upstream
resource retirement, just like other managed-to-unmanaged transitions.

Apply and build from a clean checkout of the pinned revision:

```sh
/path/to/providers/aws-vpc-resource-controller/apply.sh .
GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test ./pkg/node/manager
GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go build -trimpath -o controller .
docker build -f /path/to/providers/aws-vpc-resource-controller/Dockerfile \
  -t your-registry/aws-windows-ipam:build .
```

The product runs the built image as Windows IPAM: set
`platforms.aws.vpcCNI.windows.ipamImage` to its digest. The chart ships the
upstream RBAC, CRDs, cert-manager certificate and webhooks, rendered from the
vendored upstream `config/` at this revision
([controller/pkg/platform/aws/upstream/vpc-resource-controller](../../controller/pkg/platform/aws/upstream/vpc-resource-controller)),
with admission scoped to Windows pods that select both the `aws-vpc` CNI and the
`cloud-worker` role: all three node selectors must be present in workload pod
templates, alongside any architecture or build selectors. Host-network pods
bypass IPAM admission. Matching workload admission fails closed if the webhook
is unavailable; site workloads bypass it. Node validation checks both old and
new labels, so removing the selection labels does not bypass validation.

The controller applies the Deployment with the cluster's facts: on AWS Linux
VPC CNI workers with host networking, where IMDS gives the controller its
region, instance identity and the worker role (the VPC CNI node policy already
covers assigning pod addresses on CAPA-owned interfaces), with
`--cluster-name` and `--vpc-id` from the one CAPI Cluster on an AWSCluster, and
this patch's `--node-selector` limited to Windows VPC CNI nodes. Two replicas
need separate workers to avoid host-port collisions. Once the Deployment is
available it sets `enable-windows-ipam: "true"` in `kube-system/amazon-vpc-cni`,
which it owns along with aws-node. Keep prefix delegation disabled for the
secondary-address profile. Verify resource capacity, pod annotation assignment,
CNI execution, cooldown and release on both Windows versions before qualifying
traffic; manifest acceptance and admission selection do not prove allocation.
