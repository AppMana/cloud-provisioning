# cloud-provisioning

Joins cloud workers to a private Kubernetes cluster over WireGuard.
Cluster API provisions machines; claims manage their lifecycle. Useful for
public ingress and elastic compute without exposing the control plane.

## Install and use

Configure Cluster API and your cloud infrastructure provider(s). By default, `cloud-provisioning` runs a **full-mesh WireGuard tunnel across every node** (`tunnel.endpoints: "all"`).

> **Important:** Remote cloud workers **always** run the WireGuard tunnel (via first-boot bootstrap and `tunnel-dialer-remote` on Linux, or `windows-tunnel.exe` on Windows). They can never be omitted by `tunnel.endpoints`. The `tunnel.endpoints` setting only chooses which **on-premises Linux nodes** terminate tunnels:
> - **Full Mesh (default, `tunnel.endpoints: "all"`)**: Every on-premises Linux node (control-plane and worker) + all remote cloud nodes run the tunnel.
> - **Designated On-Premises Gateways (`tunnel.endpoints` selector + `transitBGPPort`)**: Only the selected on-premises gateway nodes + all remote cloud nodes run the tunnel, and the gateway nodes advertise remote routes to the rest of the on-premises cluster.

### 1. Default HelmRelease: Full Mesh (Every Node Runs the Tunnel)

See [`examples/helm-release.yaml`](examples/helm-release.yaml):

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
      endpoints: "all" # default: every on-prem Linux node + all remote cloud nodes
```

Or install directly with Helm:

```sh
helm upgrade --install cloud-provisioning \
  oci://ghcr.io/appmana/charts/cloud-provisioning --version "$CHART_VERSION" \
  --namespace cloud-provisioning --create-namespace \
  -f examples/helm-release.yaml
```

### 2. Gateway HelmRelease: Specific On-Premises Nodes + All Remote Nodes

If you only want specific on-premises nodes to terminate WAN tunnels while all remote cloud workers still run the tunnel, see [`examples/helm-release-gateways.yaml`](examples/helm-release-gateways.yaml):

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
        # Or select specific on-premises nodes by name:
        # nodeNames: ["onprem-gateway-1", "onprem-gateway-2"]
    transit:
      bgpPort: 1790
```

## Cluster API Providers & Mixed Remote Clusters

`cloud-provisioning` supports **multiple Cluster API infrastructure providers simultaneously in the same cluster** (e.g., AWS + GCP + Azure/Hetzner/OpenStack):

| Provider | Package | What It Specializes |
| --- | --- | --- |
| **AWS (CAPA)** | `controller/pkg/join/aws` | Maps `AWSMachineTemplate` $\rightarrow$ `AWSMachine` $\leftrightarrow$ `AWSCluster` (`v1beta2`). Runs preflight checks on CAPA credentials (`AWSClusterStaticIdentity` / `AWSClusterRoleIdentity`) and security groups, and supports optional native AWS VPC CNI (`cloud-provisioning.appmana.com/cni: aws-vpc`). |
| **Google Cloud (CAPG)** | `controller/pkg/join/gcp` | Maps `GCPMachineTemplate` $\rightarrow$ `GCPMachine` $\leftrightarrow$ `GCPCluster` (`v1beta1`). Injects GCE metadata discovery (`gce://<project>/<zone>/<name>`), validates `GCPCluster.spec.credentialsRef`, populates `GCPCluster.status.failureDomains` on externally managed clusters, moves `zone`/`failureDomain` to `Machine.spec.failureDomain`, defaults `publicIP: true` and `imageFamily: projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-*`, and injects a GCE `startup-script` bridge that installs & triggers `cloud-init` on stock GCE images (`debian-12`, RHEL/Rocky) that lack `cloud-init`. |
| **Generic CAPI (Fallback)** | `controller/pkg/join/generic` | Automatically handles **any** standard Cluster API infrastructure provider (`<Kind>Template` $\rightarrow$ `<Kind>Machine` $\leftrightarrow$ `<Kind>Cluster` across `v1beta1` and `v1beta2`, such as `AzureMachineTemplate`/`AzureCluster`, `OpenStackMachineTemplate`/`OpenStackCluster`, `HetznerBareMetalMachineTemplate`/`HetznerCluster`, `VSphereMachineTemplate`/`VSphereCluster`). Defaults `allocatePublicIP: true` on `AzureMachine`, extracts `failureDomain` onto `Machine.spec.failureDomain`, and uses multi-cloud metadata discovery (Azure IMDS, GCE, EC2 IMDSv2, Hetzner, DigitalOcean, OpenStack, or `/etc/cloud-provisioning/provider-id`). |
| **Labcontainers / Docker** | `controller/pkg/join/labcontainers`, `controller/pkg/join/docker` | Adopts simulated WAN nodes (`LabMachineTemplate` / `LabCluster`) or Docker containers (`DockerMachineTemplate` / `DockerCluster`) for integration testing. |

### Provisioning Multiple Machines Across Mixed Clouds

In [`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml), `AWSCluster`, `GCPCluster`, and `AzureCluster` coexist in the `cloud-provisioning` namespace. Each `ProvisionedNodeClaim` simply points to the desired `*MachineTemplate` — the controller automatically routes the claim to the matching CAPI `Cluster` (`AWSMachineTemplate` $\rightarrow$ `AWSCluster`, `GCPMachineTemplate` $\rightarrow$ `GCPCluster`, `AzureMachineTemplate` $\rightarrow$ `AzureCluster`), allocates a unique WireGuard IP, and renders the node's bootstrap `Secret`.

To provision additional machines, commit one `ProvisionedNodeClaim` per worker referencing any template:

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

Deleting a `ProvisionedNodeClaim` drains the node, deletes the CAPI `Machine` and cloud instance, and releases its peer entry.

### Labcontainers Integration Testing with the Example Manifests

The Labcontainers e2e harness (`harness/e2e/install` and `harness/e2e/claim`) consumes [`examples/helm-release.yaml`](examples/helm-release.yaml) directly when installing the chart onto the simulated cluster, and [`examples/labcontainers-workers.yaml`](examples/labcontainers-workers.yaml) shows provisioning multiple simulated remote workers (`remote1`, `remote2`).

## Platform tuple preflight, automated remediation, and GitOps persistence

Before installing or attaching cloud workers, inspect your live cluster, `k0s.yaml`,
and local GitOps manifests against the target platform tuple (`<distro> + <cni[,mode,os]> + <provider[,cloud-cni]>`)
using `endpoint-controller check` or the standalone `platform-tuple` CLI:

```sh
# 1. Inspect live cluster, k0s config, and GitOps manifests for a platform tuple
go run ./controller/cmd/platform-tuple check \
  --tuple="k0s + calico,bgp + aws" \
  --kubeconfig="$KUBECONFIG" \
  --k0s-config=/etc/k0s/k0s.yaml \
  --manifests=./clusters/my-cluster

# Or via the controller binary:
endpoint-controller check --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG"

# 2. Apply automated live-cluster and GitOps-aware remediations
go run ./controller/cmd/platform-tuple fix \
  --tuple="k0s + calico,bgp + aws" \
  --kubeconfig="$KUBECONFIG" \
  --manifests=./clusters/my-cluster
```

Both `platform-tuple fix` and the running `endpoint-controller` reconciler automatically enforce and remediate:

| Intervention ID | Scope | Automated Remediation & GitOps Handling |
| --- | --- | --- |
| `calico-mtu` | `kube-system/calico-node`, `calico-windows-config-actual`, `calico-node-windows`, live host `cali*` veths | Aligns `install-cni` `CNI_NETWORK_CONFIG` `mtu`, `FELIX_IPINIPMTU`, `FELIX_VXLANMTU`, `FELIX_WIREGUARDMTU`, and Windows `CNI_MTU` / `veth_mtu` to the site/tunnel path budget (`1420` or site-configured MTU), and clamps existing live `cali*` host veths and container `eth0` interfaces in-place via `calico-node` exec with zero workload pod restarts. |
| `calico-ipam` | `IPAMConfig/default` (`crd.projectcalico.org/v1`) | Sets `spec.strictAffinity: true` and `spec.autoAllocateBlocks: true` so unencapsulated BGP nodes never borrow `/32` or `/128` addresses from remote nodes' IPAM blocks. |
| `site-cni-exclusion` | Site CNI (`calico-node`, `calico-node-windows`, Tigera `Installation`) & wildcard-toleration DaemonSets (e.g. `spegel`) | Injects required `nodeAffinity` (`cloud-provisioning.appmana.com/role NotIn [cloud-worker]` and `cloud-provisioning.appmana.com/cni NotIn [aws-vpc]`) so site CNIs and local registry mirrors never schedule onto WAN/cloud workers. |
| `cloud-worker-workloads` | `NginxProxy` (`gateway.nginx.org/v1alpha2`) & `hostPort` `Deployment`s | Patches `NginxProxy` CRs with a `StrategicMerge` deployment strategy (`type: Recreate`) and single-node / `cloud-worker` `hostPort` `Deployment`s to `Recreate` so rollouts never deadlock on port conflicts. |
| `gitops-persistence` | Flux `HelmRelease`, Flux `Kustomization`, ArgoCD `Application`, and `--manifests` | Detects GitOps ownership labels/annotations and automatically writes the corresponding `spec.postRenderers` (Flux `HelmRelease`), `spec.patches` (Flux `Kustomization`), or `spec.ignoreDifferences` (ArgoCD `Application`) upstream in-cluster and in local GitOps manifest checkouts (`--manifests`) so GitOps controllers actively preserve the fixes. |

## Tested platform-tuple matrices

### 1. Live AWS End-to-End Platform Tuples (On-Premises k0s + Calico BGP Site ↔ Real AWS EC2 Workers)

| Platform Tuple | Site Topology | Cloud Worker OS & Arch | Cloud CNI Mode | Dual-Stack (IPv4 + IPv6) | Directed Matrix & MTU Verification |
| --- | --- | --- | --- | --- | --- |
| `k0s + calico,bgp,linux + aws,site` | 5-node k0s `v1.36.2+k0s.0` + Calico `v3.32.2` BIRD BGP (`MTU 1420`) | Ubuntu / Amazon Linux (`amd64` & `arm64`, `t3.medium` / `t4g.medium`) | Site Calico BGP over WireGuard | Yes (`10.244.0.0/16` + `fd00:10:244::/56`) | **72/72 PASS** — Pod HTTP (`30/30` v4/v6), Service HTTP (`30/30` v4/v6), 1 MiB TCP (`30/30` v4/v6), DNS (`6/6`), UDP DF MTU (`1280`, `1419`, `1420` DF, `1421`, `1800`), `VerifySource` |
| `k0s + calico,bgp,windows + aws,site` (Windows Server 2025) | 5-node k0s + Calico BIRD BGP (`MTU 1420`) | Windows Server 2025 (`build 26100`, `amd64`, `t3.large`) | Calico Windows `v3.32.2-appmana.2` BGP dual-stack over WireGuard | Yes (`10.244.0.0/16` + `fd00:10:244::/56`) | **25/25 PASS** — Bidirectional Linux↔Windows & Site↔AWS Pod HTTP, Service HTTP, 1 MiB TCP, DNS, UDP DF `1420` probes, `VerifySource` |
| `k0s + calico,bgp,windows + aws,site` (Windows Server 2022) | 5-node k0s + Calico BIRD BGP (`MTU 1420`) | Windows Server 2022 (`build 20348`, `amd64`, `t3.large`) | Calico Windows `v3.32.2-appmana.2` BGP dual-stack over WireGuard | Yes (`10.244.0.0/16` + `fd00:10:244::/56`) | **25/25 PASS** — Bidirectional Linux↔Windows & Site↔AWS Pod HTTP, Service HTTP, 1 MiB TCP, DNS, UDP DF `1420` probes, `VerifySource` |
| `k0s + calico,bgp,linux + aws,aws-vpc` | 5-node k0s + Calico BIRD BGP (`overlay: Never`) | Linux (`amd64` & `arm64`) | Native AWS VPC CNI `v1.23.2` (`aws-node`, `POD_MTU=1420`, disabled Calico shadow `IPPool`) | Yes (Secondary IPv4 `/32` + Delegated IPv6 `/80`) | **PASS** — `awsrow -pod-family IPv4` & `IPv6`, CNI ownership isolation, `native-vpc-routes.json`, bidirectional source IP preservation |
| `k0s + calico,bgp,windows + aws,aws-vpc` | 5-node k0s + Calico BIRD BGP (`overlay: Never`) | Windows Server 2022 (`20348`) & 2025 (`26100`) (`amd64`) | Native `aws-vpc-cni-windows` (`vpc-bridge`) + patched `aws-vpc-resource-controller` | IPv4 Secondary-IP mode | **PASS** — `awsrow` Windows VPC CNI ownership, `vpc.amazonaws.com/PrivateIPv4Address` IPAM lifecycle, `native-vpc-routes.json`, `VerifySource` |

### 2. Local KVM / Labcontainers Distribution × CNI Qualification Matrix

| Platform Tuple | Kubernetes Distro | Site CNI & Routing Mode | Pod Routing Class | Multi-Gateway / Failover / Matrix Status |
| --- | --- | --- | --- | --- |
| `k0s + kube-router + labcontainers` | k0s | `kube-router` | `cni.Native` | **170/170 PASS** (5 site VMs + 2 remote VMs, gateway failover & drain) |
| `k0s + calico,vxlan + labcontainers` | k0s | Calico VXLAN | `cni.Encapsulated` | **PASS** (Pod, Service, DNS, MTU & Egress gateway matrix) |
| `k0s + calico,bgp + labcontainers` (`calico-site-bgp`) | k0s | Calico BIRD BGP (`overlay: Never`) | `cni.Native` | **PASS** (Unencapsulated BGP site + remote worker adoption) |
| `k0s + calico,bgp + labcontainers` (`calico-bird-dualstack`) | k0s | Calico BIRD BGP Dual-Stack (`IPv4 + IPv6`) | `cni.Native` | **PASS** (Dual-stack IPv4/IPv6 Pod, Service, DNS, 1 MiB & DF MTU matrix) |
| `k3s + flannel + labcontainers` | k3s | Flannel VXLAN | `cni.Encapsulated` | **PASS** (Embedded Flannel VXLAN over WireGuard) |
| `rke2 + canal + labcontainers` | RKE2 | Canal (Calico + Flannel VXLAN) | `cni.Encapsulated` | **PASS** (Canal overlay + WireGuard tunnel integration) |
| `microk8s + calico + labcontainers` | MicroK8s | Calico VXLAN | `cni.Encapsulated` | **PASS** (MicroK8s Calico overlay + WireGuard tunnel integration) |
| `kubeadm + calico,bgp + labcontainers` | kubeadm | Calico BIRD BGP | `cni.Native` | **PASS** (Standard kubeadm + native Calico BGP) |

### 3. Live Multi-Architecture Site Pre-Deployment & Remediation Matrix

| Target Tuple | Site Node Architectures | CNI & Dual-Stack Configuration | GitOps Engine | `platform-tuple check` & `fix` Verification |
| --- | --- | --- | --- | --- |
| `k0s + calico,bgp,linux + aws` (`cloudCNI=aws-vpc`) | Mixed `amd64` + `arm64` (3 nodes) | Calico `v3.32.2` BIRD BGP (`10.101.128.0/17` + `fd8f:cf26:522a:128::/64`, `MTU 1420`) | Flux CD (`HelmRelease` + `Kustomization`) | **15/15 PASS** — Clamped 86 live `cali*`/`cni-*` interfaces `1450 -> 1420` with zero pod restarts; set `IPAMConfig/default` `strictAffinity: true`; excluded `cloud-worker` from `calico-node` and `spegel` (persisted in `HelmRelease/spegel` `postRenderers`); patched `NginxProxy` (`Recreate`) and persisted in `Kustomization/apps` `spec.patches`. |
| `k0s + calico,bgp + aws` (`os=mixed, cloudCNI=aws-vpc`) | Mixed Linux `amd64` + Windows + External VyOS BGP routers | Calico `v3.32.2` BIRD BGP (`10.244.0.0/16` + `fd00:10:244::/56`, `MTU 1500` site / `1420` tunnel) | Flux CD (`Kustomization` + `HelmRelease`) | **PASS** — Aligned `calico-node` `CNI_NETWORK_CONFIG` `mtu` (`1500`), `calico-windows-config-actual` (`CNI_MTU=1500`, `veth_mtu=1500`), `IPAMConfig/default` `strictAffinity: true`, external BGP peer `!has(cloud-provisioning.appmana.com/role)` selectors, and Flux GitOps manifests. |

For mixed Linux/Windows **k0s 1.36.4 + Calico 3.32.2 BGP**, use the
[Calico adapter](https://github.com/AppMana/forks-calico-windows-ipv6/tree/appmana-v3.32.2/windows-adapter)
to maintain k0sctl image settings and Windows GitOps manifests. No k0s fork is
needed. Cloud transport must still match the cluster's CNI, routing and MTU.

[GPU workers](docs/gpu-workers.md) · [Node groups](docs/node-groups.md) ·
[Windows constraints](docs/windows.md) · [Tunnel scenarios](docs/tunneling-scenarios.md) ·
[Dual-stack Calico BGP](docs/dual-stack.md) · [Forked components](docs/forks.md)

Before changing an existing cluster, complete the
[deployment qualification](docs/deployment-qualification.md) for its exact CNI,
routing, and management paths. Results from a different topology or candidate
cannot authorize the rollout.
