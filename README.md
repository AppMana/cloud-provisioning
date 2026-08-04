# cloud-provisioning

[![images](https://img.shields.io/badge/ghcr.io-appmana%2Fcloud--provisioning-blue)](https://github.com/orgs/AppMana/packages?repo_name=cloud-provisioning)

Join a public cloud node to an on-premises, firewalled cluster over a
WireGuard tunnel, so the cluster can run internet-facing workloads (an
ingress gateway, a VPN endpoint) on a node the internet can reach while
the control plane stays private.

You commit one resource:

```yaml
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: public-worker
  namespace: wg-dialer
spec:
  requests:            # resolved to the smallest satisfying instance type
    cpu: "2"
    memory: 2Gi
  # instanceType: t3.micro   # ...or name one exactly, instead of requests
  arch: amd64
  clusterName: my-cluster
```

The controller creates the CAPI `Machine` + provider machine, renders
the node's cloud-init (WireGuard identity, sha-pinned dialer binary,
join token), and manages both dialer DaemonSets. `kubectl delete
provisionednodeclaim public-worker` destroys the instance. The claim
goes in whichever namespace holds the CAPI `Cluster`.

The node joins as a normal worker, labelled
`cloud-provisioning.appmana.com/role=cloud-worker` and tainted
`cloud-provisioning.appmana.com/internet-facing:NoSchedule`. Pod
networking is Calico's ordinary BGP mesh (`overlay: Never`) carried
over the tunnel — no VXLAN.

## Install

```bash
kubectl -n wg-dialer create secret generic aws-credentials \
  --from-literal=AccessKeyID=... --from-literal=SecretAccessKey=...

helm install cloud-provisioning oci://ghcr.io/appmana/charts/cloud-provisioning --version 0.1.0 \
  --namespace wg-dialer --create-namespace \
  --set cluster.apiAddress=https://10.2.0.22:6443 \
  --set cluster.apiVIP=10.2.0.22 \
  --set cluster.podCIDRs=10.3.0.0/16 \
  --set cluster.serviceCIDRs=10.152.184.0/24 \
  --set cluster.nodeVIP4Prefix=10.2.0. \
  --set tunnel.endpoints=kubernetes.io/hostname=worker-1 \
  --set targetCluster.enabled=true --set targetCluster.name=my-cluster \
  --set provider.aws.credentialsSecret=aws-credentials \
  --set provider.aws.region=us-west-2 \
  --set provider.aws.subnetID=subnet-... \
  --set provider.aws.securityGroupIDs=sg-... \
  --set provider.aws.ami.amd64=ami-...
```

The credentials Secret is the only thing created outside the chart --
it is the one input that cannot go into git. Everything else,
including the CAPI `Cluster` pair the Machines hang off, is rendered
from values, and `helm uninstall` removes all of it.

Or from a checkout: `helm install cloud-provisioning ./charts/cloud-provisioning -f values.yaml`
(run `helm dependency update ./charts/cloud-provisioning` first if you
enable the Cluster API subchart).

Required values (the chart fails at render if missing): `cluster.apiAddress`,
`cluster.apiVIP`, `cluster.podCIDRs`, `cluster.serviceCIDRs`,
`cluster.nodeVIP4Prefix`.

Values worth knowing:

| Value | Why |
|---|---|
| `tunnel.endpoints` | Node selector for which local nodes terminate tunnels. Empty = every Linux worker; control planes are always excluded unless named. This is the blast radius. |
| `dialerBinary.<arch>.url` / `.sha256` | First-boot binary for the remote (it has no image puller before it joins). A URL without its sha is refused. |
| `cniPlugins.<arch>.url` / `.sha256` | Needed when the cluster's CNI config chains plugins a stock cloud image lacks (e.g. `bandwidth`). |
| `cluster-api-operator.enabled` | Optional subchart. Prefer a separate release: coupling the lifecycles lets a failed upgrade here delete the provider namespaces. |

Cluster API must be installed (core + an infrastructure provider). Its
own controllers need `deployment.nodeSelector` on the Provider CRs if
the cluster has Windows nodes — the upstream manifests carry no OS
selector.

## Prerequisites

- Cluster API installed (core + an infrastructure provider), and
  cert-manager, which it requires.
- The security group must allow inbound UDP 51820 from the cluster's
  egress address.

## How it works

1. **Claim** → the CAPI `Machine` + provider machine pair, instance type
   resolved from `requests` (smallest fit) or taken from `instanceType`.
2. **Bootstrap** → cloud-init brings up the tunnel and gates the join on
   the API being reachable *through* it. This is the only way the node
   can join, and the systemd unit is never disabled: it is the floor
   that keeps the node reachable.
3. **Adoption** → after joining, a DaemonSet feeds the same dialer a
   live peer list from an in-cluster Secret, so peer and CIDR changes
   reach a node whose userdata is immutable. It also installs its own
   binary onto the host, making the image the upgrade channel.
4. **Network** → Calico peers BGP across the tunnel and distributes pod
   routes. Set the cluster's Calico MTU to ~1420.

## The routing invariant

The tunnel provides **node-to-node reachability only**. The dialer's
kernel routes are host routes (`/32`, `/128`) to peer node addresses —
anything broader is refused at parse time. Pod and service CIDRs go
only into WireGuard's cryptokey accept-list (AllowedIPs), which is a
packet filter, not a route source. Routing to pods is Calico's job.

Three rules follow, each found by a live failure:

- Never route a peer's own endpoint through the tunnel — the encrypted
  packet's outer destination matches the same route and re-encapsulates
  forever (observed at line rate).
- Prune routes, don't only add them, or a route that becomes wrong
  keeps blackholing.
- Install a peer's routes only once it is reachable (endpoint known or
  handshake seen); a route to an unreachable peer is a blackhole.

Direction matters: internal nodes always dial out, the remote only
listens and learns their addresses by roaming.

## Layout

```
controller/            endpoint-controller (claim + join + mesh) and the dialer
controller/pkg/join/   one package per specialization: k0s, kubeadm (join);
                       aws, docker (fulfillment)
join-patterns/         cloud-init templates, one per join mechanism
charts/                the Helm chart
harness/netns-routing/ single-NIC routing e2e for the dialer
harness/kind-e2e/      one claim -> a real joined node over a real tunnel
harness/vm-single-nic/ real VM, real boot/reboot
```
