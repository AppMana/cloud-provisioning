# cloud-provisioning

Joins cloud workers to a private Kubernetes cluster over WireGuard.
Cluster API provisions machines; claims manage their lifecycle. Useful for
public ingress and elastic compute without exposing the control plane.

## Install and use

Configure Cluster API, your infrastructure provider and credentials using the
[AWS setup](docs/aws.md). Then install the chart with an explicit published version:

```sh
helm upgrade --install cloud-provisioning \
  oci://ghcr.io/appmana/charts/cloud-provisioning --version "$CHART_VERSION" \
  --namespace cloud-provisioning --create-namespace \
  --set providerManagerNamespace=capa-system \
  --set-string tunnel.endpoints='kubernetes.io/hostname=worker-1'
```

Apply [a worker template and claim](examples/aws-worker.yaml). Claims create
billable resources; deleting one removes its worker. The chart uses these
Linux amd64/arm64 images:

- ghcr.io/appmana/cloud-provisioning-endpoint-controller
- ghcr.io/appmana/cloud-provisioning-dialer

Use matching published chart/image versions and configure the first-boot dialer
download as described in [setup](docs/aws.md). No Windows controller image is
required to manage Windows workers.

For mixed Linux/Windows **k0s 1.36.4 + Calico 3.32.2 BGP**, use the
[Calico adapter](https://github.com/AppMana/forks-calico-windows-ipv6/tree/appmana-v3.32.2/windows-adapter)
to maintain k0sctl image settings and Windows GitOps manifests. No k0s fork is
needed. Cloud transport must still match the cluster's CNI, routing and MTU.

[GPU workers](docs/gpu-workers.md) · [Node groups](docs/node-groups.md) ·
[Windows constraints](docs/windows.md) · [Tunnel scenarios](docs/tunneling-scenarios.md) ·
[Dual-stack Calico BGP](docs/dual-stack.md)
