# Single-NIC VM testing

The Go harness in `harness/e2e` runs each Kubernetes node in its own KVM guest
with one physical Ethernet NIC (`ens2`, PCI alias `enp1s2`). QEMU Guest Agent
uses virtio-serial for command execution and file input. Guests have no
management NIC, management bridge or user-mode NAT. Containerlab supplies
external segments and QEMU wrappers; routers, edges and the bastion are
container appliances. Runtime assertions verify guest hardware and isolation.
Lifecycle replacements repeat the physical-NIC and KVM assertions before
accepting each new Node identity.

The fake infrastructure provider currently reports the guest's routed address
as both `ExternalIP` and `InternalIP`. It does not model EC2's separate public
endpoint and private NIC address, or overlapping private subnets across clouds.
Those address-domain cases require real cloud validation until the VM topology
and provider model support them together. Changing reported status alone would
not reproduce the corresponding routing behavior.

The fake provider checks remote-slot bindings across its namespace before each
reconcile pass. Two infrastructure Machines bound to the same slot, including a
terminating Machine, block the pass before address publication or VM changes.
This protects fixed-template tests; it does not allocate slots for generated
node-group names. A durable pool allocator remains required for those tests.

Out-of-band workload probes isolate pod-network behavior from API-server-to-
kubelet access. AWS rows require a separate exec/logs check through every
control plane and record it in `kubelet-access.json`. See the
[MicroK8s address validation](microk8s-kubelet-address-results.json) for the
coverage and scope of this requirement.

The harness imports its locally built Linux dialer images into each guest's
container runtime and sets `dialerImage.pullPolicy=Never`. The chart defaults to
`IfNotPresent` for deployments that can pull images. A remote join is accepted
only after its current adoption DaemonSet Pod is running and Ready with the
desired image and pull policy, and its adoption Secret acknowledges the current
peer-document hash. VM and AWS rows use the same Linux adoption checks.
The [adoption validation report](microk8s-adoption-fix-results.json) distinguishes
observed native results from unit coverage and unfinished campaign gates.
Survivors are checked again after removal and replacement. These receipts prove
configuration acknowledgment; the traffic matrices separately test connectivity.

While waiting for registration, the harness can ask a machine adapter whether
bootstrap has failed. Ubuntu VMs inspect cloud-init through serial management;
Linux AWS workers inspect it through SSM. A verified terminal error stops the
wait. An unavailable observer or transport error is inconclusive and does not
change the instance's CAPI provisioning status. Windows does not run this Linux
probe. See the [bootstrap observer tests](bootstrap-health-results.json).

## Run a profile

From `harness/e2e`:

```sh
make platform-image
go run ./cmd/lab -rig vm -distro k0s -cni calico -product \
  -remotes remote1,remote2 -check -work-dir .state/platform
```

Use a separate work directory for each fresh distribution configuration. Place
the platform disk at `<work-dir>/platform-node.qcow2`; a symlink is supported.
kubeadm additionally needs `kubeadm-node.qcow2`, built with
`make kubeadm-image STATE=.state/kubeadm`.

`-placements` requires `-check` and validates every placement name before
provisioning. A list containing only commas and whitespace is rejected. Omit
`-placements` to run the initial two-worker placement.

The shared lab name is `cldt`; run lab commands serially. The command holds
`/tmp/cloud-provisioning-cldt.lock`. Generated disks, credentials, logs and
captures belong under ignored `.state/` directories.

To retain an existing cluster while running a second profile, use the
[isolated VM runner](../../harness/e2e/runner/README.md). A new work directory
alone does not isolate the fixed host bridges and routes.
The [k0s 1.36 kube-router site result](k0s-1.36-kuberouter-site-results.json)
records a fresh five-node site with bundled kube-router 2.10.0, containerd 2.3.2,
and unchanged retained-cluster identities. Its scope excludes remote lifecycle
and packet-matrix qualification.
The separate [lifecycle and placement result](k0s-1.36-kuberouter-lifecycle-results.json)
records both remote CAPI removal/replacement cycles and all four endpoint
placements on that verified site: 1,324 converged checks across ten matrices.
Each replacement received a new Node UID; the five site UIDs stayed unchanged.
Placement checks require both published membership and kernel WireGuard devices
to match the requested endpoints. These are convergence results, not continuous
packet-loss measurements.

New lab runs also write a `matrix-pass` event for every completed convergence
attempt, including failures followed by a passing retry. `series` identifies a
convergence window; `attempt` starts at one within that window. Events retain
the source, destination, check kind, result and error. The final `matrix` event
remains the acceptance result. Inspect the pass events when a placement takes
time to converge; a final green matrix does not imply uninterrupted traffic.
The earlier 1.36 lifecycle report predates these pass events and cannot recover
its intermediate observations retrospectively.

The [k0s 1.36 sole-endpoint cut result](k0s-1.36-kuberouter-sole-endpoint-cut-results.json)
verifies serial management with the sole physical NIC down. All 52 required
survivor checks passed during site isolation; 50 site-dependent paths were
reported as not required, rather than counted as successes. Restoring the NIC
returned the full 140-check matrix. This result covers one cut row, not the
remaining outage campaign or continuous packet-loss qualification.

The [twelve-row outage result](k0s-1.36-kuberouter-outage-results.json) covers
cuts and reboots of the selected site worker and both remotes under one-worker
and two-worker placements. Across 39 final matrices, 4,904 required checks
passed and 100 site-dependent checks were explicitly not required. One returned
matrix initially failed 15 probes before recovering; its post-pass route/device
snapshot and regression replay preserve that observation. The replay verifies
failure retention, not the cause or timing of recovery. Other victims and
placements, and continuous packet-loss guarantees, require separate evidence.

Minimal guests need not contain the `wg` CLI. The cross-platform
[tunnel observer](../../controller/cmd/tunnelobserve/README.md) reads peer
counters through netlink on Linux and the driver API on Windows. Keep observer
exit codes with captures; an unavailable CLI is not evidence of absent peers.

| Distribution | CNI selection | Ownership |
| --- | --- | --- |
| k0s | calico | explicitly selected bundled Calico VXLAN |
| k0s | kube-router (default) | bundled native BGP |
| k3s | flannel (default) | bundled Flannel VXLAN |
| RKE2 | canal (default) | bundled Canal |
| MicroK8s | calico (default) | bundled Calico; native snap revision |
| kubeadm | calico | explicit standalone regression profile |

`-cni default` selects the distribution's configured default independently of
profile ordering; k0s uses Kube-router. Use `-cni calico` for the supported
Calico profile. Unsupported combinations
fail before deployment. Bundled profiles observe the installed network rather
than injecting a separate CNI. The
[native k0s configuration report](k0s-default-contract-results.json) records the
observed default and accepted dual-stack combinations.

The [MicroK8s lifecycle result](microk8s-lifecycle-results.json) covers native
v1.34.9, snap revision 9063, bundled Calico 3.29.3 and containerd 1.7.29.
Seven single-NIC KVM guests run on an isolated AWS host. Both remote CAPI
replacement cycles and the four placement matrices passed 1,324 checks across
ten matrices, all on their first attempt. Native harness coverage is 1,506/3,248
statements. The infrastructure provider is the VM test provider; hosting the
runner on AWS does not qualify CAPA worker provisioning. Continuous traffic
and lifecycle cycles under every placement require separate campaigns.

For a fresh k0s Calico site, `-k0s-calico-mtu 1370` configures the bundled
overlay through k0s's supported configuration. The value is an explicit path
budget, not a universal default. Zero leaves distribution defaults intact;
positive values must be 1280–65535 and are rejected for other profiles or
`-reuse-site`. This flag does not install patched Windows Felix. See
[MTU configuration](../windows-gateway.md#configure-the-k0s-mtu) and the
[native TCP/UDP results](k0s-windows-mtu-results.json).

## CAPI and lifecycle contracts

The default `-capi-mode imported` observes the site's API, supplies an
ImportedControlPlane and a connection Secret, and lets CAPI initialize its
Cluster and associate Machines with Nodes. The connection uses the existing
Kubernetes Service and adds no management network. Each remote must reach CAPI
Running with the expected Node name and matching providerIDs. This is an
explicit harness import, not automatic product discovery of arbitrary clusters.
The infrastructure endpoint uses the distribution's API port: 16443 for
MicroK8s and 6443 by default. Association through the Kubernetes Service does
not validate that separately advertised endpoint; the
[MicroK8s endpoint check](microk8s-endpoint-results.json) covers that contract.

The local infrastructure provider consumes each Machine's bootstrap Secret
reference and owns VM creation and deletion. The VM's cloud-init or Ignition
consumes the rendered bytes. The harness does not rewrite join commands or
patch `Machine.status.nodeRef`.

`-check` includes deleting and re-adding each remote. Deletion must remove the
claim's Machine, infrastructure machine, Node, bootstrap Secret and peer entries;
re-addition must produce a new Node UID and restore the datapath. Product
finalizers remain in effect. `-lifecycle=false` selects a datapath-only run.
With `-placements`, each selected placement must settle and pass its baseline
matrix before both removal/replacement cycles run there. Lifecycle events name
the placement, and endpoint membership and kernel devices are checked before
each removal and after each rejoin. Without `-placements`, lifecycle tests use
the initial two-worker placement. Earlier reports that describe initial-placement
lifecycle followed by placement-only matrices retain that narrower scope.
`-capi-mode unconnected` exercises the providerID fallback without claiming CAPI
Node association.

Measurement code accepts `rig.Nodes`, which resolves machines without owning
their creation or teardown. `rig.Fleet` can combine a local site with explicitly
bound EC2 nodes. `rig.Rig` adds lab lifecycle operations; the CAPA controller
continues to own EC2 lifecycle. CAPI association compares the observed Machine
and Node providerIDs without requiring a local-provider prefix.

| Machine implementation | Bootstrap owner and format | Guest commands | Failure operations |
| --- | --- | --- | --- |
| Ubuntu QEMU VM | local infrastructure provider; cloud-init | serial QGA | physical NIC cut/restore, VM power |
| SCOS QEMU VM | local infrastructure provider; Ignition | serial QGA with SCOS execution wrapper | physical NIC cut/restore, VM power |
| EC2 | CAPA; provider-native first boot | SSM over the existing ENI; private S3 staging for stdin | EC2 power API; physical NIC cuts explicitly unsupported |

Bootstrap dispatch uses the machine's `BootstrapConsumer` capability and rejects
unsupported formats before applying data. Supplying bootstrap data to an already
running EC2 adapter is an error: that operation belongs to CAPA's launch workflow.
Implementing an adapter does not establish that its complete network or outage
matrix has passed.

## Campaigns and outages

```sh
go run ./cmd/campaign -work-dir .state/campaign-001 \
  -profiles k0s/calico,k0s/kube-router,k3s/flannel,rke2/canal,kubeadm/calico \
  -outage remote1:cut,remote1:reboot
```

The campaign runs profiles serially, rebuilding the site for each row. It stops
at the first failure so the cluster remains available for diagnosis;
`-keep-going` permits replacement of that lab for later rows. Reports include
the source revision, dirty-worktree status, harness binary hash and input disk.

The matrix checks ordered pairs by pod and Service address, large transfers,
cluster DNS and external access. Placement rows require published membership
and actual WireGuard devices to match the selected endpoints before outages.
An outage requires a healthy baseline, expected survivor connectivity and full
recovery. A physical NIC cut leaves the guest running; a VM reboot exercises
first-boot-independent tunnel recovery from durable state.

`-outage all` selects cuts and reboots for every measured node under each
placement. When the only site endpoint is down, cross-site paths and remote
cluster DNS are explicitly not required; direct remote-to-remote and external
traffic remain required. Unavailable checks do not count as passes. The direct-IP
external probe is `https://1.1.1.1/cdn-cgi/trace`, avoiding a DNS dependency during
intentional site isolation.

`cmd/lab -reuse-site` invokes the distribution's `cluster.SiteReuser` capability.
k0s verifies its version and controller network configuration; MicroK8s verifies
the snap version/revision and renews a native join credential. Reused sites do
not establish a fresh isolation or bringup proof. Prior event files are archived.

## Evidence and coverage

Each run writes JSON events and observations in its own work directory. A pass
requires the run's successful result and gates; old logs are not evidence for a
new run. Automated observations exclude Secrets. Bootstrap material and private
peer state must remain in private, ignored runtime files.

The [five-profile results artifact](single-nic-v4-results.json) contains the
completed k0s/Calico, k0s/kube-router, k3s/Flannel, RKE2/Canal and kubeadm/Calico
VM campaign: 170 matrix gates, 10 replacement cycles and 40 remote1 cut/reboot
cases across four placements. Each final matrix is 140/140. This artifact does
not cover MicroK8s, OKD remote joining, AWS, or every possible outage victim.

Run `make coverage` from `harness/e2e` for controller and harness short
suites. Campaigns build an instrumented harness with a per-process `GOCOVERDIR`:

```sh
go tool covdata percent -i <row-dir>/coverage
```

Runtime harness coverage does not instrument Kubernetes or CNI binaries. Abrupt
process termination may prevent counters from flushing. Captured, sanitized CNI
objects under `controller/pkg/cni/testdata` drive interpretation regressions;
they complement live datapath tests. [Code ownership](code-ownership.md) maps
superseded paths to Git history for cleanup review.

## OKD and SCOS

The OKD lab pins `4.21.0-okd-scos.11` and uses the official agent installer for a
compact three-controller site with bundled OVN-Kubernetes. It verifies release
asset checksums and preserves the installer Ignition while adding serial lab
management. SELinux remains enforcing.

```sh
bash cluster/okd/fetch-assets.sh
docker build -t cloud-provisioning/scos:single-nic rig/vm/scos
docker build -t cloud-provisioning/okd-installer:lab \
  -f cluster/okd/installer.Dockerfile cluster/okd
go run ./cmd/okdsite -prepare-only -work-dir .state/okd-site
go run ./cmd/okdsite -work-dir .state/okd-site
# Resume installed disks without rerunning the installer:
go run ./cmd/okdsite -resume -work-dir .state/okd-site
```

Installed OVN hosts hold their address on `br-ex`, not on physical `ens2`.
Restore operations must only bring the physical link back up and leave addresses
to the distribution. The SCOS adapter uses the same serial transport as Ubuntu,
with the policy-labelled guest execution wrapper required by SCOS.

The site installer, installed-disk resume, single-NIC management, OVN detection
and socket-free worker Ignition export have been exercised. Remote worker join
and OVN traffic through WireGuard remain under development. The exporter invokes
the pinned Machine Config Operator handler through authenticated Kubernetes exec;
it does not open a server port or bypass the distribution's MCS firewall rules.
See [OKD exporter](../../harness/e2e/cluster/okd/exporter/README.md).

## AWS

AWS provisioning is a separate CAPA integration using EC2 instances. Local VM
passes do not prove it. The [AWS k0s/Calico results](aws-k0s-calico.md) cover
real CAPA-owned EC2 workers, replacement and endpoint placements. See
[AWS setup and IAM policies](../aws.md). Machine
implementations must expose their actual management and failure capabilities;
SSM access over an ENI cannot be represented as serial out-of-band access.
