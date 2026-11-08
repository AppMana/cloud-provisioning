# cloud-provisioning

Add cloud worker nodes (AWS, GCP, Azure, etc.) to an on-premises Kubernetes
cluster over WireGuard by applying a single Kubernetes resource:
- **`ProvisionedNodeClaim`**: provisions and joins one cloud worker node.
- **`ProvisionedNodeGroupClaim`** (`pngc`): maintains a ReplicaSet of cloud
  worker nodes (`spec.replicas`), supporting `kubectl scale`, HPA, and KEDA.

## Supported Configurations

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

## 1. Install (`HelmRelease`)

[`examples/helm-release.yaml`](examples/helm-release.yaml) (full mesh) and
[`examples/helm-release-gateways.yaml`](examples/helm-release-gateways.yaml)
(designated on-premises gateway nodes):

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
    # joinProvider (k0s, k3s, rke2, microk8s, kubeadm), site CNI, and API address
    # are inferred automatically from the cluster at startup.
    # ProvisionedNodeGroupClaim (nodeGroups: true) and the mutating admission
    # webhook (webhook.enabled: true) are enabled by default.

    # Remote cloud nodes ALWAYS run the WireGuard tunnel.
    # tunnel.endpoints only chooses which on-premises Linux nodes terminate tunnels:
    # - "all" (default): full mesh (every on-prem Linux node + all remote cloud nodes).
    # - Or select specific on-prem gateway nodes (requires transit.bgpPort):
    #     endpoints:
    #       matchLabels:
    #         tunnel-gateway: "true"
    #       # nodeNames: ["onprem-gateway-1", "onprem-gateway-2"]
    #   transit:
    #     bgpPort: 1790
    tunnel:
      endpoints: "all"

    # Mutating admission webhook: automatically defaults Ubuntu 24.04 LTS AMI
    # lookup, IAM instance profile (`nodes.cluster-api-provider-aws.sigs.k8s.io`),
    # `publicIP: true`, and `cluster.x-k8s.io/managed-by: external` on
    # MachineTemplates, Clusters, and Claims.
    webhook:
      aws:
        identityName: "site-aws-identity"

    # Allow preflight checks to verify CAPI provider Secrets in their manager
    # namespaces (e.g. CAPA in capa-system, CAPG in capg-system):
    providerManagerNamespaces:
      - capa-system
      - capg-system

    # AWS credentials Secret in `cloud-provisioning` used by the less-privileged
    # runtime endpoint-controller (for automatic VPC/subnet discovery, automatic
    # creation & teardown of the cluster's WireGuard/pod-mesh security group,
    # and native AWS VPC CNI pod IP observation).
    aws:
      credentials:
        secretName: "aws-controller-credentials" # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
```

## 2. One-Time AWS IAM & Network Setup

Runtime controllers (`capa-controller-manager` and `endpoint-controller`) run
with least-privilege credentials and never hold `iam:CreateRole` or
`iam:PutRolePolicy` permissions. Run `endpoint-controller iam apply` once with
admin credentials to provision the known-quantity IAM roles, policies, and
worker instance profile (`nodes.cluster-api-provider-aws.sigs.k8s.io`):

```sh
# Create/update the IAM roles, Customer Managed Policies, EC2 Instance Profile,
# and Kubernetes AWSClusterRoleIdentity.
# Pass --create-vpc if the region has no default VPC or you want an isolated VPC:
endpoint-controller iam apply \
  --tuple="k0s + calico,bgp + aws,aws-vpc" \
  --cluster=site-aws \
  --region=us-west-2 \
  --cluster-role-identity=site-aws-identity \
  --source-identity=aws-static-creds \
  --kubeconfig="$KUBECONFIG"

# Tear down all owned IAM roles, policies, instance profile, and managed VPC/SG resources:
endpoint-controller iam cleanup \
  --tuple="k0s + calico,bgp + aws,aws-vpc" \
  --cluster=site-aws \
  --region=us-west-2 \
  --kubeconfig="$KUBECONFIG"
```

### How VPC, Subnet, and Security Group Selection Works (Zero NAT Gateway & Zero Public IPv4)

Because `AWSCluster` carries `cluster.x-k8s.io/managed-by: external`, **CAPA's
`AWSCluster` controller stands down and never creates an AWS NAT Gateway
($0.045/hr + per-GB) or Elastic Load Balancer**. Instead, `cloud-provisioning`
resolves networking automatically and supports **zero-public-IPv4 ($0.005/hr
per IP saved)** operation over AWS's free Internet Gateway (`igw-...`):

1. **VPC & Subnet (with Free IPv6 + Internet Gateway)**:
   - **Automatic discovery (default)**: When `AWSCluster.spec.network` is
     omitted, the controller queries EC2 `DescribeVpcs` and `DescribeSubnets`,
     selects a VPC tagged for the cluster (`--create-vpc`) or the region's
     default VPC (`isDefault: true`), prefers an IPv6-enabled subnet
     (`assignIpv6AddressOnCreation: true`) before falling back to an IPv4
     public subnet, and patches `AWSCluster.spec.network` (including VPC/subnet
     IPv6 CIDRs when present) and `AWSMachine.spec.subnet`.
   - **Avoiding both NAT Gateways and Public IPv4s**:
     - **Zero NAT Gateway & Zero Public IPv4 by default**: By default,
       `AWSMachineTemplate` / `AWSMachine` defaults to `publicIP: false` and
       `assignPrimaryIPv6: enabled`, dialing the worker's free globally routable
       IPv6 address (`[2600:...]:51820`) over the free Internet Gateway.
     - Running `endpoint-controller iam apply --create-vpc` creates a dedicated
       dual-stack VPC with an Amazon-provided `/56` IPv6 CIDR, a `/64` IPv6
       subnet (`assignIpv6AddressOnCreation: true`, `mapPublicIpOnLaunch: false`),
       and a free Internet Gateway (`::/0` and `0.0.0.0/0` routes — **never a
       NAT Gateway**).
     - Only set `publicIP: true` on a template when you explicitly need an IPv4
       address (for example, a `replicas: 1` IPv4 ingress/gateway node group).
   - **Explicit VPC / Subnet**: Set `spec.network.vpc.id` and
     `spec.network.subnets` on `AWSCluster` (or `spec.template.spec.subnet.id`
     on `AWSMachineTemplate`) to pin a specific existing VPC or subnet.
2. **Security Group (automatic creation & teardown when unused)**:
   - Because AWS Security Groups (`sg-...`) are VPC-scoped, a static default ID
     cannot exist across accounts or VPCs.
   - When `AWSMachineTemplate` omits `additionalSecurityGroups`, the controller
     automatically creates `cloud-provisioning-<cluster>` in the resolved VPC
     (tagged `cloud-provisioning.appmana.com/managed-by=cloud-provisioning`),
     authorizes inbound `UDP 51820` (`::/0` and `0.0.0.0/0` for WireGuard from
     on-premises) and all-protocol (`-1`) self-ingress (for node/pod mesh
     traffic), and attaches its `sg-...` ID to each `AWSMachine`.
   - When the last `ProvisionedNodeClaim` in the cluster is deleted (or a
     `ProvisionedNodeGroupClaim` scales to `0`), the controller automatically
     deletes the security group once the instances terminate.

## 3. Add Cloud Nodes (`ProvisionedNodeClaim` & `ProvisionedNodeGroupClaim`)

See [`examples/node-group.yaml`](examples/node-group.yaml) and
[`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml):

```yaml
# --- Less-Privileged Runtime Credentials ---
# 1. CAPA controller (in `capa-system`) uses `aws-capa-credentials` (or assumes
#    the role created by `endpoint-controller iam apply` via AWSClusterRoleIdentity).
# 2. endpoint-controller (in `cloud-provisioning`) uses `aws-controller-credentials`
#    bound to the controller policy (`docs/iam/controller.json`), which allows
#    EC2 VPC/subnet discovery and tag-scoped creation/deletion of the
#    `cloud-provisioning-<cluster>` security group.
apiVersion: v1
kind: Secret
metadata:
  name: aws-capa-credentials
  namespace: capa-system # CAPA always resolves secretRef in capa-system
type: Opaque
stringData:
  AccessKeyID: AKIAIOSFODNN7EXAMPLE
  SecretAccessKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSClusterStaticIdentity
metadata:
  name: site-aws-identity
spec:
  secretRef: aws-capa-credentials
  allowedNamespaces:
    list:
      - cloud-provisioning
---
# The admission webhook automatically stamps `cluster.x-k8s.io/managed-by: external`,
# fills `controlPlaneEndpoint` from the on-premises API VIP, and defaults `identityRef`.
# Omit `spec.network` to auto-discover the cluster/default VPC and public subnet,
# or set `spec.network.vpc.id` and `spec.network.subnets` to pin a specific VPC:
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSCluster
metadata:
  name: site-aws
  namespace: cloud-provisioning
spec:
  region: us-west-2
  # Optional explicit VPC & public subnet override:
  # network:
  #   vpc:
  #     id: vpc-0123456789abcdef0
  #   subnets:
  #     - id: subnet-0123456789abcdef0
  #       resourceID: subnet-0123456789abcdef0
  #       isPublic: true
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: site-aws
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    kind: AWSCluster
    name: site-aws
---
# --- Template 1: IPv6 Worker Template (Default — Zero NAT Gateway, Zero Public IPv4) ---
# - `publicIP: false` and `assignPrimaryIPv6: enabled` are the defaults when omitted:
#   workers use free global AWS IPv6 addresses over the VPC's free Internet Gateway.
# - Security group (`cloud-provisioning-site-aws`) is created in the VPC when the
#   first node is claimed and deleted automatically when the last node is removed.
# - `ami` defaults to Canonical's official Ubuntu 24.04 LTS AMI lookup (`099720109477`).
# - `subnet` is populated automatically from the discovered or configured VPC subnet.
# - `iamInstanceProfile` defaults to `nodes.cluster-api-provider-aws.sigs.k8s.io`.
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSMachineTemplate
metadata:
  name: aws-ubuntu-amd64
  namespace: cloud-provisioning
spec:
  template:
    metadata:
      labels:
        cloud-provisioning.appmana.com/os: linux
        cloud-provisioning.appmana.com/arch: amd64
        # "site" runs the on-prem CNI over WireGuard; "aws-vpc" runs native AWS VPC CNI:
        cloud-provisioning.appmana.com/cni: site
    spec:
      instanceType: t3.large
---
# --- Template 2: IPv4 Template (Opt-In When Public IPv4 Is Explicitly Needed) ---
# Set `publicIP: true` only when you need a public IPv4 address (e.g. for a 1-replica
# IPv4 HTTP/HTTPS ingress or gateway group):
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSMachineTemplate
metadata:
  name: aws-ubuntu-ipv4
  namespace: cloud-provisioning
spec:
  template:
    metadata:
      labels:
        cloud-provisioning.appmana.com/os: linux
        cloud-provisioning.appmana.com/arch: amd64
        cloud-provisioning.appmana.com/cni: site
    spec:
      instanceType: t3.small
      publicIP: true
---
# --- Group 1: IPv6 Worker Pool (ProvisionedNodeGroupClaim / pngc, replicas: 3) ---
# Maintains `spec.replicas` child ProvisionedNodeClaims (<group>-<uid-hash>-<ordinal>)
# with zero NAT Gateway and zero public IPv4 cost.
# Scaling up provisions new cloud nodes; scaling down cordons and drains the
# highest-ordinal Node via the Eviction API, withdraws its WireGuard peer, and
# terminates the cloud instance (and deletes the security group when scaled to 0).
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeGroupClaim
metadata:
  name: aws-worker-pool
  namespace: cloud-provisioning
spec:
  replicas: 3 # Scale with `kubectl scale pngc aws-worker-pool --replicas=5` or KEDA

  # workloadSelector (optional): label selector matching the workload Pods served
  # by this node group. Exposed as `.status.selector` on the Kubernetes `/scale`
  # subresource (`subresources.scale.labelSelectorPath`) for HPA / KEDA autoscalers.
  workloadSelector:
    matchLabels:
      app: batch-worker

  template:
    spec:
      infrastructureRef:
        kind: AWSMachineTemplate
        name: aws-ubuntu-amd64
---
# --- Group 2: Single IPv4 Gateway / Ingress Node (replicas: 1) ---
# Keeps a single public-IPv4 node (`publicIP: true`) when IPv4 connectivity/ingress
# is required, while the main worker pool (`aws-worker-pool`) scales on free IPv6:
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeGroupClaim
metadata:
  name: aws-ipv4-gateway
  namespace: cloud-provisioning
spec:
  replicas: 1
  template:
    spec:
      infrastructureRef:
        kind: AWSMachineTemplate
        name: aws-ubuntu-ipv4
```

### Multi-Cloud in the Same Cluster

Multiple CAPI providers can run side by side in the same namespace
(see [`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml)):

| Provider | Package | Specialization |
| --- | --- | --- |
| AWS (`AWSMachine`) | `controller/pkg/join/aws` | EC2 IMDSv2 identity, auto VPC/subnet discovery, auto SG lifecycle (`cloud-provisioning-<cluster>`), AMI/profile defaults, `aws-vpc` CNI |
| GCP (`GCPMachine`) | `controller/pkg/join/gcp` | GCE metadata identity, `credentialsRef` check, `publicIP`/`imageFamily`/`failureDomain` defaults, `startup-script` `cloud-init` bridge |
| Generic CAPI (`*Machine`) | `controller/pkg/join/generic` | Any `<Kind>Template` / `<Kind>Cluster` (e.g. CAPZ `AzureMachine`) |
| Labcontainers / Docker | `controller/pkg/join/labcontainers`, `controller/pkg/join/docker` | Simulated WAN VMs and containers for e2e tests |

## 4. Cluster Preflight & Remediation

```sh
endpoint-controller check --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG"
endpoint-controller fix   --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG" --manifests=./clusters/my-cluster
```

## Documentation

[AWS](docs/aws.md) · [GPU workers](docs/gpu-workers.md) · [Node groups](docs/node-groups.md) ·
[Windows](docs/windows.md) · [Tunnel scenarios](docs/tunneling-scenarios.md) ·
[Dual-stack Calico BGP](docs/dual-stack.md) · [Forks](docs/forks.md) ·
[Qualification](docs/deployment-qualification.md)
