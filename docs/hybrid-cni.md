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
CNI DaemonSets tolerate every taint, so the cloud worker taint does not keep
them off. A Tigera operator install is changed through its `Installation`
override. Site nodes carry no such label and still match. If another writer
removes the requirement more than three times in ten minutes the controller
stops restoring it and reports the conflict on new claims. A claim for an AWS
VPC CNI node creates no machine until every site network DaemonSet excludes
it; OVN-Kubernetes and networks with no DaemonSet (k3s embedded flannel) are
refused. Without a Calico agent on AWS nodes, Calico IPAM never allocates
there, so the site's pools need no node selector.
The AWS Linux `aws-node` DaemonSet must select only AWS Linux nodes. Windows
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

## Linux campaign manifests

Use `harness/e2e/aws/vpc_cni.py` to render the pinned AWS VPC CNI 1.23.2 Helm
chart for separate IPv4 and IPv6 campaigns. The renderer verifies the chart
archive digest, requires immutable CNI/init image references, validates site,
Service and VPC ranges, and restricts the installer to labelled Linux cloud
workers. It sets IPv6 consistently in the initializer and IPAM containers;
IPv6 uses prefix delegation and requires Nitro instances. IPv4 uses secondary
addresses. Site-bound IPv4 traffic is excluded from SNAT, while normal VPC
egress retains its upstream behavior.

```sh
python3 harness/e2e/aws/vpc_cni.py \
  --chart "$PINNED_CNI_CHART_ARCHIVE" --family ipv6 \
  --site-cidr "$SITE_NODE_CIDR4" --site-cidr "$SITE_POD_CIDR4" \
  --site-cidr "$SITE_NODE_CIDR6" --site-cidr "$SITE_POD_CIDR6" \
  --service-cidr "$SERVICE_CIDR4" --service-cidr "$SERVICE_CIDR6" \
  --vpc-cidr "$VPC_CIDR4" --vpc-cidr "$VPC_CIDR6" \
  --run-id "$ISOLATED_RUN_ID" \
  --image "$CNI_IMAGE_DIGEST" --init-image "$CNI_INIT_IMAGE_DIGEST" \
  > "$PRIVATE_MANIFEST_PATH"
```

Installation requires the matching VPC/subnet family, native IPAM IAM policy,
positive Calico exclusion, and pre-registration cloud labels. The renderer tags
IPAM-created ENIs with the test run ID; the primary ENI also needs verified
ownership tags before the allocation observer can accept it. Observe actual
ENI allocations and packet sources after installation. Rendered manifests alone
are insufficient evidence for a mixed-CNI campaign.

For an isolated IPv4 run, `harness/e2e/aws/native_access.py --work-dir
"$PRIVATE_AWS_RUN" --runner "$ISOLATED_LAB_RUNNER" --calico-site` configures
single-ENI IPAM permissions, the read-only allocation observer and a site SNAT
exclusion for the verified AWS subnet. Run it on the setup host; only derived
observer credentials enter the lab. The exclusion is a disabled Calico pool
with allocation and BGP export disabled. Existing pool overlap or foreign
ownership is rejected. Remove this run-owned pool after its workers and routes
are removed if retaining the lab for a different campaign.

The chart archive is published in [the AWS Helm repository](https://aws.github.io/eks-charts/).
The family and prefix-delegation contract is documented in the
[pinned VPC CNI release](https://github.com/aws/amazon-vpc-cni-k8s/blob/v1.23.2/README.md).

IPv4/IPv6 `awsrow` campaigns with nodes registered on the AWS VPC CNI class
(the `cloud-provisioning.appmana.com/cni` node label) also require
`native-vpc-routes.json` to pass. This checks host-originated and forwarded pod
routes between at least two workers against each worker's physical ENI. Every
measured worker must belong to the campaign VPC. Connectivity through WireGuard
does not satisfy this check, even when the workload matrix passes.
