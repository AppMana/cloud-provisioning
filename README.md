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

## Compatibility

“Tested” covers only the linked scenario. “Fails” records a failed gate;
“unknown” includes incomplete qualification.

| Scenario | Result |
| --- | --- |
| k0s 1.36.4, Calico 3.32.1 BGP dual-stack ([required settings](docs/dual-stack.md#required-cluster-settings)): VM lifecycle and four placements | [tested](docs/validation/k0s-1.36.4-calico-bird-dualstack-results.json) |
| Same profile: AWS add/remove/re-add and four placements | [tested](docs/validation/aws-k0s-1.36.4-calico-bird-dualstack-results.json) |
| k0s 1.36.2, Calico: VM bootstrap and worker replacement | [tested](docs/validation/k0s-1.36-lifecycle-results.json) |
| Same version: complete placement/outage/AWS matrix | unknown |
| k0s 1.36.2, Kube-router: lifecycle and placements | [tested](docs/validation/k0s-1.36-kuberouter-lifecycle-results.json) |
| Same profile: twelve outage cases | [tested](docs/validation/k0s-1.36-kuberouter-outage-results.json) |
| MicroK8s 1.34.9, Calico: VM lifecycle/placements | [tested](docs/validation/microk8s-lifecycle-results.json) |
| MicroK8s CAPA lifecycle checks | [tested](docs/validation/microk8s-capa-lifecycle-results.json) |
| MicroK8s loss-free UDP endpoint handover | [fails](docs/validation/microk8s-source-routing-results.json) |
| k0s 1.34.1, Calico: AWS placements and worker replacement | [tested](docs/validation/aws-k0s-calico-results.json) |
| k3s/Flannel, RKE2/Canal, kubeadm/Calico recorded VM scenarios | [tested](docs/validation/single-nic-vms.md) |
| OKD/OVN full remote-worker lifecycle | unknown |
| Linux KEDA worker groups | [tested](docs/validation/node-group-keda-results.json) |
| Pooled workers: steady-state cross-cloud UDP | [tested](docs/validation/node-group-vm-udp-cloud-results.json) |
| Windows 2022/2025 host tunnel generation changes | [tested](docs/validation/windows-stable-owner-isolation-results.json) |
| Windows Calico MTU repair | [tested](docs/validation/windows-mtu-periodic-repair-results.json) |
| Windows full cloud-worker CNI/lifecycle | unknown |
| Linux T4 Vulkan/NVENC/network workloads | [tested](docs/validation/linux-gpu-results.json) |
| Windows GPU lifecycle; Linux arm64 workloads | unknown |
| Latest recorded main chart publication | [fails](https://github.com/AppMana/cloud-provisioning/actions/runs/34423319430) |

[Build configuration](.github/workflows/build-controller.yml) ·
[VM harness](docs/validation/single-nic-vms.md)
