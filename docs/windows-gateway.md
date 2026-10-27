# Windows gateway integration design

The opt-in AWS controller reconciles Windows gateway attachments with durable
resource leases and consumer acknowledgements. Windows Server 2022 and 2025 each require native qualification.
Large-transfer MTU handling, self-Service hairpin access and lifecycle qualification
remain incomplete. This is an experimental configuration, not a production support claim.

Windows HNS needs an Ethernet adapter for the tested native Calico VXLAN
network. The Windows WireGuard adapter carries host IP traffic but cannot host
that HNS network. A Linux gateway in the worker subnet can terminate WireGuard
and forward encapsulated traffic onto Ethernet. Windows retains its native
distribution CNI and physical NIC address.

## Planning implementation

The provider-independent [attachment package](../controller/pkg/attachment/plan.go)
validates CAPI Machine UID, Node UID, provider identity, network-interface identity,
network and subnet membership. It renders host return routes separately from
the worker's native transport address and rejects routes overlapping the cloud
subnet or observed WireGuard underlay endpoints. The current forwarding
capability is IPv4 UDP transport within a shared subnet.

[Peer projection](../controller/pkg/attachment/peers.go) adds the worker's native
host prefix to the gateway peer for site consumers. For the attached worker it
removes CNI transport destinations from kernel-route requests, including legacy
single-host and elected transit routes, while preserving WireGuard AllowedIPs.
It refuses an absent or duplicate gateway key and conflicting ownership of the
worker address. Projection operates on copies and is idempotent.

Render a proposed plan from a public topology snapshot with the harness command:

```sh
cd harness/e2e
go run ./cmd/gatewayplan < observed-topology.json > proposed-attachment.json
```

The input contains `request` (a `GatewayRequest`), `gatewayKey`, `sitePeers` and
`workerPeers`. The two peer lists are the current renders for their respective
consumers. The output contains the validated plan and both projected lists.
The command performs no cloud or Kubernetes mutations and does not establish
readiness. Provider adapters must revalidate instance and interface identities
before applying it; a snapshot is not a lease on those resources.

## Lifecycle implementation

The [attachment reconciler](../controller/pkg/attachment/reconcile.go) composes
three capabilities: persistent intent storage, infrastructure forwarding and
consumer publication. Each step advances one recorded boundary:

```mermaid
stateDiagram-v2
    [*] --> Preparing
    Preparing --> Publishing: forwarding observed ready
    Publishing --> Ready: consumers acknowledge
    Ready --> Publishing: forwarding or publication drifts
    Preparing --> Withdrawing: removed or replaced
    Publishing --> Withdrawing: removed or replaced
    Ready --> Withdrawing: removed or replaced
    Withdrawing --> Releasing: withdrawal acknowledged
    Releasing --> Complete: owned forwarding released
    Complete --> Preparing: new desired attachment
```

Provider preparation starts only after intent is saved. A failed persistence
write after a provider operation causes a retry of the same lease. Replacement
retains the original plan throughout withdrawal and release. It prepares the
replacement afterward; uninterrupted gateway failover remains a separate gate.
Every new attachment attempt gets a fresh persisted lease identity, including
reattachment after the intent object was deleted.

The [ConfigMap store](../controller/pkg/attachment/store.go) uses object UID and
resourceVersion preconditions. Its finalizer retains active intent through a
deletion request. Reconciliation uses the stored plan for cleanup even if the
new desired input is invalid, and removes the finalizer only after release.
Controllers must serialize reconciliation per attachment and retain the durable
intent when reporting errors. A phase alone cannot replace provider read-back
or exact consumer acknowledgement.

The [EC2 SDK transport](../controller/pkg/attachment/aws/transport.go) implements
nine attachment operations without an AWS CLI dependency. The caller supplies an
explicit region and credential provider; credentials never enter resource journals.
Use refreshable role credentials when wiring the runtime, following the
[AWS SDK credential configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html).
The transport preserves the harness JSON observation contract, including ENI
`TagSet`, source/destination check booleans, ownership tags and pagination tokens.
SDK protocol tests cover signed requests and service errors. This transport is
available through opt-in runtime registration. The managed two-worker validation below exercises the registered worker with
real AWS attachment requests.

The [EC2 route adapter](../controller/pkg/attachment/aws/routes.go) now provides
shared return-route leases with a separate per-route journal. It persists create
and delete intent, resolves uncertain API responses by reading the exact target,
preserves a route until its final lease is released, and refuses pre-existing
routes without ownership. Cleanup handles a deleted gateway ENI but refuses an
ENI reassigned to another instance. The ConfigMap journal retains its finalizer
until the owned route is absent. A single elected controller must own each
provider scope; its shared adapter serializes resource operations across workers. The endpoint-controller enables controller-runtime leader election with a Lease
in the mesh Secret's namespace, named from a hash of the Secret name. Controllers
pointing at the same mesh contend for that lock even if their Deployment names
differ. Lease release on cancellation is disabled so takeover waits for expiry. The chart's controller Role grants Lease get/list/watch/create/update/patch;
manual RBAC installations must provide these before upgrading the binary. All ten Nodes
remain Ready. No gateway requests were submitted. Different meshes sharing AWS
resources still require an explicit common ownership scope.

The [UDP ingress adapter](../controller/pkg/attachment/aws/ingress.go) shares
an explicit IPv4 host/port permission across leases. It persists a random
ownership token before creating a tagged rule and records the returned rule ID.
It rejects pre-existing exact permissions, changed rules and replacement IDs;
final release revokes only the owned rule ID. Deletion of the security group
allows the journal to retire without a revoke call. A ConfigMap finalizer
retains active leases through journal deletion.

The [EC2 resource composition](../controller/pkg/attachment/aws/forwarding.go)
prepares source/destination checking, ingress permissions and then return routes.
It retains partial progress for retry. After consumer withdrawal, cleanup
attempts every ingress and route release and restores the gateway setting only
once those releases finish. Tests exercise the three actual lease adapters
together with simulated cloud responses, including shared-worker retirement.
This composition has not yet been exercised as one native AWS operation.

Its `ForwardingBinding` must be persisted before setup: lease, provider scope,
gateway identity and exact route/ingress targets must survive Machine deletion. Cleanup uses that binding rather than resolving a replacement Machine. The
[bound-resource adapter](../controller/pkg/attachment/aws/binding.go) persists
this immutable binding in a ConfigMap before setup. Its finalizer retains it
through deletion until all provider releases complete. Cleanup takes only the
lease ID; stale writes and changes to an existing binding are rejected. An isolated Kubernetes API test covers deletion, restart cleanup and finalizer
removal with the three resource adapters and simulated EC2 responses. The [AWS resolver](../controller/pkg/attachment/aws/resolver.go) validates the
live namespaced CAPI Machine UID, Node UID, provider identity, ENI ownership,
primary address, single-interface topology and shared security group. It emits
site-source and worker-source ingress permissions for both transport directions. Additional simulated tests reject replaced Nodes, extra NICs,
foreign network identities and changed security-group membership. The native
check is read-only and does not qualify guest forwarding.

The [attachment forwarder](../controller/pkg/attachment/aws/attachment_forwarder.go)
implements the provider-independent lifecycle using this resolver and persisted
bindings. It requires a guest capability and waits for guest forwarding readiness
after preparing EC2 resources. Cleanup waits for guest withdrawal and uses the
saved binding even when live Machine resolution fails. Tests cover these gates;
the native execution transport and controller registration remain unfinished.

The [Linux gateway guest check](../controller/pkg/attachment/aws/linux_guest.go)
requires existing IPv4 forwarding and verifies route lookups for each site
source in both directions using `from` and `iif`. Traffic arriving over
WireGuard must leave the physical interface for the worker; worker traffic
arriving on the physical interface must return over WireGuard. It does not
change sysctls, routes or NAT, and its release leaves those externally managed
settings intact. Failed prerequisites prevent readiness. This is route/sysctl evidence, not a packet-transfer result.

The existing dialer already handles reverse-path filtering. Its
`ensureTransit` option adds tunnel-subnet masquerading; it serves destinations
without tunnels and remains a
separate capability. The native Windows VXLAN gateway path retains original
transport source addresses and must not enable that option merely to satisfy
its forwarding check.

The remaining integration must wire the native guest execution transport,
supply consumer publication, register reconciliation in the running controller
and connect its intent lifetime to CAPI Machine removal. Shared settings need
their own resource journals; the outer attachment intent does not manage them.

The shared tunnel contract accepts `gateway-projections.json` in the central
peer Secret. Each entry binds an attachment lease to worker/gateway public keys,
the worker's native host address and the transport hosts it reaches directly.
Both the site dialer's Secret reader and the remote adoption renderer apply
this projection. Sites route the native worker address through the gateway;
the attached worker suppresses direct transport kernel routes while retaining
WireGuard AllowedIPs; the gateway retains its native path to its workers.
Removing an entry restores the ordinary render from unchanged source fields.
Duplicate leases/worker addresses and conflicting prefix ownership are rejected.
Site transit rendering uses the same projection, so nodes reaching remotes
through a site relay also receive the Windows native host route. Withdrawal
removes that route from the desired transit set. Host-prefix spelling does not
turn a host route into a pod-block route. Invalid destinations and failed route
installation now fail transit reconciliation; they cannot count as an applied
configuration. Relayed-role acknowledgement is implemented but requires live rollout validation.

Site endpoint dialers write `site-applied-<node>` receipts to the central Secret
only after successful kernel application. Receipts contain the Node UID,
WireGuard public key and deterministic rendered peer-list hash. The writer
rechecks Node/Secret identities and current content before an optimistic update;
`tunnel.SiteConverged` verifies the same identity and content at the reader.
Changing the projection invalidates old receipts. Retained endpoints using another site relay include their role and the applied
relay/transit-route hash. They acknowledge only after all remote peers have
accepted their relayed placement. Verification recomputes the selected relay
from current Node readiness; a receipt for the previous relay no longer matches.
Non-endpoint nodes write a `transit` receipt after installing and pruning their
transit routes. This receipt uses the Node/mesh identities and applied route
hash; it does not require a WireGuard key for an interface that no longer exists.
A retained-endpoint receipt cannot acknowledge the subsequent non-endpoint role.

The controller configuration
pins the site image; direct DaemonSet image edits are overwritten by that owner. No gateway projection was active during this validation. Retained-endpoint
receipts have unit coverage but still need a live placement-transition test. The [projection store](../controller/pkg/attachment/projection_store.go) persists
desired entries in the original mesh Secret using resourceVersion concurrency
checks. It preserves unrelated leases and fields, refuses active binding changes,
and validates the complete site render before updating it. Tests cover shared
withdrawal, stale lease removal, mesh replacement and real API update conflicts. The [consumer verifier](../controller/pkg/attachment/consumers.go) checks each
required Node UID, site receipt or exact remote document/Secret UID and applied
hash. Missing recipients remain pending. The central source snapshot must still
match, including API backend fields, and remain stable through verification. Receipt updates alone do not invalidate the intended source snapshot. Required
consumer identities and intended remote documents must be retained through
withdrawal; they must not be inferred from whichever pods still exist. The [snapshot builder](../controller/pkg/attachment/snapshot.go) renders expected
remote documents from the committed mesh Secret and retained consumer identities. It shares `tunnel.RemotePeerDocument` with the endpoint controller, including
canonical API membership and gateway projection. It refuses missing recipients,
changed public keys and unacknowledged site roles. It does not use the current
adoption payload as desired state, because that payload may still be stale. The [publication lifecycle](../controller/pkg/attachment/publication.go) persists
the projection and required consumer set in a ConfigMap before updating the mesh. Its finalizer retains an append-only recipient history through withdrawal. A remote
recipient can stop acknowledging only after its original Node UID and adoption
Secret UID are gone and its public key is absent from the mesh. Missing pods,
NotReady nodes and deletion timestamps do not prove retirement. Every current
recipient, including a same-name replacement, is recorded before its exact
acknowledgement is accepted. Site recipients and the attachment worker/gateway
cannot be retired by this mechanism. Publication waits
for exact peer acknowledgements before invoking native CNI transport selection. Withdrawal restores the recorded CNI state, removes only its projection and
waits for restored peer-list acknowledgements before retiring publication intent. Unit tests use the real projection store, snapshot builder and verifier with a
simulated CNI capability. An isolated API test verifies recipient retention,
stale-write rejection and finalizer removal. The [Calico transport adapter](../controller/pkg/attachment/calico_transport.go)
stores its lease, original annotation and Node/provider identity in
`cloud-provisioning.appmana.com/native-transport` atomically with the address
change. Ordinary endpoint-controller address reconciliation defers to that
validated owner while still maintaining claim metadata. Restoration checks the
original Node UID, preserves unrelated annotations and refuses external address
changes. A deleted Node can retire without touching its replacement. Setting an
annotation alone is not readiness: the adapter requires a native CNI observer. The [Windows Calico observer](../controller/pkg/attachment/windows_calico.go)
reads HNS through a Machine-bound guest transport and requires exactly one
`Calico` Overlay network with the intended management address. The separate
`External` network is not sufficient. The
management address is host-network evidence; it does not prove packet delivery
or policy enforcement. The [live consumer resolver](../controller/pkg/attachment/consumer_resolver.go)
retains every published site and remote identity without filtering by pod
availability or readiness. It binds remote Machines to Node/provider identities
and adoption Secret UIDs. The three currently relayed site consumers remain required;
their relayed receipts must be verified during rollout. Do not write projection entries manually as a substitute for that lifecycle.

## Responsibilities

Separate guest bootstrap, CNI transport and infrastructure forwarding. A guest
renderer installs the distribution worker. A network attachment selects direct
tunnel transport or a subnet gateway. An infrastructure capability implements
the forwarding resources for that attachment. The join reconciler should not
branch on combinations of Windows, AWS and individual CNIs.

| Input | Owner | Purpose |
| --- | --- | --- |
| Machine UID and provider identity | CAPI | Bind the attachment to the actual instance and detect replacement |
| Gateway Machine UID and provider identity | CAPI | Bind forwarding to the current gateway instance |
| Subnet and network-interface identity | Infrastructure provider | Validate placement and select the forwarding target |
| CNI transport addresses | CNI observation plus guest observation | Carry the actual encapsulated packet endpoints |
| Management addresses | Distribution join provider | Reach API backends independently of workload addressing |
| WireGuard endpoint addresses | Tunnel configuration | Establish the outer tunnel without routing it through itself |
| Applied configuration digest | Tunnel publisher | Confirm consumers have received a change before retirement |

The CNI transport address set must remain separate from `PeerSpec.RouteHosts`.
The latter also carries management and elected transit addresses. Its existing
single-owner and acknowledgement semantics remain useful for tunnel delivery,
but do not authorize cloud route creation. Never derive VPC return routes by
copying the entire peer route list.

## Reconciliation and removal

1. Resolve the worker and gateway by Machine UID, provider ID and Node identity.
   Require the same supported cloud network placement and a ready gateway.
2. Prepare provider forwarding with explicit resource ownership. For AWS the
   tested path requires gateway source/destination checking disabled, host return
   routes targeting the gateway ENI and scoped VXLAN ingress. Do not replace an
   existing route owned by another attachment or open VXLAN to the Internet.
3. Publish worker CNI transport hosts on the gateway's peer, preserving a single
   WireGuard owner per prefix. Wait for exact consumer acknowledgement before
   declaring the attachment usable.
4. Preserve the Windows HNS transport address. Configure host management routes
   so they do not intercept VXLAN intended for the physical NIC. API bootstrap
   and distribution-local balancing must work before the Node can register.
5. On worker removal, withdraw the attachment and observe acknowledgement before
   releasing its forwarding resources. Shared gateway settings remain while any
   attachment depends on them. Restore only settings still owned by this controller.
6. Gateway deletion must respect dependent workers. Replacement needs an explicit
   handover and observed convergence; changing an ENI route alone is not proof
   that consumers have moved. Persist retirement state across controller restarts.

The local VM provider should implement the same forwarding capability through
its out-of-band guest agent and router VM. Each guest still has one physical NIC.
AWS implements it through EC2 route, security-group and instance APIs. Neither
provider should simulate Ready conditions or successful acknowledgements.

## Acceptance gates

Run both Windows builds with continuous endpoint-controller reconciliation.
Require fresh CAPI boots, Services and DNS, bidirectional pod traffic, large
transfers, NetworkPolicy enforcement, worker removal/replacement and gateway
failure/replacement. Revalidate existing Linux workers under the chosen native
Traefik configuration. The evidence below covers selected smoke, transfer and ingress-policy paths;
remaining gates must be completed before release.

## Enable the AWS attachment controller

The optional runtime supports a Windows Calico worker and a Linux subnet gateway.
Other CNI/OS combinations require their corresponding observer and transport
capabilities. Set `awsGateway.configMap` to a ConfigMap in the release namespace
with a `scope.json` key:

```json
{
  "scope": {
    "Account": "123456789012",
    "Region": "us-west-2",
    "VPCID": "vpc-example",
    "SubnetID": "subnet-example",
    "RouteTableID": "rtb-example",
    "OwnerTag": "cloud-provisioning-test",
    "OwnerValue": "my-cluster"
  },
  "clusterName": "my-cluster",
  "groupID": "sg-example",
  "meshUID": "REPLACE-WITH-CURRENT-PEER-SECRET-UID",
  "nativeInterface": "ens5",
  "tunnelInterface": "REPLACE-WITH-GATEWAY-WIREGUARD-INTERFACE"
}
```

The peer Secret UID pins the mesh across restarts; replacing that Secret requires
explicit scope reconfiguration. Restart the controller after changing this startup
configuration. Configure an AWS SDK credential provider for the controller role.
For temporary test credentials, `awsGateway.credentialsSecret` names an existing
Secret containing `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and
`AWS_SESSION_TOKEN`; restart after rotating these environment credentials. Never
put credentials in `scope.json`, requests or resource journals. Attach the gateway
route, source-check, ingress and observation policies documented in [AWS](aws.md).
The worker and gateway require a working SSM agent and instance role.

Submit a ConfigMap in the release namespace labeled
`cloud-provisioning.appmana.com/gateway-request: <peer-secret-name>`. Its
`data.request.json` is a JSON [GatewayRequest](../controller/pkg/attachment/plan.go):
`worker` and `gateway` each contain the observed CAPI `uid`, `nodeUID`,
`providerID`, `interfaceID`, provider-qualified `networkID`, native `subnet` and
`address`; `siteTransport` lists native CNI transport IPv4 destinations.
`siteReturnSources` adds observed mesh source addresses that need AWS return
routes while retaining their worker tunnel routes (including API balancer
addresses). Do not mix those source-only addresses into `siteTransport`. `underlay`
lists outer tunnel endpoint addresses, and `udpPort` is Calico's configured VXLAN
port. Resolve these identities from the running cluster and provider rather than
copying an old validation artifact. The AWS resolver rechecks them before use.

Set `tcpPorts` to additional control-plane listeners that the distribution uses
through the native gateway. For k0s's default Konnectivity agent listener, include
`"tcpPorts": [8132]`; the configured Kubernetes API port is already included by
the runtime. Use the actual configured listener if it differs. Do not add k0s's
port to distributions that do not use it. Rules remain scoped to the worker's
native `/32` in the owned security group. Port order and duplicates do not affect
identity, but changing the port set retires the previous attachment and prepares
a new immutable binding. An empty list retains API-only TCP ingress.

A request's UID forms its durable attachment identity. The controller adds its
finalizer before infrastructure preparation, periodically checks readiness and
drift, and withdraws when the request is deleted or its selection label removed.
The lifecycle record is stored in a `network-attachment-*` ConfigMap under
`data.record.json`; `Ready` requires provider, guest and consumer observations.
The request stays finalized until withdrawal and resource release complete.

One elected mesh controller must exclusively own an AWS resource scope. Multiple
meshes sharing that scope are not coordinated by the mesh Lease. The AWS runtime
uses [CAPI lifetime guards](../controller/pkg/attachment/runtime/capi_lifetime.go)
to install a separate pre-drain and pre-terminate hook pair for each attachment
lease on its worker and gateway before preparing resources. A deleting
participant triggers deletion of the matching request by UID. Its finalizer
retains withdrawal, and shared gateways keep every other lease's hook pair.
Hooks are removed only after publication withdrawal and resource release have
been committed as complete; partial hook removal retries against the original
Machine UIDs. A protected deleting Machine remains an acknowledgement recipient.

Automatic gateway deletion and the remaining provider/distro paths still need
native tests. Deployments without the paired hooks must withdraw the attachment
and wait for its request finalizer before deleting participants. Missing consumers
block acknowledgement until retirement is proved against Node, adoption Secret
and mesh identities. One UDP case failed in the post-removal matrix, so this
resource-lifecycle result does not qualify sustained workload connectivity.

The native Windows tunnel service verifies desired kernel routes before renewing
an unchanged delivery receipt. Missing routes trigger repair from the current OS
table rather than the backend's installed-route cache; failed repair leaves no
receipt. Healthy unchanged state does not replace WireGuard peers. Verification
allows Windows-created multicast/link-local routes and checks retirement only for
routes owned by this adapter. This validates route presence, not end-to-end API or
pod connectivity. Image rebakes must include the repaired native binary.

## MTU requirements and transfer validation

Set workload MTU from the smallest effective packet budget on the complete path.
The tested 1420-byte WireGuard interface carries IPv4 VXLAN with 50 bytes of
additional overhead, giving a 1370-byte inner packet budget. Calico's
[encapsulation guidance](https://docs.tigera.io/calico/latest/networking/configuring/mtu)
describes its built-in WireGuard arrangement; this project instead carries
Calico VXLAN inside a separate tunnel. Configure both Linux and Windows workers.
Small requests and TCP success alone cannot qualify that configuration.

The dialer's MSS rules match TCP directly on the WireGuard interface. They do
not match the inner TCP header when WireGuard carries VXLAN UDP. Keep these
rules for direct TCP paths; encapsulated paths still need the correct workload
MTU.

Windows requires the [Calico 3.32 Felix MTU patch](../providers/calico/README.md). It resolves the attached endpoint's isolated namespace, applies an MTU upper
bound, and checks read-back before publishing endpoint policy. Unavailable
compartments remain pending while other endpoints progress. The helper therefore runs in Felix's endpoint reconciliation.

## Calico address ownership

The product writes each cloud worker's Calico node address: the tunnel address,
or the native address once the attachment controller moves a worker to native
transport. Linux Calico's `IP=autodetect` would select another address and
overwrite that choice at startup and in its periodic address checks, so the site's
`calico-node` never runs on a cloud worker. On tunnel-class Linux workers the
product runs `calico-node-tunnel` instead: a copy of the site's DaemonSet, kept in
step with it, with `IP=none` and, where the site sets `IP6`, `IP6=none`. With
detection off Calico keeps the stored address
([Calico's address selection](https://docs.tigera.io/calico/latest/networking/ipam/ip-autodetection)).
See `siteCNI` in the chart's values. The site's own nodes keep the site's
autodetection.

Windows Calico (`calico-node-windows`) still detects its address on Windows
cloud workers; the product does not yet own it there.

Require stable stored addresses and running Calico pod identities across at
least one periodic address-check interval after rollout. Readiness alone does
not prove address stability. Do not ignore address changes in retirement
validation or lengthen receipt expiry to conceal competing address writers.

## Configure the k0s MTU

For the tested 1420-byte WireGuard path carrying IPv4 VXLAN, configure a
1370-byte Calico MTU on both Linux and Windows. The
[k0s 1.36.2 schema](https://github.com/k0sproject/k0s/blob/v1.36.2%2Bk0s.0/pkg/apis/k0s/v1beta1/calico.go)
defaults Calico to 1450; changing only Windows can make TCP pass while larger
UDP exchanges involving Linux still fail.

Merge this setting into the existing k0s controller configuration:

```yaml
spec:
  network:
    provider: calico
    calico:
      mtu: 1370
```

For static configuration, update every controller's existing configuration file,
validate it with `k0s config validate --config /etc/k0s/k0s.yaml`, then restart
controllers one at a time. Require the restarted API's `/readyz` and Node readiness
before proceeding. Wait for the generated Linux `calico-node` DaemonSet rollout.
Recreate workload pods to verify the new interface MTU; existing veths can retain
the old value. Dynamic-config clusters require their corresponding k0s configuration
workflow instead of this static-file procedure.

On Windows, use the [Calico 3.32 MTU patch](../providers/calico/README.md) with
`FELIX_VXLANMTU=1370`. The fresh-site harness flag `-k0s-calico-mtu 1370`
configures k0s's bundled Calico, but does not install that Windows patch. It leaves
other CNI profiles untouched and rejects `-reuse-site`; configure an existing
site through its distribution before reusing it for validation.

k0s owns the bundled Windows DaemonSet too. Configure image or command changes
through its Calico settings: a direct DaemonSet edit is removed when a controller
regenerates the manifests. Use the [derived Windows Calico image](../images/calico/README.md)
with `spec.images.calico.windows.node` and the appropriate pull Secret. This
packages the executable before startup and preserves the distribution's command.

For an explicitly host-staged validation binary, append
[this patch object](../providers/calico/k0s-mtu-validation-patch.json) to
`spec.network.calico.patches`. It requires and checks the staged file on each
Windows host. Remove that command override when using the derived image. In
either configuration, check the actual binary hash after controller restarts as
well as the pod MTU: existing interfaces can retain a correct MTU after a
controller has replaced the Felix deployment.

## Diagnose Windows UDP loss

The continuous UDP acceptance gate remains open. Native traces identify both
`vfpext.sys` request drops and TCPIP reply drops during HNS encapsulation-layer
rebuilds. These observations do not explain every loss. Require native traffic,
isolation and lifecycle validation before promoting a corrective image.

Check the running CNI image digest and bootstrap script before changing network
creation. The tested VXLAN script creates an `External` Overlay with VSID 9999;
the reviewed fork's guarded L2Bridge bootstrap is different code. A network's
name alone does not establish ownership or make deletion safe.

For removal experiments, record API deletion, VFP space removal and the measured
traffic window separately. An absent HNS API object does not prove dataplane
cleanup has finished. Drops remain reproducible after the observed space
cleanup, so overlay removal is not a qualified workaround. Stabilize Calico's
node addresses before measuring this behavior; address stability alone does
not establish reliable packet delivery.

Use fixed-cadence probes to distinguish missing replies from the sequential
probe's receive timeout. Preserve unsent slots separately from transmitted
requests and retain every loss. A five-second pause in a serial probe alone is
not evidence of a five-second network outage. Fixed-cadence diagnostics do not
replace the existing survivor acceptance checks.
