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

SDK-backed VM/container rigs never interpret a missing saved session as
permission to destroy a same-named Containerlab lab. `Down` is a no-op when no
owned session exists; `Up` lets the SDK reject runtime name collisions. For
container rigs, nonempty bind directories without an owned session also stop
deployment rather than being erased. Recover or relocate legacy lab state
explicitly; normal SDK operation does not adopt or remove it automatically.
The explicit nil-runtime compatibility path remains for legacy callers/tests
and does not provide these ownership guarantees.

This module pins published Labcontainers commit `9fea7eee373a`; no local SDK
workspace or SDK replace directive is needed. The local product controller
module remains a dependency in this repository.

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
native file transfer. Rebuild the product VM wrapper before using this path:
the updated Dockerfile installs `/labcontainers-guest` and retains `/cldt-guest`
as a compatibility symlink. Existing old wrapper images do not have the new
entry point. Image-builder SSH, CoreOS's policy wrapper, and nil-runtime legacy
tests remain separate paths; their migration is not claimed complete here.

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
image and qualifies the command adapter only, not the product image builder or
Kubernetes/Calico networking.

Container commands and file transfers also use the SDK session, including
router/bastion appliances inside a VM lab. Appliance views select their parent
VM session rather than reconstructing Docker container names or looking for a
separate container-rig session. A configured runtime that cannot reconnect fails
explicitly instead of falling back to host CLI execution.

The k0s builder now constructs upstream `v1beta1.ClusterConfig` objects,
including native Calico patch objects. Configuration writing, native argument
installation, and single-attempt readiness probes use
`labcontainers/pkg/kubernetes/k0s`. Product profiles, site orchestration, and
cloud provisioning assertions remain here.

Fresh k0s builds require an explicitly prepared binary and its content pin:
`cluster.Deps.K0sBinary` and `K0sBinarySHA256`, or the lab command's
`--k0s-binary /absolute/path/to/k0s --k0s-binary-sha256 <64-hex-digest>`.
There is no automatic upstream binary download or fallback to a cached build.
Prepare the intended AppMana fork artifact before running a fork scenario.
Reuse of an existing installation does not require a new binary.

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
