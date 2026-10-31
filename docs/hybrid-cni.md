# Calico BGP on premises with AWS VPC CNI on EC2

Calico owns site networking; AWS VPC CNI owns EC2 pod allocation. Qualify the
complete mixed-CNI path before deployment; Calico-only evidence does not apply.

## CNI placement

| Nodes | Workload networking | Address ownership |
| --- | --- | --- |
| On-premises nodes | Calico with BGP; VXLAN and IP-in-IP disabled | Site Calico IPAM allocations |
| AWS Linux nodes | AWS VPC CNI | EC2 ENI secondary addresses or delegated prefixes |
| AWS Windows Server 2022/2025 nodes | AWS VPC CNI Windows plugins | AWS Windows IPAM allocations on the primary ENI |

Calico node agents and CNI installers must not run on AWS nodes. Installing
Calico on AWS, even as a fallback for an incomplete VPC CNI integration,
violates this target. The product enforces this (`siteCNI.exclusion`, on by
default): every site network DaemonSet (those mounting `/etc/cni/net.d` or
`/opt/cni/bin`, `calico-node-windows`, and any named in `siteCNI.daemonSets`)
gets the required node-affinity requirement
`cloud-provisioning.appmana.com/cni NotIn [aws-vpc]` in every required term.
In addition, non-`kube-system` DaemonSets that declare a wildcard taint
toleration (`operator: Exists` with an empty `key`, such as node-local OCI
registry mirrors like `spegel`) automatically receive
`cloud-provisioning.appmana.com/role NotIn [cloud-worker]` so LAN-only daemons
do not schedule onto WAN/cloud workers.
CNI DaemonSets tolerate every taint, so the cloud worker taint does not keep
them off. A Tigera operator install is changed through its `Installation`
override. When a DaemonSet or `Deployment` is managed by **Flux CD** (`HelmRelease`
or `Kustomization`) or **Argo CD** (`Application`), both `endpoint-controller`
and `platform-tuple fix` propagate the exclusion or rollout strategy patch
upstream to the owning GitOps custom resource (`HelmRelease.spec.postRenderers`
kustomize strategic-merge patch, `Kustomization.spec.patches`, or Argo CD
`Application.spec.ignoreDifferences`) so GitOps controllers enforce rather than
revert the remediation. Site nodes carry no such label and still match. If another writer
removes the requirement more than three times in ten minutes the controller
stops restoring it and reports the conflict on new claims. A claim for an AWS
VPC CNI node creates no machine until every site network DaemonSet excludes
it; OVN-Kubernetes and networks with no DaemonSet (k3s embedded flannel) are
refused. Without a Calico agent on AWS nodes, Calico IPAM never allocates
there, so the site's pools need no node selector.

For Calico BGP sites, the controller and `platform-tuple fix` also reconcile:
- **Calico MTU alignment (`calico-mtu`)**: Ensures `kube-system/calico-node`
  `install-cni` `CNI_NETWORK_CONFIG` `mtu`, `FELIX_IPINIPMTU`, `FELIX_VXLANMTU`,
  `FELIX_WIREGUARDMTU`, and (when present) `kube-system/calico-windows-config-actual`
  `CNI_MTU` / `veth_mtu` match the site/tunnel path budget, and clamps any live
  host `cali*` veth and container `eth0` interfaces in-place via `calico-node`
  exec without restarting running pods.
- **Strict IPAM block affinity (`calico-ipam`)**: Sets `spec.strictAffinity: true`
  and `spec.autoAllocateBlocks: true` on `IPAMConfig/default` (`crd.projectcalico.org/v1`)
  so unencapsulated BGP nodes never borrow single `/32` or `/128` pod addresses
  from another node's IPAM block.
- **Single-node / cloud-worker `hostPort` rollout safety (`cloud-worker-workloads`)**:
  Patches `NginxProxy` CRs (`gateway.nginx.org/v1alpha2`) and single-node or
  `cloud-worker` `Deployment`s that bind `hostPort`s to `spec.strategy.type: Recreate`
  (and propagates the patch to any owning Flux `Kustomization`, `HelmRelease`, or
  Argo CD `Application`).
The product applies the AWS Linux `aws-node` DaemonSet itself, scheduled only
on AWS VPC CNI Linux nodes (see [Linux AWS VPC CNI](#linux-aws-vpc-cni)). Windows
requires its native VPC CNI binaries/configuration and Windows IPAM controller;
the Linux DaemonSet is not evidence that Windows networking is configured.

Preserve existing AWS CNI configuration, ENI ownership and pod address management
when attaching an AWS worker. Do not overwrite it with the site CNI's files,
assign it a Calico workload block, or run Felix on it. Windows BGP/RRAS and
Windows Calico VXLAN are not part of this profile.

## Routing contract

The two address-management domains need bidirectional native pod routing.
On-premises BGP distributes the site pod CIDRs to the site routing boundary.
The inter-site transport carries native pod packets, with explicit routes back
to the site pod CIDRs in the AWS network. VPC pod addresses remain natively
reachable within their AWS network. The allocation observer publishes the
verified account/region/VPC identity and exact ENI destinations, bound to the
peer's current key. A worker omits those destinations from another worker's
tunnel only when both belong to that native network. Site and foreign-network
peers retain the provider routes; no cluster-wide or VPC-wide bypass is inferred.

The product must observe provider-owned per-node VPC allocations separately
from Calico IPAM. It must not infer AWS pod ownership from `Node.spec.podCIDR`
or assign the whole VPC prefix to every WireGuard peer. Routing, source
acceptance, return routes and withdrawal must use the same verified ownership.
Site, VPC and Service CIDRs must not overlap. Cross-site SNAT behavior must be
explicit; a passing NATed probe is not proof that original pod addresses work.

## Fresh VM site profile

`lab -rig vm -distro k0s -cni calico-site-bgp` builds the on-premises
component with k0s's bundled Calico. It uses `mode: bird`, `overlay: Never`,
and each node's Kubernetes InternalIP. Calico placement is left as k0s ships
it: keeping it off AWS nodes is the product's job, and the site stays the
deployment an operator has.

This profile permits verified site reuse and product installation for AWS
qualification. Reuse checks require native BGP.
Generic remote VM claims remain disabled because they need their own remote
CNI configuration. AWS VPC CNI qualification is still in progress. Use an
[isolated VM runner](../harness/e2e/runner/README.md) alongside a retained lab.
Changing an existing overlay cluster to this profile is not a supported
migration procedure.

## Control-plane dependency

The retained lab uses an on-premises k0s control plane. AWS documents its
standard Windows VPC CNI setup with an EKS-managed VPC resource controller.
Keeping the k0s control plane therefore requires qualifying the Windows IPAM
controller/webhook integration as well as the CNI binaries; changing Calico to
BGP does not provide those components.

AWS's Windows secondary-IP workflow allocates ENI addresses, advertises
`vpc.amazonaws.com/PrivateIPv4Address` capacity, mutates pod resource requests,
and supplies the allocated address through a pod annotation consumed by the
Windows plugin. Verify that complete lifecycle, including cooldown and release.
Do not infer support for EKS-only ENI trunking from support for secondary-IP
allocation.

The inspected upstream controller also reads the EC2 region and instance
identity document from IMDS at startup, even when a region flag is supplied.
A self-hosted deployment therefore needs suitable AWS-side placement and
credentials, or an explicitly reviewed upstream integration change. A Deployment
on the on-premises control plane alone does not satisfy this dependency.

## Required evidence

1. Record exact Node UIDs, provider/ENI bindings, CNI binaries/configuration and
   pod address ownership. No AWS node may run a Calico node agent or installer.
2. Require on-premises Calico BGP sessions to be Established and the intended
   prefixes to be present in routing tables; `vxlanMode: Never` alone is not a
   BGP test.
3. Verify ordinary Windows 2022/2025 pods receive real VPC addresses and reach
   one another directly through native AWS networking.
4. Run Linux/Windows directed pod, Service, DNS, large TCP and UDP matrices
   between the site and AWS, checking both directions and original identities.
5. Repeat pod creation, worker replacement, gateway changes and reboot tests,
   retaining packet losses and proving address/route withdrawal and reuse.

## Sources

- [AWS Windows networking](https://docs.aws.amazon.com/eks/latest/best-practices/windows-networking.html)
- [AWS Windows secondary IPv4 workflow](https://github.com/aws/amazon-vpc-resource-controller-k8s/blob/master/docs/windows/secondary_ip_mode_workflow.md)
- [AWS Windows VPC CNI plugins](https://github.com/aws/amazon-vpc-cni-plugins)
- [Calico BGP peering](https://docs.tigera.io/calico/latest/networking/configuring/bgp)
- [AWS hybrid network routing requirements](https://docs.aws.amazon.com/eks/latest/userguide/hybrid-nodes-networking.html)

## Linux AWS VPC CNI

The controller applies `aws-node` (DaemonSet and `amazon-vpc-cni` ConfigMap in
`kube-system`) from the pinned AWS VPC CNI 1.23.2 chart, with the server-side
field manager `cloud-provisioning`, and keeps it current. An `aws-node` already
installed by hand is adopted. Its configuration comes from the cluster, not
values:

- Placement: Linux nodes registered with `cloud-provisioning.appmana.com/cni:
  aws-vpc` and the cloud worker role.
- SNAT: kept off traffic to the site's pod ranges (Calico's enabled pools,
  Cilium's node ranges, or the site nodes' pod CIDRs), the Service ranges and
  each site node's address, which the site routes back to pods. Normal VPC
  egress keeps its upstream behaviour.
- Pod MTU: the tunnel's 1420 bytes, or the site's Calico pod MTU where the
  site has set a smaller budget for its paths.
- One ENI per node (`MAX_ENI=1`): pods share the interface the tunnel uses.
- Family: `platforms.aws.vpcCNI.family`; IPv6 uses prefix delegation and needs
  Nitro instances, IPv4 uses secondary addresses.

`platforms.aws.vpcCNI.image` and `initImage` take digest-pinned mirrors of
the upstream images. Observe actual ENI allocations and packet sources after
installation; an applied DaemonSet alone is insufficient evidence for a
mixed-CNI campaign.

On a Calico site the controller makes each AWS subnet range cluster-internal
to Calico with a disabled pool (no allocation, no BGP export, no NAT), so site
pods reaching AWS pods keep their source. It refuses a range inside a pool the
site allocates from, and holds VPC CNI claims until the pools exist.

The operator gives the worker role the published
[VPC CNI node policy](aws.md) and the workers' security groups a rule
admitting each other; for an isolated run,
`harness/e2e/aws/native_access.py --work-dir "$PRIVATE_AWS_RUN"` does both.

The chart archive is published in [the AWS Helm repository](https://aws.github.io/eks-charts/).
The family and prefix-delegation contract is documented in the
[pinned VPC CNI release](https://github.com/aws/amazon-vpc-cni-k8s/blob/v1.23.2/README.md).

IPv4/IPv6 `awsrow` campaigns with nodes registered on the AWS VPC CNI class
(the `cloud-provisioning.appmana.com/cni` node label) also require
`native-vpc-routes.json` to pass. This checks host-originated and forwarded pod
routes between at least two workers against each worker's physical ENI. Every
measured worker must belong to the campaign VPC. Connectivity through WireGuard
does not satisfy this check, even when the workload matrix passes.

## Windows AWS VPC CNI

Windows VPC CNI nodes run AWS's native `vpc-bridge` plugin, baked into the
image, and AWS's Windows IPAM. The controller configures the plugin on each
node through the `aws-vpc-cni-windows` HostProcess DaemonSet in `kube-system`
(scheduled only on Windows AMD64 nodes registered with the AWS VPC CNI class).
Its initializer verifies the baked plugin against the pinned
[recipe](../controller/pkg/platform/aws/windowscni/cni-artifacts.json), the
patched build of [providers/aws-vpc-cni-plugins](../providers/aws-vpc-cni-plugins/README.md),
then writes `vpc-bridge.conf` once. Its settings come from the cluster:

- Service range: the cluster's IPv4 Service range.
- DNS: the `kube-system/kube-dns` Service address and the zone CoreDNS serves
  (`kube-system/coredns`, its `kubernetes` plugin).
- SNAT: kept off the same site ranges as Linux.
- Host routes: every site range but the Service range. The VPC's router has
  no route to the site, so site-bound pod traffic goes to the host, whose
  tunnel routes the site's pod prefixes (a native class's Windows tunnel
  service always routes native pod prefixes).
- Pod MTU: as Linux.

The plugin keeps the configuration a node was given: the initializer refuses
to replace a live one, so a site change reaches Windows nodes that join after
it, and an existing node's initializer reports the difference when it next
restarts. Replace the node to take the change.

Windows IPAM is AWS's VPC resource controller with the
[node-selector patch](../providers/aws-vpc-resource-controller/README.md): set
`platforms.aws.vpcCNI.windows.ipamImage` to the built image's digest (and
`pullSecrets` to kube-system Secrets if the registry needs them). The chart
installs its RBAC, CRDs, cert-manager certificate and webhooks; the controller
runs it on Linux VPC CNI workers for the one AWS cluster's VPC and enables
`enable-windows-ipam` once it is available. Without the image, Windows VPC CNI
claims are refused.

