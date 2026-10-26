# Harness SDK integration

The harness constructs upstream Containerlab Go topology objects and passes them
to Labcontainers. The Kubernetes API-access client is shared from
`labcontainers/pkg/kubernetes/kube`; product-specific CAPI identity assertions
remain in this project. Probe Pods, Services, and their Namespace use native
Kubernetes objects, not manifest strings. Probe images must be preloaded.

VM network bootstrap constructs Labcontainers' cloud-init schema-generated
`NetworkConfigVersion2` objects. The shared writer handles the launcher's
network-config file boundary; tests do not assemble YAML. This product's
cloud-connected scenario still supplies its existing site gateway and resolver
explicitly in that object. Those settings are not generic SDK defaults, and
the product's single-link constraint does not restrict general SDK VM topologies.

The VM rig never interprets a missing saved session as permission to destroy a
same-named Containerlab lab. `Down` is a no-op when no owned session exists;
`Up` lets the SDK reject runtime name collisions. Recover or relocate older lab
state explicitly; normal SDK operation does not adopt or remove it
automatically. Cluster nodes are always VMs. Routers, cloud edges and the
bastion stay containers and are reached only through the lab's own session.

This module pins its published Labcontainers dependency in [go.mod](go.mod);
no local SDK workspace or SDK replace directive is needed. Build the SDK daemon
and VM wrapper from that same revision. The local product controller module
remains a dependency in this repository.

From this module, test the published dependency with:

```sh
GOWORK=off go test -p 2 ./...
```

Build the matching SDK daemon with `make build` in its worktree and set
`LABCONTAINERS_LABD` to its absolute `bin/labd` path for live tests. VM recovery
also requires the isolated corrected Containerlab binary described in the SDK
README, selected using `LABCONTAINERS_CONTAINERLAB`. Neither setup replaces the
host installation.

Normal Ubuntu VM command and file operations reconnect to the saved SDK session
and use `Node.Commands()` over serial QGA, including SDK-owned node identity and
native file transfer. The product wrapper now inherits the SDK's prepared Ubuntu
image and launcher instead of maintaining a second NIC/QEMU implementation.
It retains `/cldt-guest` only as a compatibility symlink. Build it with an
explicit, already-loaded SDK base:

```sh
make vm-wrapper LABCONTAINERS_VM_IMAGE=YOUR_PREPARED_SDK_VM_IMAGE
```

Use `VM_WRAPPER_IMAGE=YOUR_NEW_TAG` to qualify a candidate without overwriting
the normal product tag. Missing or unloaded base images fail before the build;
there is no default base or automatic pull in the Make target. The SDK launcher
owns declared-NIC handling, loopback-only QEMU control listeners, QGA, and the
`/labcontainers-reset-instance` marker. Existing old product wrappers must be
rebuilt before using these paths. Image-builder SSH remains a separate path; its
migration is not claimed complete here.

The Ubuntu SDK command adapter has an opt-in real-VM regression:

```sh
GOWORK=off \
LABCONTAINERS_LABD=/absolute/path/labd \
LABCONTAINERS_CONTAINERLAB=/absolute/path/corrected-containerlab \
CLOUD_PROVISIONING_SDK_VM_IMAGE=YOUR_PRELOADED_SDK_VM_IMAGE \
CLOUD_PROVISIONING_SDK_APPLIANCE_IMAGE=YOUR_PRELOADED_ALPINE_IMAGE \
go test ./rig/vm -run '^TestLiveSDKGuestCommands$' -count=1 -v -timeout=6m
```

It constructs a native topology with one zero-NIC VM and a networkless appliance, reconnects through the
product runtime, checks binary stdin/file transfer and mode, preserves nonzero
command diagnostics, and tears down the owned session. The work/evidence path
is printed and retained. It requires `/labcontainers-guest` in the prepared
image and qualifies the runtime command adapter only, not the guest image builder or
Kubernetes/Calico networking.

Container commands and file transfers also use the SDK session, including
router/bastion appliances inside a VM lab. Appliance views select their parent
VM session rather than reconstructing Docker container names or looking for a
separate container-rig session. A configured runtime that cannot reconnect fails
explicitly instead of falling back to host CLI execution.

The standalone Calico installer (kubeadm profile) no longer downloads vanilla
Calico or reuses an unverified `calico.yaml` cache. Go callers supply
`network.Deps.CalicoObjects` as native `runtime.Object` values, including an
`apps/v1.DaemonSet` for `kube-system/calico-node`. Custom resources can use
upstream `unstructured.Unstructured`; their fields are preserved. The fixture
copies objects before setting the pool CIDR and `imagePullPolicy: Never`, and
requires SHA256-pinned image references for DaemonSet/Deployment containers.
Other workload kinds are rejected rather than silently missing their images.

For CLI runs, supply `--calico-objects` and `--calico-objects-sha256`: an explicit
prepared native Kubernetes `v1.List` JSON artifact from the aligned fork. The
artifact can be serialized from the same Go objects; callers need not write
YAML. Preparing/building the fork and selecting compatible image digests remain
project responsibilities. Content pins alone do not prove fork provenance.
These options do not override distribution-bundled Calico; using them with a
different distribution fails explicitly. Bundled-provider fork alignment and
full Windows pod-network qualification remain outstanding.

The k0s builder now constructs upstream `v1beta1.ClusterConfig` objects,
including native Calico patch objects. Configuration writing, native argument
installation, and single-attempt readiness probes use
`labcontainers/pkg/kubernetes/k0s`. Product profiles, site orchestration, and
cloud provisioning assertions remain here.

Fresh k0s builds require an explicitly prepared binary and its content pin:
`cluster.Deps.K0sBinary` and `K0sBinarySHA256`, or the lab command's
`--k0s-binary /absolute/path/to/k0s --k0s-binary-sha256 <64-hex-digest>`.
For a fork, also supply `--k0s-version <exact-version>` and
`--k0s-source-revision <full-source-commit>`. The dual-stack BGP profile passes
these through the SDK artifact pin validator; its Kubernetes base version stays
pinned. Every staged or reused node must report the selected version. Reusing a
fork also requires its expected checksum, rather than trusting an observed
checksum as the expected artifact. These identity checks do not qualify embedded
components, native architecture execution, restart continuity or networking.
There is no automatic upstream binary download or fallback to a cached build.
Prepare the intended AppMana fork artifact before running a fork scenario.
Reuse of an existing installation does not require a new binary.

For fresh k0s Calico profiles, also pass `cluster.Deps.K0sImages` using the
upstream `v1beta1.ClusterImages` type. The CLI accepts the same native object's
JSON via `--k0s-images` and `--k0s-images-sha256`. No parallel image schema is
introduced. Calico CNI/node/controllers and kube-proxy must have explicit
SHA256 pins in their native `ImageSpec.Version` (`tag@sha256:...`), and
`DefaultPullPolicy` must explicitly select `Always`, `IfNotPresent`, or `Never`
to match the deployment being tested. Preload the aligned images for `Never`;
provide registry access for policies that permit pulls. The fixture preserves
the selected policy and does not infer fork provenance or compatibility from a tag.
Both native Windows image groups are also required with pinned CNI/node,
kube-proxy and pause images: k0s can enable Windows manifests dynamically when
a Windows worker joins, so leaving those fields unset would permit implicit
upstream selection later. Other native image
fields pass through unchanged. The reader does not insert k0s's default pull
policy, and the fixture copies the native object before serialization.
This is configuration/preflight coverage, not evidence that the aligned
Windows cluster or its pod network has booted successfully.

Fresh k3s builds likewise require `cluster.Deps.K3sBinary` and
`K3sBinarySHA256`, or `--k3s-binary /absolute/path/to/k3s
--k3s-binary-sha256 <64-hex-digest>`. Both builders use the SDK's generic
`artifact.ReadFile` verifier and upload the returned, verified bytes. The k3s
builder no longer reads an implicit cache or downloads a release. A digest
verifies content identity, not that a binary is the right fork or version;
artifact preparation remains the calling project's responsibility.
The k3s service is expressed with upstream `go-systemd/unit.UnitOption` values
and serialized by that library directly into the existing Labcontainers node
transport. Unit defaults and supervision choices remain visible in this
product's builder; there is no parallel Labcontainers service schema.

Fresh RKE2 builds require `--rke2-artifacts-dir` containing `install.sh`,
`rke2.linux-amd64.tar.gz`, and `sha256sum-amd64.txt`, with
`--rke2-installer-sha256`, `--rke2-archive-sha256`, and
`--rke2-checksums-sha256`. All three files are verified before any node is
modified. The builder uses the staged native installer through
`labcontainers/pkg/kubernetes/rke2`, with explicit native tar-installation
environment arguments. It neither downloads an installer nor silently reuses
an arbitrary existing RKE2 executable. This extraction has unit-test coverage;
it is not a new live RKE2 qualification.

The kubeadm builder now constructs upstream `v1beta4.InitConfiguration`,
`v1beta4.ClusterConfiguration`, and `v1alpha1.KubeProxyConfiguration` objects;
the product's explicit API forwarder is a native `corev1.Pod`. The SDK's
`kube.WriteObjects` writes these objects at the guest-file boundary, including
multi-document configuration streams. Tests decode the resulting native
objects to verify endpoint, kubelet arguments, conntrack settings, and proxy
mounts. This changes configuration authoring, not the runtime binaries selected
by the existing node-stack builder; its artifact preparation is still pending
extraction. The forwarder remains a product scenario choice, not an SDK default.

The native Go configuration dependency's pseudo-version resolves the
`v1.36.2+k0s.0` release commit `bdf1c22c23a5`; it does not select the binary
under test. This requires Go 1.26.3 or newer.

Full distribution-builder extraction and Calico/k0s pod-network qualification
are still pending. These changes do not claim either is complete.

MicroK8s site nodes must now come from prepared images with the builder's pinned
version/revision already installed, including snap base dependencies and runtime
images. The builder verifies every site node before mutation; it does not run
apt or install a snap from the store during a test. It explicitly holds automatic
MicroK8s snap refreshes indefinitely so a long-running scenario cannot change
versions after 24 hours. Preparing the image remains a separate project-owned
step; this check does not prove that all offline runtime images are present.
The native refresh hold requires snapd 2.58 or newer, as documented in
[Snap's update controls](https://snapcraft.io/docs/how-to-guides/manage-snaps/manage-updates/).

## VM nodes through the SDK

Cluster VMs are declared as containerlab `generic_vm` nodes, which is what the
SDK commands through the serial guest agent; a `linux` node's commands run in
its wrapper container instead. A machine is ready only when its declared
Ethernet devices exist in the guest, since the agent can answer first. Image
archives are carried into guests by chunked file transfer and imported from
that file: one guest agent message carries at most ~47 MiB of stdin
(QEMU refuses a message over 64 MiB, and stdin travels base64-encoded).

## Traffic matrix

`check` measures every ordered pair of probe pods. Each pod runs the upstream
`agnhost netexec` image pinned by digest, pulled by digest through the node's
runtime before the pods start (`Pods.Prepare`), behind a `PreferDualStack`
Service. Per pair: pod and Service HTTP, and a 1 MiB body posted to the target's
echo and returned, so each transfer crosses in both directions. Per node:
cluster DNS. For dual-stack pods the same pod, Service and transfer checks run
over IPv6, and DNS must answer the target Service's IPv6 address.

UDP size probes send exact echoes in each family around the smaller of the two
pods' interface MTUs, read inside the pods: the IPv6 minimum MTU, MTU-1 and
MTU with the don't-fragment bit set and the learned path MTU ignored
(`udpprobe -dont-fragment`), then MTU+1 and 1800 bytes, which the sender
fragments. Sizes are whole IP datagrams. Every one of the ten attempts must
come back: a single lost datagram at one size is the signature of a path
smaller than the pod MTU. The static prober is carried into each probe pod.

### Native handover characterization

`observe/wireguard_handover_native.py` compares single-device prefix ownership
with independent WireGuard receivers in a four-namespace topology. Run it only
inside fresh mount and network namespaces of an isolated Linux VM, with
`iproute2`, `sysctl`, `mount`, and `wg` already staged:

```sh
unshare --mount --net --propagation private \
  python3 observe/wireguard_handover_native.py --mode single
unshare --mount --net --propagation private \
  python3 observe/wireguard_handover_native.py --mode dual
```

To exercise the Go receive-only planner, build `./cmd/receiveplan`, stage the
binary in the VM through the labcontainers node transport, and pass
`--planner /staged/receiveplan` with `--mode dual`. The experiment uses the
planner's actual interface identities and source permissions. The planner
preserves desired egress separately, rejects missing peer documents and
ambiguous ownership, and excludes protected native prefixes from site ingress.
It does not allocate listening ports, distribute private identities, activate
routes, or authorize retirement of previous receive permissions.

The dialer's `TestNativeReceivePreparation` exercises receiver creation through
netlink and wgctrl. Build the dialer test binary with `CGO_ENABLED=0`, stage it
through the labcontainers node transport, and run it inside a fresh network
namespace of the isolated VM:

```sh
unshare --net env CLDT_NATIVE_RECEIVE_PATHS=1 /staged/dialer.test \
  -test.run '^TestNativeReceivePreparation$' -test.v
```

It checks separate public identities and listener ports, stable repeated
preparation, dual-family accept lists, retained old receivers, and unchanged
routes and policy rules. It rejects identity replacement and implicit source
withdrawal. The reconciliation loop invokes preparation only when its adoption
Secret contains an explicit `receive-prepare.json` request. That request binds
the node, kernel boot and root public identity, mesh identity, current `peers.json` hash,
future peer document, native network boundaries, and exact per-peer UDP ports.
Preparation publishes
`receive-ready.json` with public kernel bindings and the exact request hash;
it does not change `peers.json` or the applied-list acknowledgment. The local
kernel boot ID must match both the request and current Node status before
publication. `ValidateReceiveReadiness` requires an independently observed ready
target, the same boot and request identities, and complete, distinct listener
bindings. It validates preparation only; it cannot authorize traffic movement
or retirement. The controller's `prepareRemoteReceivePaths` handler stages a
proposal only from an acknowledged forwarding base, checks the Machine/Node
provider binding, and clears stale readiness when the proposal changes. Its
result retains the target, adoption version, and request/base hashes alongside
the public bindings. The main controller loop does not yet invoke this handler;
activation orchestration must gate agent support and supply verified native
network boundaries and a permitted listener-port pool. Port reservations are
stored with the proposal in `receive-ports.json` and remain occupied until
explicit retirement, including reservations for peers absent from a later
proposal. A live receiver cannot change ports during preparation; recreation
uses its reserved port. Kernel-selected ephemeral ports are not accepted as
prepared transport. The standalone test
does not establish publication, activation, retirement, or reboot recovery.

To send packets through receivers created by that runtime code, pass
`--runtime /staged/dialer.test` to `wireguard_handover_native.py --mode dual`
instead of `--planner`. The experiment invokes a test-only adapter inside the
cloud namespace, uses its returned public identities and actual listener ports,
and lets WireGuard learn site endpoints from authenticated traffic. The adapter
exercises the readiness handler with an in-memory Kubernetes API and verifies
stable receipts across repeated preparation. Receiver configuration is performed
by the Go runtime, while the experiment still owns
the site peers and route transitions. All four IPv4 exchange phases must
preserve the original pod source. This is a mechanism test, not controller
API-server publication semantics or CAPI lifecycle qualification.

`TestNativeReceiveRealAPI` runs the reconciliation path against a real lab API
from a fresh VM network namespace. Its fixture needs a short-lived kubeconfig,
a Unix-socket TCP relay to the API, a dedicated namespace whose name starts with
`cldt-receiver-api-`, and the expected `cp` Node UID. The test ServiceAccount needs
`get,patch` on only the `adoption` Secret and `get` on only the `cp` Node; an
`unrelated` Secret must also exist so the test can check denied access. Stage the
credentials with mode `0600` and pass these environment variables:

```text
CLDT_NATIVE_RECEIVE_API_CONFIG=/staged/worker-kubeconfig
CLDT_NATIVE_RECEIVE_API_SOCKET=/staged/api-relay.sock
CLDT_NATIVE_RECEIVE_API_NAMESPACE=<isolated-test-namespace>
CLDT_NATIVE_RECEIVE_API_NODE_UID=<expected-lab-node-uid>
```

Run the staged test binary with `-test.run '^TestNativeReceiveRealAPI$' -test.v`
under `unshare --net`. The relay carries TLS unchanged and avoids adding guest
routes. The test checks real authorization and resource-version conflicts,
prepared kernel bindings, unchanged active forwarding, and recovery from a
malformed request. Stop the relay and remove the test namespace and its scoped
RBAC resources afterward. It does not move traffic between receivers or qualify
the controller's activation and retirement protocol.

The single-device control expects failed exchanges when sender and receiver
ownership disagree. The dual-receiver case requires exchanges in both
asymmetric directions and checks the source address observed by the receiver.
`expectedOutcomesObserved` validates that experimental contract, including the
negative control; `qualificationEligible` is always false. These kernel
experiments do not test product orchestration, lifecycle recovery, Windows,
IPv6, or deployment safety. Keep the returned JSON with the campaign evidence.

## Real-cluster acceptance

`cmd/acceptance` qualifies an existing cluster from an explicit kubeconfig,
with no bastion or node access. It never reads `$KUBECONFIG` or
`~/.kube/config`.

```sh
go run ./cmd/acceptance -kubeconfig /path/to/kubeconfig.yaml -out new.json preconditions
go run ./cmd/acceptance -kubeconfig /path/to/kubeconfig.yaml -out new.json matrix
```

`preconditions` uses a client whose transport refuses every request but GET,
and reuses the product's network detection. It reports the network and
whether it is dual-stack, masquerade of every node's blocks, whether Calico's
address autodetection could select an address on a remote (only `cidr=`
methods excluding the tunnel ranges pass), the pod MTU against `-tunnel-mtu`
(default 1420), tunnel range overlap, borrowed addresses, explicit BGP peers
and endpoint candidates. Required failures exit nonzero.

`matrix` also runs the preconditions, then creates one namespace labelled
`cloud-provisioning.appmana.com/acceptance=true`, a probe pod and Service per
Ready Linux node (or `-nodes`), runs the full matrix above through pod exec,
and deletes that namespace on exit, including on interruption. It deletes no
namespace without that label. Probe pods pull the pinned agnhost image.
