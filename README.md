# cloud-provisioning

Joins cloud workers to a private Kubernetes cluster over WireGuard. Cluster API
provisions machines; `ProvisionedNodeClaim`s manage their lifecycle.

## Install

By default, every on-premises Linux node and every remote cloud worker runs the
WireGuard tunnel (`tunnel.endpoints: "all"`). Remote cloud workers always run
the tunnel; `tunnel.endpoints` only selects which on-premises nodes terminate
tunnels.

### Full mesh (default)

[`examples/helm-release.yaml`](examples/helm-release.yaml):

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: cloud-provisioning
  namespace: cloud-provisioning
spec:
  interval: 10m
  url: oci://ghcr.io/appmana/charts/cloud-provisioning
  ref:
    tag: 0.1.23
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cloud-provisioning
  namespace: cloud-provisioning
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: cloud-provisioning
  values:
    joinProvider: k0s
    tunnel:
      endpoints: "all"
```

### Specific on-premises gateway nodes + all remote nodes

[`examples/helm-release-gateways.yaml`](examples/helm-release-gateways.yaml):

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cloud-provisioning
  namespace: cloud-provisioning
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: cloud-provisioning
  values:
    joinProvider: k0s
    tunnel:
      endpoints:
        matchLabels:
          tunnel-gateway: "true"
        # Or by name:
        # nodeNames: ["onprem-gateway-1", "onprem-gateway-2"]
    transit:
      bgpPort: 1790
```

## Provisioning machines across clouds

Multiple Cluster API providers (`AWSCluster`, `GCPCluster`, `AzureCluster`, etc.)
can coexist in the same namespace. Each `ProvisionedNodeClaim` references a
`*MachineTemplate` and is matched to its corresponding CAPI `Cluster`
(see [`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml)
and [`examples/labcontainers-workers.yaml`](examples/labcontainers-workers.yaml)):

```yaml
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: aws-worker-1
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSMachineTemplate
    name: aws-ubuntu-amd64
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: aws-worker-2
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSMachineTemplate
    name: aws-ubuntu-amd64
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: gcp-worker-1
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: GCPMachineTemplate
    name: gcp-ubuntu-amd64
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: azure-worker-1
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AzureMachineTemplate
    name: azure-ubuntu-amd64
```

| Provider | Package | Specialization |
| --- | --- | --- |
| AWS (`AWSMachine`) | `controller/pkg/join/aws` | EC2 IMDSv2 identity, CAPA credential/SG validation, optional `aws-vpc` CNI |
| GCP (`GCPMachine`) | `controller/pkg/join/gcp` | GCE metadata identity, `credentialsRef` check, `publicIP`/`imageFamily`/`failureDomain` defaults, `startup-script` `cloud-init` bridge |
| Generic CAPI (`*Machine`) | `controller/pkg/join/generic` | Fallback for any `<Kind>Template` / `<Kind>Cluster` (e.g. CAPZ `AzureMachine`) |
| Labcontainers / Docker | `controller/pkg/join/labcontainers`, `controller/pkg/join/docker` | Simulated WAN VMs and containers for e2e tests |

## Cluster preflight and remediation

```sh
endpoint-controller check --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG"
endpoint-controller fix   --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG" --manifests=./clusters/my-cluster
```

## Supported configurations

| Distro | Site CNI | Worker OS / Arch | Cloud CNI | AWS | GCP | Generic CAPI | Labcontainers |
| --- | --- | --- | --- | :---: | :---: | :---: | :---: |
| `k0s` | `calico,bgp` | Linux (`amd64`, `arm64`) | `site` | ☑ |  |  | ☑ |
| `k0s` | `calico,bgp` | Linux (`amd64`, `arm64`) | `aws-vpc` | ☑ |  |  |  |
| `k0s` | `calico,bgp` | Windows (`amd64`) | `site` | ☑ |  |  |  |
| `k0s` | `calico,bgp` | Windows (`amd64`) | `aws-vpc` | ☑ |  |  |  |
| `k0s` | `calico,vxlan` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |
| `k0s` | `kube-router` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |
| `k3s` | `flannel` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |
| `rke2` | `canal` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |
| `microk8s` | `calico` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |
| `kubeadm` | `calico,bgp` | Linux (`amd64`, `arm64`) | `site` |  |  |  | ☑ |

## Documentation

[AWS](docs/aws.md) · [GPU workers](docs/gpu-workers.md) · [Node groups](docs/node-groups.md) ·
[Windows](docs/windows.md) · [Tunnel scenarios](docs/tunneling-scenarios.md) ·
[Dual-stack Calico BGP](docs/dual-stack.md) · [Forks](docs/forks.md) ·
[Qualification](docs/deployment-qualification.md)
