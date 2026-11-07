# cloud-provisioning

Add cloud worker nodes (AWS, GCP, Azure, etc.) to an on-premises Kubernetes
cluster over WireGuard by applying a single Kubernetes resource:
- **`ProvisionedNodeClaim`**: provisions and joins one cloud worker node.
- **`ProvisionedNodeGroupClaim`** (`pngc`): maintains a ReplicaSet of cloud
  worker nodes (`spec.replicas`), supporting `kubectl scale`, HPA, and KEDA.

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

    # Enable ProvisionedNodeGroupClaim (ReplicaSet of cloud nodes):
    experimentalNodeGroups: true

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

    # Optional: allow preflight checks to verify CAPI provider Secrets in their
    # manager namespaces (e.g. CAPA in capa-system, CAPG in capg-system):
    providerManagerNamespaces:
      - capa-system
      - capg-system

    # Optional: AWS credentials Secret in `cloud-provisioning` used by the
    # controller itself (for preflight SG checks, s3:// URL signing, and native
    # AWS VPC CNI pod IP observation). Separate from CAPA's Secret in `capa-system`.
    aws:
      credentials:
        secretName: "" # Secret with AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
```

## 2. Add Cloud Nodes (`ProvisionedNodeClaim` & `ProvisionedNodeGroupClaim`)

See [`examples/node-group.yaml`](examples/node-group.yaml) and
[`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml):

```yaml
# --- AWS Credentials & Permissions ---
# 1. CAPA controller (in `capa-system`) launches/terminates EC2 instances.
#    Its Secret MUST live in `capa-system` and is referenced by an
#    AWSClusterStaticIdentity (or AWSClusterRoleIdentity to assume an IAM role).
# 2. The EC2 worker's `iamInstanceProfile` needs only minimal node permissions
#    (or AmazonEKSWorkerNodePolicy / AmazonEC2ContainerRegistryReadOnly when
#    using native `aws-vpc` CNI). Set `aws.iamBootstrap.enabled: true` in the
#    HelmRelease to bootstrap the IAM roles/instance profile automatically.
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
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSCluster
metadata:
  name: site-aws
  namespace: cloud-provisioning
  annotations:
    cluster.x-k8s.io/managed-by: external # On-prem control plane is external to CAPI
spec:
  region: us-west-2
  identityRef:
    kind: AWSClusterStaticIdentity
    name: site-aws-identity
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: site-aws
  namespace: cloud-provisioning
  annotations:
    cluster.x-k8s.io/managed-by: external
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSCluster
    name: site-aws
---
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
      iamInstanceProfile: nodes.cluster-api-provider-aws.sigs.k8s.io
      ami:
        id: ami-0abcdef1234567890
      subnet:
        id: subnet-0123456789abcdef0
      additionalSecurityGroups:
        - id: sg-0123456789abcdef0 # Must allow inbound UDP 51820 from on-prem
      publicIP: true
---
# --- Option A: Single Cloud Worker Node (ProvisionedNodeClaim) ---
# Applying this resource launches the cloud VM, configures its WireGuard tunnel,
# and joins it to the on-premises cluster. Deleting it drains the node,
# tears down the VM, and removes its WireGuard peer.
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: aws-worker-1
  namespace: cloud-provisioning
spec:
  clusterName: site-aws
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSMachineTemplate
    name: aws-ubuntu-amd64
---
# --- Option B: ReplicaSet of Cloud Worker Nodes (ProvisionedNodeGroupClaim / pngc) ---
# Maintains `spec.replicas` child ProvisionedNodeClaims (<group>-<uid-hash>-<ordinal>).
# Scaling up provisions new cloud nodes; scaling down cordons and drains the
# highest-ordinal Node via the Eviction API, withdraws its WireGuard peer, and
# terminates the cloud instance. Each joined Node is labeled with:
#   cloud-provisioning.appmana.com/node-group-uid: <metadata.uid of this group>
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
      clusterName: site-aws
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: AWSMachineTemplate
        name: aws-ubuntu-amd64
```

### Multi-Cloud in the Same Cluster

Multiple CAPI providers can run side by side in the same namespace
(see [`examples/mixed-cloud-workers.yaml`](examples/mixed-cloud-workers.yaml)):

| Provider | Package | Specialization |
| --- | --- | --- |
| AWS (`AWSMachine`) | `controller/pkg/join/aws` | EC2 IMDSv2 identity, CAPA `capa-system` Secret & SG validation, optional `aws-vpc` CNI |
| GCP (`GCPMachine`) | `controller/pkg/join/gcp` | GCE metadata identity, `credentialsRef` check, `publicIP`/`imageFamily`/`failureDomain` defaults, `startup-script` `cloud-init` bridge |
| Generic CAPI (`*Machine`) | `controller/pkg/join/generic` | Any `<Kind>Template` / `<Kind>Cluster` (e.g. CAPZ `AzureMachine`) |
| Labcontainers / Docker | `controller/pkg/join/labcontainers`, `controller/pkg/join/docker` | Simulated WAN VMs and containers for e2e tests |

## 3. Cluster Preflight & Remediation

```sh
endpoint-controller check --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG"
endpoint-controller fix   --tuple="k0s + calico,bgp + aws" --kubeconfig="$KUBECONFIG" --manifests=./clusters/my-cluster
```

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

## Documentation

[AWS](docs/aws.md) · [GPU workers](docs/gpu-workers.md) · [Node groups](docs/node-groups.md) ·
[Windows](docs/windows.md) · [Tunnel scenarios](docs/tunneling-scenarios.md) ·
[Dual-stack Calico BGP](docs/dual-stack.md) · [Forks](docs/forks.md) ·
[Qualification](docs/deployment-qualification.md)
