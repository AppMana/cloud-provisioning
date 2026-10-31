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

Online Helm upgrades preserve the dialer's existing named adoption-Secret grants.
Rendering offline cannot read those grants. For a rendered-manifest upgrade,
render against the target API and retain those exact bytes; keep machine
membership stable between rendering and applying the RBAC resources.

For Linux receiver-preparation testing, `tunnel.receivePreparation` accepts
`nativeCIDRs` (the complete protected cloud network ranges) and `ports` (distinct
UDP listener ports already permitted by the worker's security groups, excluding
the existing WireGuard port). The controller stages additional receivers through
the normal adoption reconciliation. This experimental setting requires agents
with receiver support; it freezes active peer-list updates and retains existing
endpoints. It does not activate routes or complete a handover. Removing the
setting does not discard a persisted proposal or release its forwarding base.
An optional `journal` object supplies explicit `roundLifetime`, `maxReceiptAge`
and `drainInterval` duration strings. This persists the validated public plan
and initial prepare round in a controller-owned ConfigMap, bound to the cluster
and Machine UIDs. Reconciliation resumes that exact journal; it does not renew
expired rounds or advance phases. Choose the drain bound for the deployment's
packet lifetime before enabling phase execution.

Apply [a worker template and claim](examples/aws-worker.yaml). Claims create
billable resources; deleting one removes its worker. The chart uses these
Linux amd64/arm64 images:

- ghcr.io/appmana/cloud-provisioning-endpoint-controller
- ghcr.io/appmana/cloud-provisioning-dialer

Use matching published chart/image versions. By default, first-boot dialer
bootstrap downloads the latest published multi-architecture / universal release
binary (`dialerBinary.version: "latest"`, with no manual SHA-256 pinning required
unless explicitly overridden; see [AWS setup](docs/aws.md)). No Windows controller
image is required to manage Windows workers.

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
