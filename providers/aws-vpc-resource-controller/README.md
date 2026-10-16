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
```

The node selector does not scope admission webhooks. Deployment must also limit
the Windows IPAM webhook to workloads explicitly selecting AWS Windows nodes;
site Windows workloads must retain their existing resource requests. Reuse the
upstream RBAC, certificate and webhook manifests with that restriction.

This source extension alone does not install Windows networking. Deployment
also requires AWS-side controller placement for upstream IMDS discovery,
scoped EC2 permissions, Windows IPAM enablement, and the native Windows CNI
plugin and Kubernetes connector. Preserve k0s's runtime and kubelet ownership.
Qualify fresh Windows 2022 and 2025 workers, native pod allocation and release,
and site connectivity according to [the hybrid CNI contract](../../docs/hybrid-cni.md).
