# cloud-provisioning

[![images](https://img.shields.io/badge/ghcr.io-appmana%2Fcloud--provisioning-blue)](https://github.com/orgs/AppMana/packages?repo_name=cloud-provisioning)

Join a public cloud node to an on-premises, firewalled cluster over a
WireGuard tunnel, so the cluster can run internet-facing workloads such
as an ingress gateway on a node the internet can reach while the
control plane stays private.

You describe the machine with an ordinary
[Cluster API](https://cluster-api.sigs.k8s.io/) machine template, then
commit one claim naming it:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSMachineTemplate
metadata:
  name: public-worker
  namespace: cloud-provisioning
spec:
  template:
    spec:
      instanceType: t3.micro
      ami:
        id: ami-0123456789abcdef0
      subnet:
        id: subnet-0123456789abcdef0
      additionalSecurityGroups:
        - id: sg-0123456789abcdef0
      publicIP: true
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: public-worker
  namespace: cloud-provisioning
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSMachineTemplate
    name: public-worker
  clusterName: my-cluster
```

The template carries the whole machine, including the security group
admitting UDP 51820, so a provisioned node is reachable because its
template says so. `kubectl delete provisionednodeclaim public-worker`
destroys the instance and removes the node.

The node joins as a normal worker, labelled
`cloud-provisioning.appmana.com/role=cloud-worker` and tainted
`cloud-provisioning.appmana.com/internet-facing:NoSchedule`. Pod
networking is the cluster's own CNI carried over the tunnel, with no
overlay of its own.

A complete example, including the Cluster API objects to apply once per
cluster, is in [examples/aws.yaml](examples/aws.yaml).

## Install

```bash
helm install cloud-provisioning oci://ghcr.io/appmana/charts/cloud-provisioning \
  --namespace cloud-provisioning --create-namespace \
  --set tunnel.endpoints=kubernetes.io/hostname=worker-1
```

Nothing else is required. The API server address, the pod ranges and
the service ranges are read from the cluster at startup; set the
matching values only to override what was discovered.

Or from a checkout: `helm install cloud-provisioning ./charts/cloud-provisioning -f values.yaml`
(run `helm dependency update ./charts/cloud-provisioning` first if you
enable the Cluster API subchart).

Values worth knowing:

| Value | Why |
|---|---|
| `tunnel.endpoints` | Node selector for which of this cluster's nodes terminate tunnels. Empty selects every Linux worker; control planes are excluded unless named. Only selected nodes get a tunnel interface. |
| `dialerBinary.<arch>.url` / `.sha256` | First-boot binary for the remote node, which has no image puller before it joins. A URL without its digest is refused. |
| `cniPlugins.<arch>.url` / `.sha256` | Needed when the cluster's CNI configuration chains plugins a stock cloud image lacks, such as `bandwidth`. |
| `cluster-api-operator.enabled` | Optional subchart. A separate release is preferable: coupling the lifecycles lets a failed upgrade here delete the provider namespaces. |

## Prerequisites

- [Cluster API](https://cluster-api.sigs.k8s.io/user/quick-start),
  core plus an infrastructure provider.
- [cert-manager](https://cert-manager.io/docs/installation/), which
  Cluster API requires.

Cluster API's own controllers need `deployment.nodeSelector` on the
Provider resources if the cluster has Windows nodes, since the upstream
manifests carry no OS selector.

## A worked example

Suppose you run a k0s cluster on your own hardware, with Calico for
networking, and you want a worker in AWS. Your control planes have
private addresses and nothing from the internet can reach them.

You install the chart, selecting one of your workers to terminate
tunnels. Then you commit the two objects above and watch:

1. The controller reads what it needs from the cluster: the API server
   address from the endpoints of the `kubernetes` service, the service
   range from the allocator, and the pod range from Calico's IP pool.
   None of it is configuration you maintain.
2. The claim becomes a Cluster API `Machine` and an `AWSMachine` built
   from your template. The kind comes from the template's kind with the
   `Template` suffix removed.
3. The controller generates a WireGuard identity for the new node and
   renders its cloud-init: the peer list, the join token, and a
   digest-pinned dialer binary.
4. The AWS provider launches the instance. It boots, reads its own
   architecture, fetches the matching binary, checks the digest, and
   brings up the tunnel. Your selected worker dials out to it; the
   instance only listens, because it is the side with a public address.
5. The instance waits until the API server answers through the tunnel,
   then runs `k0s worker` with its token. This is the only path it has,
   so it cannot join any other way.
6. The node registers, carrying the cloud-worker label and the
   internet-facing taint so ordinary workloads stay off it.
7. The controller tells Calico which address to peer on for that node:
   its tunnel address. The instance's own address belongs to AWS and
   means nothing to your cluster, and the tunnel address is the one
   every tunnel endpoint can reach.
8. Calico establishes a session across the tunnel and distributes pod
   routes. Pods on the new node reach pods and services at home, and
   the reverse, over the ordinary CNI.

Your ingress gateway, tolerating the internet-facing taint, schedules
onto the new node and serves the internet from an address the internet
can reach. Your control plane never leaves the private network.

## The tunnel

The tunnel carries reachability between nodes and nothing else. The
dialer installs host routes only, `/32` or `/128`, to the addresses of
peer nodes. Anything broader is refused when the peer list is parsed.
Pod and service ranges go into WireGuard's cryptokey accept list, which
decides which key may encrypt a packet and is not a source of routes.
Routing to pods stays the CNI's job, over the sessions those host
routes make possible.

```mermaid
graph LR
  subgraph onprem["your cluster, private network"]
    cp["control plane<br/>10.0.0.10"]
    w1["worker-1<br/>tunnel endpoint<br/>10.0.0.21"]
    w2["worker-2<br/>10.0.0.22"]
  end
  subgraph aws["AWS"]
    r1["remote node<br/>public address<br/>tunnel 10.100.0.128"]
  end
  cp --- w1
  cp --- w2
  w1 --- w2
  w1 === r1
  linkStyle 3 stroke-width:3px
```

The thick line is the WireGuard tunnel. `worker-1` dials out to the
remote node, which has a public address and listens. Nothing dials
inward to your network, and no other node grows a tunnel interface.

Three rules follow from carrying only host routes:

- A peer's own endpoint is never routed through the tunnel. The
  encrypted packet's outer destination would match that route and it
  would encapsulate itself indefinitely.
- Routes are pruned as well as added, so a route that becomes wrong
  stops black-holing traffic.
- A peer's routes are installed only once it is reachable, meaning its
  endpoint is known or a handshake has been seen. A route to an
  unreachable peer is a black hole.

## A mesh of tunnels

More than one node can terminate tunnels, and more than one remote node
can join. Every selected node dials every remote node, and remote nodes
dial each other, because two nodes in different clouds share no private
network and cannot reach each other any other way.

```mermaid
graph LR
  subgraph onprem["your cluster"]
    w1["worker-1"]
    w2["worker-2"]
  end
  subgraph c1["cloud A"]
    r1["remote-1"]
  end
  subgraph c2["cloud B"]
    r2["remote-2"]
  end
  w1 === r1
  w1 === r2
  w2 === r1
  w2 === r2
  r1 === r2
```

Each node holds one WireGuard interface with one peer entry per
counterpart, so the mesh costs one interface per node rather than one
per pair. The interface is named from the identity of the peer list, so
it is the same on every member and never collides with a `wg0` or a
Tailscale interface the node already has.

Which of your nodes take part is the `tunnel.endpoints` selector. An
empty selector means every Linux worker; control planes are left out
unless you name them, so a tunnel cannot cost a control plane its
default route.

## Layout

```
controller/               endpoint-controller (claim, join and mesh) and the dialer
controller/pkg/join/      one package per specialization: k0s and kubeadm for
                          joining, aws and docker for fulfillment
controller/pkg/discover/  reads the cluster's own addresses and ranges
join-patterns/            cloud-init templates, one per join mechanism
charts/                   the Helm chart
examples/                 a complete set of manifests
harness/netns-routing/    single-NIC routing end to end for the dialer
harness/kind-e2e/         one claim becomes a real joined node over a real tunnel
harness/vm-single-nic/    real VM, real boot and reboot
```
