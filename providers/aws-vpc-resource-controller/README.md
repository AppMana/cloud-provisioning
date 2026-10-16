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

## Deployment overlay

[deploy](deploy/kustomization.yaml) composes the pinned upstream manifests. It
places the controller on AWS Linux cloud workers with host networking for native
IMDS access and uses the instance profile instead of an EKS-specific projected
identity token. Two replicas require separate workers to avoid host-port
collisions. Grant their instance profile the scoped Windows IPAM permissions
before installation; do not grant site nodes those permissions.

Create an environment-specific Kustomization that references this overlay,
replaces the upstream `controller` image with the immutable digest built from
the patched source, and generates `aws-windows-ipam-settings` in `kube-system`
with `clusterName` and `vpcID`. Do not apply the base directly: it intentionally
has no environment settings or release image selection. The controller's AWS
account and region are discovered through IMDS on its EC2 host.

The overlay preserves upstream RBAC and cert-manager certificate wiring. It
scopes admission to Windows pods explicitly selecting both the `aws-vpc` CNI
and `cloud-worker` role. All three node selectors must be present in workload
pod templates, alongside any architecture/build selectors. Host-network pods
bypass IPAM admission. Matching workload admission fails closed if the webhook
is unavailable; site workloads bypass it. Node validation checks both old and
new labels so removing the selection labels does not bypass validation.

After controller readiness and webhook certificate verification, enable Windows
IPAM through the existing `kube-system/amazon-vpc-cni` ConfigMap's
`enable-windows-ipam: "true"` setting. Preserve other keys and its existing owner;
do not replace a shared CNI ConfigMap. Keep prefix delegation disabled for the
secondary-address profile. Verify resource capacity, pod annotation assignment,
CNI execution, cooldown and release on both Windows versions before qualifying
traffic. Manifest acceptance and admission selection do not prove allocation.
