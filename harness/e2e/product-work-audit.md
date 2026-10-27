# Harness product-work audit

Independent audit of the harness at f79e938 (2026-10-26) for places where the
harness does the product's job. Every C and D item, every assertion and every
/tmp write below must be resolved. Update the Status table as items close; an
item is closed only by a commit that moves the work into the product (or
documents a genuine operator prerequisite) and deletes the harness stand-in.

## Status

| Item | Status |
|---|---|
| C1 Calico placement | done: DaemonSet exclusion in product (95021de). IPPool node selectors are not needed: no Calico agent or CNI runs on a native node, so nothing allocates from site pools there |
| C2 Calico IP env delete | done: product runs `calico-node-tunnel` (IP=none, IP6=none) on tunnel-class Linux nodes and keeps the site calico-node off cloud workers; harness patch and flag deleted. OPEN part: Windows `calico-node-windows` still autodetects on Windows cloud workers (tracked with C17) |
| C3 kubeadm IP_AUTODETECTION can-reach | done: now governs site nodes only (remotes run the product agent with IP=none), so it is site configuration, not cover |
| C4 dual-stack cidr autodetect | done: same as C3; acceptance no longer requires an operator autodetection setting |
| C5 BGP mesh scoping (site_mesh.go) | done: product replaces the full mesh with BGPPeer cloud-provisioning-site-mesh, refuses tunnel claims while an operator peer reaches cloud nodes; site_mesh.go and -calico-site-mesh deleted; acceptance uses the product check |
| C6 control-plane recognition | done: product recognises k0s, MicroK8s and taint marks (`platform.IsControlPlane`); harness labelling deleted; placement rows and windows_join.py no longer read a harness label |
| C7 harness providerID patch | done: harness deleted the Node patch; `WaitRegistration` fails when the node did not register under the product's `--provider-id` |
| C8 microk8s join token renewal | done: chart runs `microk8s-token-agent` on a MicroK8s control plane; it mints `token|expiry` lines, renews at half life, prunes its own expired tokens, follows snap refreshes and keeps microk8s-provider-config current; harness minting deleted |
| C9 Linux VPC CNI install | done: controller applies aws-node (pinned chart 1.23.2 render, server-side, field manager cloud-provisioning, adopts a hand install) with SNAT exclusions from the site's exact pod, Service and node ranges, pod MTU min(tunnel, site Calico), one ENI; static RBAC in chart; vpc_cni.py deleted; awsrow requires the product to own aws-node |
| C10 VPC CNI IAM | done: published `docs/iam/vpc-cni-node-ipv4.json`/`-ipv6.json` keyed on CAPA's cluster tag, plus `controller.json`; harness attaches the published policy (no harness-built policy) |
| C11 SG self ingress | done: documented requirement; the product refuses a VPC CNI claim whose security groups do not admit each other (`join.ClassValidator`, DescribeSecurityGroups) before any machine exists; harness adds the rule as an operator |
| C12 disabled IPPool for provider ranges | done: on Calico sites the product creates a disabled `!all()` no-NAT no-export pool per AWSCluster subnet range, prunes unused ones, refuses ranges inside site pools and holds native claims until the pools exist; harness `calico_exclusion` deleted |
| C13 observer STS renewal | done: controller uses the SDK standard chain via generic `aws.credentials` (env keys or config/credentials files; role_arn+source_profile refreshes itself); the observer configures itself per Machine from its AWSCluster, EC2 subnets, STS and CAPA's ownership tag; `observer_credentials.py`, `native-pod-scope`, `--aws-pod-config` and `pkg/awscredentials` deleted; harness gives a run-scoped IAM user's keys as an operator would |
| C14 receipt TLS + reload | done: the chart issues the receipt certificate through cert-manager (self-signed root, 10-year CA, renewed serving certificate) unless a Secret is named; the controller reloads a renewed certificate without restarting and keeps the last good one; harness cert-manager objects deleted; aws.md claim corrected |
| C15 dialer URL renewal, --reuse-values undo | done: artifact sources may be s3://bucket/key, which the controller signs with aws.credentials at each bootstrap render for the join token's lifetime; nothing renews URLs, so a Helm upgrade cannot revert one; `controller-artifacts.json` published; renew_artifact.py deleted |
| C16 GatewayRequest built by harness | open |
| C17 Windows VPC CNI + IPAM | open |
| D1 k0s mixed-OS egress workaround | open |
| D2 kubeadm image prerequisites | open |
| D3 LabCluster status | done (54ef1cd) |
| D4 static identity renewal | open |
| D5 windows-publisher image release | open |
| D6 nodegroup CRD | open |
| D7 forks | open |
| Assertions list | CAPI association (C7), control-plane placement (C6), kubeadm Calico (C3) and CalicoNodesReady (C5, now measures the product agent on remotes) closed; rest open |
| /tmp writes | open |
| IAM docs key ownership on the harness's `cloud-provisioning-test` tag and `RUN_ID` (found while closing C13) | open: product policies should key on CAPA's cluster tag and named resources |
| `discover.PodCIDRs` widens node pod CIDRs to a guessed /16 (found while closing C9) | done: it and `NodeAddressRanges` (a guessed /24) had no callers and were deleted; the VPC CNI reads exact ranges |
| Mechanical write guard (harness kube/AWS writes refused outside an allowlist) | open |

## Found while closing items

- `scripts/coverage.sh` never set `KUBEBUILDER_ASSETS`, so every envtest skipped; once enabled, an RBAC envtest and an observer race surfaced. Fixed: the script uses the newest installed envtest binaries and starts Go data afresh per run.
- `observe/survivor_native.py` lost other hosts' evidence when one host failed first (`Executor.map` cancels pending work). Fixed with `each_host` and a regression test.

## Report (verbatim)

I audited HEAD (f79e938) across all 493 tracked files under `harness/`, `scripts/`, `.github/`, plus `images/` and `providers/`. Line numbers refer to HEAD.

Another session is changing the working tree as I write. It has uncommitted edits to `calico_config.go`, `bgp_dualstack.go`, `k0s.go`, `cluster.go`, `cmd/lab/main.go` and `cmd/awsrow/*`, a new `controller/pkg/cni/site/`, and a new chart value `siteCNI.exclusion`. Those edits are taking the Calico placement patch (row C1) out of the harness and into the product, so C1 is in flux.

Of the "known" items, two are not C: `aws/install.py` (installing CAPA is an operator prerequisite) and the static identity in `aws/identity.py`. I classed both as A, and put the hourly renewal behind `identity.py` in D. The rest of the known list is confirmed. Everything from C5 down is beyond the known list.

## C: product work the harness is papering over

| # | file:line | What it writes | Why a real deployment needs it | Who should own it; does the product do it? |
|---|---|---|---|---|
| C1 | `cluster/k0s/calico_config.go:31-52`, `:66-73` (kubelet `--node-labels cni=calico-site`, used at `k0s.go:95,102`); `bgp_dualstack.go:92-103,121-126` | Patches the k0s ClusterConfig so `calico-node` (agent and CNI installer) runs only on site-labelled nodes; sets `CALICO_IPV4POOL_NODE_SELECTOR` and `CALICO_IPV6POOL_NODE_SELECTOR` | Keeps Calico off nodes whose network the AWS VPC CNI owns | HEAD: product does not do it. The working tree is adding `controller/pkg/cni/site` (DaemonSet node-affinity exclusion). That package has no IPPool selector handling, while the edit also drops the harness pool selectors. |
| C2 | `calico_config.go:33-35` (managed-addresses mode) | Strategic patch deleting `calico-node`'s `IP` env var | Otherwise Calico's autodetection overwrites the product's `projectcalico.org/IPv4Address` annotation (written at `endpoint-controller/main.go:1280`) | `pkg/cni` or the chart. Product writes the annotation but nothing makes it stick. Documented only as a manual step (windows-gateway.md "Calico address ownership", dual-stack.md:79). |
| C3 | `network/calico/calico.go:61-62` | `kubectl set env ds/calico-node IP_AUTODETECTION_METHOD=can-reach=&lt;API&gt;` | Same purpose as C2, for the kubeadm/Calico rows | **Duplicate; hides whether the product's version works.** can-reach converges on the tunnel address by itself, so the row passes even if the product's annotation re-assertion is removed. |
| C4 | `bgp_dualstack.go:37-39` | `IP_AUTODETECTION_METHOD` / `IP6_AUTODETECTION_METHOD = cidr=&lt;site ranges&gt;` | Same class as C2 (documented at dual-stack.md:79) | `pkg/cni` or docs. Product does not do it. |
| C5 | `cluster/k0s/site_mesh.go:24-38`, applied at `cmd/lab/main.go:391` (`-calico-site-mesh`) | `BGPConfiguration default nodeToNodeMeshEnabled=false` plus `BGPPeer` limited to `!has(cloud-provisioning.appmana.com/role)` | Without it, remote `calico-node` pods are never Ready and every Calico rollout stalls | Controller (transit/cni). Product does not do it. Documented as an operator step (dual-stack.md:80) and checked by `acceptance/preconditions.go:462`. |
| C6 | `cluster/k0s/k0s.go:305-314` (called at :118 and :282); `cluster/microk8s/microk8s.go:113-117` | `kubectl label node &lt;cp&gt; node-role.kubernetes.io/control-plane=` | Product excludes control planes only by this label (`endpoint-controller/main.go:122,719,791,1140`). k0s sets `node.k0sproject.io/role=control-plane` instead (confirmed in k0s source, `cmd/controller/controller.go:727`) and never this label. On a stock k0s site, empty `tunnel.endpoints` would select the controller+worker members, and the `control-plane` placement selector would match nothing. | endpoint-controller should also recognise the k0s role label or the control-plane taint. Not documented. |
| C7 | `provider/provider.go:344-357` (`SetNodeProviderID`), `:365-402` (`AdoptNodes`), called at `cmd/lab/main.go:486` | Patches `Node.spec.providerID` when it is empty | CAPI and the product match Machine to Node on this field | **Duplicate; hides whether the product's version works.** The product already sets it through `--provider-id` (`join-patterns/_shared.tmpl:175-189`, fed by `pkg/join/labcontainers`). If that broke, the harness would fill the field and both CAPI modes would still pass. AWS rows do not do this, so they are clean. |
| C8 | `cluster/microk8s/microk8s.go:120-142` | Runs `microk8s add-node --token-ttl 14400` and writes Secret `microk8s-provider-config` with a 4h `expiresAt` | The product (`pkg/join/microk8s`) only reads it and refuses to render once it is within 5 minutes of expiry | Missing in `pkg/join/microk8s`. Documented as a manual step (`join-patterns/README.md:94-105`); nothing renews it. |
| C9 | `aws/vpc_cni.py:43-67` | AWS VPC CNI values: nodeSelector on the product's labels, `AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS` (site and Service ranges), `POD_MTU`, single ENI | Installs the cloud-node CNI and its SNAT exclusions | Missing (chart, or `pkg/cni/awspod`). Only `examples/aws-vpc-cni-coexistence-values.yaml` covers it. |
| C10 | `aws/native_access.py:97` | `iam put-role-policy cldt-native-cni` on the worker node role (Assign/Unassign IPs, CreateTags) | IAM the VPC CNI needs for IPAM | `docs/iam`. Not in any `docs/iam/*.json`, so undocumented. |
| C11 | `aws/native_access.py:102-104` | Self-referencing security-group ingress, all protocols | Native VPC pod-to-pod traffic. `prepare_campaign.py:26` also relies on it for receive ports 51821-51840. | docs. Undocumented. |
| C12 | `aws/native_access.py:51-57`, `:114-126` | Disabled Calico `IPPool` for the AWS subnet (`!all()`, no BGP export, no NAT) | Stops the site's Calico from SNATing traffic bound for VPC pods | `pkg/cni/awspod` already knows the pod ranges. Product does not do it. Documented in hybrid-cni.md:136. |
| C13 | `aws/observer_credentials.py:68-92` (hourly loop with `--duration`), also run from `prepare_campaign.py:158` | Creates or replaces Secret `native-pod-observer-file` with 1h STS credentials | `awsPodObserver.credentialsFileSecret` and `pkg/awscredentials/file.go` only read a file that something else must rotate | Missing (web identity/IRSA or role assumption in `pkg/awscredentials`). |
| C14 | `aws/prepare_campaign.py:32-52`, `:160-163` | cert-manager Issuer, CA Certificate and serving Certificate for the `...-receipts` Service, set as `tunnel.receiptTLSSecret` | The chart requires an existing TLS Secret, and values.yaml says "Restart on certificate rotation" | Chart (template a Certificate) plus controller (reload). aws.md:751 says the certificate "renews without manual rotation", but the controller would keep the old certificate until restarted. No row runs long enough to see it. |
| C15 | `aws/assets.py:42-49`; `aws/renew_artifact.py:75-80` | 4h presigned dialer URLs, renewed by `kubectl patch` on the controller Deployment's arguments, bypassing Helm | `pkg/join/download_expiry.go` only rejects URLs that have already expired | Missing (chart/controller). Also, `awsrow/main.go:332`'s `helm upgrade --reuse-values` rebuilds the Deployment from the old Helm values, silently undoing a renewal. |
| C16 | `aws/gateway_request.py:11-36`, `:40-51`, `:110`; driven by `windows_lifecycle.py:158` | Builds the GatewayRequest (Machine/Node UIDs, providerID, ENI, networkID, subnet, address, underlay) and creates the labelled ConfigMap | Needed for every Windows worker and every replacement | `pkg/attachment` (it already holds AWS credentials and validates these identities) should derive it. Today it only consumes it. Documented as manual (windows-gateway.md:330-345). |
| C17 | `images/aws/windows/` (`kustomization.yaml:14` `settings.json={}`, `cni-daemonset.yaml`, `configure-cni.ps1:86-122`, `prepare-cni.ps1:71-73` machine `KUBECONFIG`); `providers/aws-vpc-resource-controller/deploy/*`; `publisher.py` components `aws-windows-ipam` and `calico-node-windows` | Windows VPC CNI installer writing `vpc-bridge.conf` (SNAT exclusions, Service CIDR, DNS, host routes, MTU); Windows IPAM controller and webhooks | Without these, Windows pods on AWS have no network | Missing. Product does none of it. |

## D: unclear, and what would settle it

| # | file:line | What it writes | What would settle it |
|---|---|---|---|
| D1 | `cluster/k0s/egress_install.go:20-81` (`--k0s-linux-egress`); `cmd/k0segress` | Adds the `cldt-konnectivity-default-route-linux` DaemonSet | It works around a k0s bug (mixed-OS API→webhook egress). The fix belongs in k0s (`providers/k0s/native-linux-egress.patch`), not here. Decide whether the product claims support for unpatched mixed-OS k0s. |
| D2 | `cluster/kubeadm/stack.go:69`, `:118-135`, baked by `cmd/nodeimage/main.go:112` | CNI plugins, `br_netfilter`, `ip_forward` and swapoff in the kubeadm worker image | Only the k0s pattern reads the chart's `cniPlugins` (`k0s-worker.cloud-config.tmpl:57-62`). Either document these as kubeadm image requirements (then A) or make the kubeadm pattern use `cniPlugins` (then C). |
| D3 | `provider/provider.go:439-460`, called at `claim/claim.go:115` | Sets `LabCluster` endpoint, `status.ready` and `status.initialization.provisioned` | This is the lab provider's own job (B). But VM rows never annotate the LabCluster as external, so the product's external-readiness path (`pkg/claim/reconciler.go:487-540`) is never run. That path sets only `status.ready`, while the harness comment at `provider.go:316-324` says CAPI v1.11 reads only `initialization.provisioned` for v1beta2-contract providers. Settle by annotating the LabCluster `managed-by: external` and dropping the harness status write. |
| D4 | `aws/session.py:23` plus `aws/identity.py:15-16` | 1h assumed-role session written into the `AWSClusterStaticIdentity` Secret, rerun every hour | aws.md:445 tells operators to "renew before expiration", but nothing ships a renewer. Should the docs recommend `AWSClusterRoleIdentity`/IRSA instead? |
| D5 | `aws/publisher.py` component `windows-publisher` (1h pull Secret) | Private ECR image for `windowsPublisher.image` | CI (`build-controller.yml:26`) publishes only endpoint-controller and dialer. Is the Windows publisher image meant to be a released artifact? |
| D6 | Not harness code: `docs/node-groups.md:13` | Applies the group CRD from `controller/pkg/nodegroup/testdata` | The CRD is not in `charts/.../crds`. Fine while experimental; C once that flag is supported. |
| D7 | `providers/*` (CAPA Windows bootstrap, Calico Windows MTU, VPC resource controller node selector, VPC CNI host routes, k0s patches) | Required forks of third-party components | Decide whether the product claims the configurations that need them. |

## Assertions that pass because the harness supplied the feature
- **CAPI association** (`kube/association.go:14-45`, both `capi-mode`s) passes on a providerID the harness wrote (C7).
- **The `control-plane` placement and control-plane exclusion** pass only because of the harness label (C6). Lab rows always pass an explicit selector, so the product's default exclusion is never exercised.
- **The kubeadm/Calico matrix** still passes with the product's Calico address re-assertion removed (C3).
- **`requireManagement` → `CalicoNodesReady`** on remotes (`cmd/lab/main.go` ~620; `awsrow`) measures the harness BGP mesh (C5) and Calico placement (C1).
- **AWS `VerifySource` matrix** (`awsrow/main.go:410`) passes only because SNAT is excluded by C9, C12 and C17.
- **"No Calico on AWS nodes"** in `awsrow/native_cni.go` is satisfied by C1 at HEAD.
- **Gateway `Ready`** (`windows_join.py --require-gateway`, `observe/removal.py:54`) is checked against a request the harness built (C16).
- **`awsroutecheck`** runs the product's route code on a harness file journal (`cmd/awsroutecheck/main.go:35-75`), not the product's store.
- **`cleanup.py`** deletes the security group, route table and VPC wholesale, so leaked product AWS gateway state would never be reported.

## Tests that read harness-written values as product evidence
- `kube.WaitMachineAssociation` compares Machine and Node providerID; the Node value may have come from C7.
- `aws/windows_join.py:135` derives the site kubelet version from nodes with `-l node-role.kubernetes.io/control-plane`, a label the harness wrote on k0s sites.
- `docs/aws.md:751`'s claim that the receipt certificate renews without manual rotation rests on cert-manager objects the harness created (C14).

## Writes to /tmp
- **This host:**
  - `cmd/lab/main.go:187` and `cmd/vmprovider/main.go:93` (lock file)
  - `rig/aws/input.go:29`
  - `cmd/acceptance/main.go:190`
  - `aws/vpc_cni.py:87`
  - `aws/publisher.py:94,115` (Docker config holding the ECR token)
  - `rig/vm/image/build.sh:12`
- **Documented commands:**
  - `docs/windows.md:369-557` (`/tmp/awsnode`, `/tmp/tunnelprobe.exe`, `/tmp/windows-tunnel.exe`, `/tmp/cldt-windows-*`)
  - `docs/aws.md:487`
  - `providers/capa/README.md:6-8`
- **CI:** `.github/workflows/publish-chart.yml:36-37`.
- **Bastion and guests:**
  - `install/install.go:200,258-265` (`/tmp/chart`, `/tmp/chartsrc`); default chart path in `awsrow/main.go:44` and `prepare_campaign.py:127`
  - `cluster/cniplugins.go:50-55`
  - `cluster/kubeadm/kubeadm.go:300`
  - `cluster/rke2/rke2.go:160`
  - `check/probe.go:42,49`
  - `rig/vm/vm.go:34,39`
  - `aws/image-prepare.sh:15-23`
  - `safety/k0s-mtu-native.py:106`
- **`/var/tmp` and Windows temp:**
  - `safety/network-guard-native.sh:12`
  - `safety/k0s-mtu-native.py:72,101`
  - `images/aws/windows/prepare-bootstrap.ps1:11,15`
  - `rig/aws/commands.go:101`

## A and B (about 18 A groups and 45 B groups, counted by mutation site)
- **A, operator input:**
  - Helm install/upgrade with documented values (`install/install.go:197-213`, `placement.go:67`, `awsrow/main.go:332`, `prepare_campaign.py:169`)
  - cert-manager and CAPI (`install/capi.go:18`) and CAPA (`aws/install.py:26`)
  - Lab and AWS Cluster, template, claim and ImportedControlPlane objects (`claim/claim.go:53-107`, `aws/claim.py:85-110`), claim deletion (`claim/delete.go:63`)
  - Credentials (`aws/identity.py`), IAM rendered from `docs/iam` (`bootstrap.py:91-94`, `bake.py:280,308`)
  - AMI recipes (`bake.py`, `image-prepare.sh`, `images/linux/*`, `windows_image.py`), the kubeadm node image
  - Dialer binary hosting (`assets.py`), `native-pod-scope` ConfigMap (`native_access.py:111`), the qualified apply (`deploy-qualified`)
- **B, lab, fault injection, observation or cleanup:**
  - Topology addressing, routing and iptables, identity netplan (`bringup/*`), VM rig seeds, keys and resets
  - Site builders for k0s, k3s, RKE2, MicroK8s and kubeadm, the kubeadm API forwarder, the kube-proxy conntrack settings
  - Native-profile pool patch and restart (`calico.go:117,127`)
  - The lab infrastructure provider (LabMachine providerID, addresses, ready state and finalizers; slot ConfigMaps)
  - Air-gapped image carrying (`LoadOnto`, `LoadImages`, `-preload-dialer`), the binary origin server
  - crictl and UDP prober installs, probe namespaces
  - Outages and replumbing, `windows_token.py:108` acknowledgement removal
  - Windows service and tunnel component VMs and firewall rules, probe instances, network-guard and MTU rehearsals, netns tests
  - Cleanup and verification, harness sessions, `scripts/aws/bootstrap-harness-iam.sh`, CI workflows, KEDA fixtures
