# cloud-provisioning

[![images](https://img.shields.io/badge/ghcr.io-appmana%2Fcloud--provisioning-blue)](https://github.com/orgs/AppMana/packages?repo_name=cloud-provisioning)

Join a public cloud node to an on-premises, firewalled cluster over a
WireGuard tunnel, so the cluster can run internet-facing workloads such
as an ingress gateway on a node the internet can reach, while the
control plane stays private.

The node joins as an ordinary worker. It is labelled
cloud-provisioning.appmana.com/role=cloud-worker and tainted
cloud-provisioning.appmana.com/internet-facing:NoSchedule, so nothing
lands on it unless you say so. Pod networking is the cluster's own CNI
carried over the tunnel, with no second overlay.

## Deploying it

You need [Cluster API](https://cluster-api.sigs.k8s.io/user/quick-start),
core plus the infrastructure provider for your cloud, and
[cert-manager](https://cert-manager.io/docs/installation/), which
Cluster API requires. If the cluster has Windows nodes, set
deployment.nodeSelector on the Provider resources, because the upstream
manifests carry no OS selector of their own.

Install those and the chart once per cluster:

```bash
clusterctl init --infrastructure aws

helm install cloud-provisioning oci://ghcr.io/appmana/charts/cloud-provisioning \
  --namespace cloud-provisioning --create-namespace \
  --set tunnel.endpoints='kubernetes.io/hostname=worker-1'
```

Also once per cluster, apply the Cluster API objects that describe the
cluster and the cloud account. They are not part of this chart, which
owns only its own resources. A complete set is in
[examples/aws.yaml](examples/aws.yaml).

Then, for each node you want, a machine template and a claim naming it.
This pair is the only thing you repeat:

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
template says it is, not because someone opened a port afterwards.

Watch it arrive, then schedule something onto it:

```bash
kubectl -n cloud-provisioning get provisionednodeclaim -w
kubectl get nodes -l cloud-provisioning.appmana.com/role=cloud-worker
```

```yaml
tolerations:
  - key: cloud-provisioning.appmana.com/internet-facing
    operator: Exists
    effect: NoSchedule
nodeSelector:
  cloud-provisioning.appmana.com/role: cloud-worker
```

Deleting the claim destroys the instance and removes the node:

```bash
kubectl -n cloud-provisioning delete provisionednodeclaim public-worker
```

Beyond tunnel.endpoints there is little to configure, and deliberately
so. The API server address comes from the endpoints of the kubernetes
service, and the pod addressing comes from the network's own per-node
records, re-read every pass, so there is no second copy of any of it to
drift.

tunnel.endpoints says which of your nodes terminate tunnels. It takes a
label selector, a set such as kubernetes.io/hostname in (worker-1,
worker-2), or the word all, and empty means all. Control planes are left
out unless the selector names them, so a tunnel cannot cost a control
plane its default route, and nodes this operator provisioned are never
included, since they are the far end of a tunnel rather than one of your
ends of it.

A set contains commas, and helm reads a comma in --set as the separator
between values, so a set based selector has to be escaped or the release
fails to install:

```bash
--set-string tunnel.endpoints='kubernetes.io/hostname in (worker-1\,worker-2)'
```

The remaining values matter only in specific cases. A remote node has
no image puller before it joins, so dialerBinary gives it a first-boot
binary by URL and digest; a URL without its digest is refused. If the
cluster's CNI configuration chains plugins a stock cloud image lacks,
such as bandwidth, cniPlugins supplies them the same way. There is an
optional Cluster API subchart, but a separate release is better,
because coupling the lifecycles lets a failed upgrade here delete the
provider namespaces.

## What happens

Suppose you run a k0s cluster on your own hardware, with Calico for
networking, and you want a worker in AWS. Your control planes have
private addresses and nothing on the internet can reach them. You
install the chart, selecting one worker to terminate tunnels, and
commit the two objects above.

The controller reads what it needs from the cluster: the API server
address, and which pod blocks belong to which node from Calico's own
records. The claim becomes a Cluster API Machine and an AWSMachine
built from your template, the kind taken from the template's kind with
the Template suffix removed. It generates a WireGuard identity for the
new node and renders its cloud-init: the peer list, the join token, and
a digest-pinned dialer binary.

AWS launches the instance. It boots, reads its own architecture,
fetches the matching binary, checks the digest, and brings up the
tunnel. Your worker dials out to it; the instance only listens, because
it is the side with a public address. It waits until the API server
answers through the tunnel and then runs k0s worker with its token,
which is the only path it has, so it cannot join any other way.

The node registers with the cloud-worker label and the internet-facing
taint. The controller tells Calico which address to peer on for it: its
tunnel address, since the instance's own address belongs to AWS and
means nothing to your cluster. It does the same in reverse for the node
holding the tunnel, pinning it to the address its own neighbours reach
it by, so bringing up a tunnel cannot cost a node the network it
already had. Calico then establishes a session across the tunnel and
distributes pod routes.

Your ingress gateway, tolerating the taint, schedules onto the new node
and serves the internet from an address the internet can reach. Your
control plane never leaves the private network.

## The tunnel

The tunnel carries reachability between nodes and nothing else. The
dialer installs host routes only, a /32 or a /128 per peer address, and
anything broader is refused when the peer list is parsed.

WireGuard's cryptokey accept list decides which key may encrypt a
packet. It is not a source of routes. Each peer is permitted its own
node's addresses and the pod blocks that node owns, and nothing else,
because the list is a trie with one owner per prefix: a range shared
between two peers belongs to whichever was written last, and traffic
for it follows whichever that happened to be. No service range is
permitted anywhere, since a service address is translated to a backing
pod on the sending node before anything is routed, so a packet crossing
the tunnel is already addressed to a pod or to a node.

Routing to pods stays the network's job, over the sessions those host
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

The thick line is the tunnel. worker-1 dials out to the remote node,
which listens. Nothing dials inward to your network, and no other node
grows a tunnel interface.

Three rules follow from carrying only host routes. A peer's own
endpoint is never routed through the tunnel, because the encrypted
packet's outer destination would match that route and it would
encapsulate itself forever. Routes are pruned as well as added, so a
route that becomes wrong stops black-holing traffic. And a peer's
routes are installed only once it is reachable, meaning its endpoint is
known or a handshake has been seen, because a route to an unreachable
peer is a black hole.

More than one node can terminate tunnels, and more than one remote node
can join. Every selected node dials every remote node, and remote nodes
dial each other, since two nodes in different clouds share no private
network and have no other way to meet.

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

Each node holds one interface with one peer entry per counterpart, so
the mesh costs one interface per node rather than one per pair. The
interface is named from the identity of the peer list, so it is the
same on every member and never collides with a wg0 or a Tailscale
interface the node already has.

## Nodes that hold no tunnel

Most nodes hold no tunnel, and they still have to reach a remote node's
pods. They cannot manage it alone: a remote node is known by its tunnel
address, and a node with no tunnel has no route to that, so it cannot
even open a session to the remote to learn anything from it.

So a node that does hold a tunnel tells the others. It speaks BGP to
the rest of the site, on a port of its own because a network that
speaks BGP already has something on 179, and advertises the remote
nodes and their pod blocks with itself as the next hop. The site's own
router receives them like any other route, which leaves the choice
between two endpoints, and the withdrawal when a remote goes away,
where they belong.

Both are advertised, and the blocks are not redundant. A router will
not resolve one BGP route's next hop using another BGP route, since
that is how resolution loops form. The node address arrives that way,
so the blocks behind it would stay unreachable however well the address
underneath them is routed. Carrying the blocks too, with the endpoint
as the next hop, resolves them against a directly connected address and
asks nothing of recursion.

```mermaid
graph LR
  subgraph onprem["your cluster"]
    cp["control plane"]
    w1["worker-1<br/>tunnel endpoint"]
    w2["worker-2<br/>no tunnel"]
  end
  subgraph aws["AWS"]
    r1["remote node"]
  end
  cp --- w1
  w2 --- w1
  w1 === r1
  linkStyle 2 stroke-width:3px
```

worker-2 sends to a remote pod by way of worker-1, which forwards it
into the tunnel. The tunnel carries traffic belonging to neither of the
two nodes holding it, in both directions, which is what makes this work
at all.

## Compatibility

What each distribution has passed on the containerlab harness: a
seven-node HA lab (three control planes, two site workers behind a
NAT router, two remotes in separate clouds meeting only across a
modelled internet), the site built by `harness/clab/cluster.d/`, the
remotes joined through this operator's own provider for that
distribution.

Two axes, because both change what the mesh must do: the
distribution decides how a remote joins and who balances its API
path, and the container network decides whether the tunnel carries
pod addresses or node addresses.

| | kubeadm | k0s | k3s | RKE2 |
|---|---|---|---|---|
| native join through the provider | bootstrap token + CA pin | bootstrap token in a kubeconfig, gzip+base64 | `K10<CA-hash>::<id>.<secret>`, minted via the API | same, supervisor on 9345 |
| who balances the API path | **wg-apiproxy** (ours) | nllb (envoy, `[::1]:7443`) | agent balancer (`127.0.0.1:6444`) | agent balancer (`127.0.0.1:6443`) |
| calico (native) | pass | pass, full 6×9 | pass | pass |
| kube-router (native) | pass | pass (built-in) | — | — |
| flannel (encapsulated) | pass | — | pass (embedded) | pass (canal, built-in) |
| cilium (encapsulated) | pass | pass | pass | pass |
| outage: `cp-dies` (link) | pass | pass | pass | **excluded: RKE2 limitation** |
| outage: `reboot-remote`, `reboot-cp` | pass | pass | pass | pass |
| mechanism assertions | pass | pass | pass | pass |

Thirteen combinations, each rebuilt from an empty topology and joined
through the product's own provider. A dash is a combination not run
rather than one that failed: each distribution carries its own
network as a row (`default`), and the ones not listed add no
mechanism the other cells do not already cover.

What a row measures: every ordered pair of nodes by pod address, the
same by service address, a 1 MiB transfer per pair, cluster DNS from
each node, and the path off the cluster from each node. That is 120
checks on the six-node rows and 161 on the seven-node ones. The
transfer check exists because every other check fits in a single
small packet and so cannot see a path whose largest packet does not
cross, which an encapsulating network stacking its own header inside
the tunnel's is the obvious way to produce.

All thirteen rows come from one campaign against one build, each row
rebuilt from an empty topology with every check enabled: thirteen
passed, none failed. Earlier campaigns are not folded in, because
they ran against trees that still carried defects these rows found.

The rows, and what each one claims:

- **Placement rows** (`harness/clab/scenarios.tsv`) vary which site
  nodes hold tunnels, from one control plane to every node, against
  one or two clouds. Each row measures every pair of nodes in both
  directions, by pod address and by service address, by a transfer far
  larger than any packet the path can carry, plus cluster DNS and the
  path off the cluster. The `all-nodes-two-clouds` row is the
  superset, so a combination that passes it has proven the mesh does
  not care what built the cluster or what carries its pods.
- **Outage rows** (`harness/clab/outages.tsv`) make three claims in
  sequence: the full matrix is green before anything breaks, the
  survivors converge to green among themselves while the victim is
  down, and the victim's return brings the whole lab back to green
  with nothing reinstalled or forgiven. `link` rows pull the cable and
  leave the machine running; `reboot` rows SIGKILL the machine and
  give it back only what a platform provides (a NIC, an address, a
  gateway), so everything else must be rebuilt by what the node runs
  at boot. `reboot-remote` is the core invariant made executable: the
  host unit raises the tunnel from its cached peer list while the
  cluster is unreachable, because the tunnel must never depend on the
  Kubernetes it carries.
- **Mechanism assertions** (`harness/clab/mechanism.sh`) verify the
  who-balances table on the live remotes: kubeadm's wg-apiproxy
  answering on the loopback with kubelet dialing it and surviving the
  dialer's death; k0s's nllb envoy; the k3s/RKE2 agent balancer's
  state file holding all three control planes. They also compare the
  network the controller says it is modeling against the one the row
  installed, encapsulation included: a mesh that models the wrong
  network publishes the wrong prefixes while every component reports
  healthy, which is how kube-router stayed broken for two full rows.
  "Full" versus the shorter suite is scope, not doubt: k0s-calico (the
  production pairing) ran all six placement rows and all nine outage
  rows; the rest run the superset placement row plus the outage rows
  whose mechanics differ.

**The RKE2 exclusion** is RKE2's own recovery story, measured and
recorded rather than papered over (`harness/clab/distros.tsv` carries
the full mechanism): a sustained partition of an RKE2 server node
wedges that node's etcd beyond the distribution's unaided recovery.
rke2-server fatals on lost leader election (by design, expecting a
clean restart), its containerd dies with it while the pod shims
survive, and the orphaned etcd keeps heartbeating raft while its
serving paths block on a log pipe nobody reads anymore; every restart
then dies at the datastore reconcile, even after the partition heals.
Killing the orphaned etcd by hand recovers the node in about three
minutes. Known upstream (rancher/rke2#4510, #4479, #7155). k3s embeds
etcd in-process, restarts it with itself, and passes the identical
row. The mesh stayed green throughout: no tunnel was on the affected
node.

Talos is excluded from the matrix by design decision, not omission:
it runs no foreign binaries and has no systemd, and this tunnel is
host-configured precisely so it cannot depend on the Kubernetes it
carries. Supporting Talos means WireGuard in the machine config or a
system extension, a different product surface tracked in
`join-patterns/README.md`.

## Layout

```
controller/               endpoint-controller (claim, join and mesh) and the dialer
controller/pkg/join/      one package per specialization: k0s and kubeadm for
                          joining, aws and docker for fulfillment
controller/pkg/discover/  reads the cluster's own API addresses
controller/pkg/cni/       recognises the network and reads each node's pod blocks
controller/pkg/tunnel/    the wire contract shared by every producer and consumer
join-patterns/            cloud-init templates, one per join mechanism
charts/                   the Helm chart
examples/                 a complete set of manifests
harness/health-check.sh   every pair of nodes, by pod address and by service address
harness/netns-routing/    single-NIC routing end to end for the dialer
harness/kind-e2e/         one claim becomes a real joined node over a real tunnel
harness/vm-single-nic/    real VM, real boot and reboot
```
