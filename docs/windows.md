# Windows workers

Windows Server 2022 (build 20348) and Server 2025 (build 26100) require separate
qualification. Workload networking remains **unqualified for release**: small
and fragmented UDP can be dropped by Windows VFP. Node registration alone does
not establish workload support.

The image configuration uses the patched k0s `v1.36.2+k0s.0.appmana.1` worker,
containerd 2.3.2 and the native Windows tunnel service. Windows Calico uses its
physical cloud NIC for VXLAN. A single-NIC Linux gateway carries traffic between
the WireGuard mesh and that cloud subnet. Attachment requests bind the Machine,
Node and ENI identities, manage shared return routes and scoped ingress rules,
and preserve Calico's native transport address. See
[gateway configuration and removal ordering](windows-gateway.md).

The pinned k0s `v1.36.2+k0s.0` validator rejects Calico/VXLAN with dual-stack
enabled. Its supported dual-stack Calico configuration uses BIRD; that is a
different dataplane from the Windows VXLAN profile tested here. Passing BIRD
validation cannot qualify the Windows VXLAN path. A Windows dual-stack VXLAN candidate needs compatible k0s and Calico
implementations, then a separate VM bringup and source-identity policy matrix. See [the pinned k0s dual-stack configuration](https://github.com/k0sproject/k0s/blob/v1.36.2%2Bk0s.0/docs/dual-stack.md).

The Windows CAPA consumer uses detached EC2Launch userdata so SSM can start
while joining is in progress. EC2Launch's successful agent state does not prove
the detached script succeeded. `bootstrap-installed` marks service installation;
absence of the temporary credential script also cannot prove success because
the consumer removes it in its failure path. The updated CAPA consumer writes an instance-bound terminal receipt, and new
Windows lifecycle rows wait for it before counting a join as complete. Build
the CAPA controller from the updated consumer before running those rows;
legacy consumers do not produce this receipt. Native synthetic success/failure
checks cover both Windows versions, while fresh CAPA receipt-based launches
remain unqualified. The Linux cloud-init observer does not apply to Windows. See [AWS's detached-task semantics](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2launch-v2-task-definitions.html).

## Validation boundaries

For CAPI webhook timeouts on mixed-OS k0s sites, see
[API egress diagnosis and the Linux routing canary](k0s-windows-egress.md).
Node readiness and successful Windows exec do not establish webhook reachability.

Use the [ordinary-pod matrix observer](../harness/e2e/observe/README.md) with the
explicit Node UIDs from the lifecycle stage under test. Finite successful samples
do not supersede earlier failures. Packet captures must inspect both request and response directions. The original samplers were collected and removed, and all 13 original Nodes
remained Ready. This is baseline-failure evidence, not a completed GPU or CAPI
lifecycle test. The original checksum settings, recorded MTUs and Pod identities were preserved;
no offload change is included in worker images or bootstrap. The same six native sampler processes ran across
all phases at a requested 10 ms interval. Disabling this offload did not eliminate
loss, including traffic involving the changed worker. These bounded observations
do not establish a change in failure rate or identify the dropping component. Phase counts are checked against original terminal logs; full logs also retain
preflight and adapter-restart transitions. On Windows 2022,
one request correlates with VFP `Invalid Packet` at `0xE0006096`; another
correlates with TCP/IP `Not locally destined` at `0xE0004136`. The third timeout
has no receiver-side capture in that run. The VFP drop-only bytes differ from
ten earlier appearances only in the inner IPv4 traffic class (`0x00` to `0x74`)
and its checksum; IPv4 and UDP checksums remain valid. This is a diagnostic lead,
not an established cause or mitigation. The earlier captured VFP drop had no
traffic-class change.

Keep raw ETL and native drop metadata alongside the ordinary PCAP, and export
`pktmon etl2pcap capture.etl --drop-only --out drops.pcapng` when comparing bytes
at the drop. PCAP conversion loses drop-report and stack-path information;
see [Microsoft's conversion reference](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/pktmon-etl2pcap).
The parser preserves the native component/reason and reports traffic-class
changes without treating them as proof of causation.

On the tested Windows 2025 build, the
native tool's documented argument sequence with `-1` preservation values also
disabled two unrelated optimization settings; the attempted reversal returned
error 87. Do not bake this command into images or startup scripts. Calico
recreated the disposable probe endpoint, removing the modified port and restoring
every observed flow setting to its original value. This provides no
evidence that reordered-fragment support fixes packet loss.

## Guest bootstrap selection

Set `spec.template.metadata.labels.kubernetes.io/os` on the infrastructure
machine template to identify the guest OS. The claim controller preserves this
label on the generated infrastructure machine. Templates without this label
retain the Linux default.

The join reconciler selects a registered native bootstrap renderer from this
label before requesting join credentials. An unsupported OS fails explicitly.
Set `windowsBootstrap.tunnelSHA256` and `windowsPublisher.image` to register the
k0s Windows renderer alongside Linux. Other distributions do not yet register
a Windows renderer.

A Windows renderer returns raw PowerShell with CAPI Secret `format: powershell`.
The infrastructure provider owns the EC2Launch envelope and transport encoding;
the join reconciler does not wrap or encode the script. Empty payloads and
unknown formats are rejected before publishing bootstrap data or peers.

## Native k0s API load balancing

When enabling k0s node-local load balancing on a cluster that accepts Windows
workers, select its native `Traefik` backend:

```yaml
spec:
  network:
    nodeLocalLoadBalancing:
      enabled: true
      type: Traefik
```

[k0s documents that Envoy is unavailable on Windows](https://docs.k0sproject.io/stable/nllb/).
Envoy can introduce a dependency cycle between loopback API access and CNI
initialization. Use the supported backend, update controller configuration
consistently, and restart existing workers as described by k0s.

## Prepared-image k0s bootstrap

The k0s Windows renderer emits native PowerShell with a base64-encoded JSON plan.
Credentials and infrastructure values are data rather than interpolated shell
code. It requires a prepared image with k0s matching the discovered cluster
version, Containers enabled, the signed WireGuardNT DLL, and a tunnel executable
matching `windowsBootstrap.tunnelSHA256`.

Run the shared image layers `images/windows/prepare-k0s.ps1` and
`prepare-wireguard.ps1`, reboot/verify the image, then stage the built tunnel
executable with `prepare-tunnel.ps1 -Source <artifact> -SHA256 <digest>`. These
layers run during baking. Bootstrap downloads no software and injects no CNI.

At first boot the renderer writes a private mesh identity and Machine name,
registers an automatic tunnel service with restart recovery and its UDP firewall
rule, and installs/starts a k0s worker. It converts the local tunnel address to a
host prefix so Windows does not install a connected route for the shared subnet.
The distribution owns its runtime, CNI and load-balancer behavior. The
`bootstrap-installed` marker records installation only, not Node readiness.

The infrastructure provider supplies either observed `providerID`/`nodeAddress`
values or a native runtime discovery script. AWS uses bounded IMDSv2 requests;
the local VM provider uses its reported identity. The distribution renderer
contains no AWS API calls. Windows AWS discovery currently supplies the primary
IPv4 address; dual-stack node registration remains a separate integration gate.

Renderer, provider-contract and PowerShell parser checks pass. Those AWS
results do not qualify the local VM provider or workload networking. CAPA's
Windows secure consumer must be installed, and images must be cleaned and
generalized before capture.

## Private AWS image capture

`harness/e2e/aws/windows_image.py` provides explicit `prepare`, `generalize`,
`status`, `capture`, `authorize` and `revise` phases for the run's owned Server 2022/2025 builders. Use
the setup AWS identity for EC2 image operations; guest commands use the derived
harness session. Each operation checks account, VPC, ownership tag and Windows
single-NIC metadata before acting.

```sh
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase prepare --service <windows-tunnel.exe>
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase generalize
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase status
# Wait for both builders to stop after generalization dispatch.
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase capture
```

Preparation removes diagnostic state from the run-owned application directory,
preserving only image recipe assets and verification records. It rejects
installed worker/test services, remaining tunnel adapters and unexpected
reparse points. Generalization uses a self-removing scheduled task to invoke
`EC2Launch.exe sysprep --clean --shutdown`, following
[AWS's EC2Launch v2 image process](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/sysprep-using-ec2launchv2.html).

Capture requires a recorded generalization dispatch and a stopped builder. It
creates private test candidates and records image IDs immediately in the run
inventory used by cleanup. Run `status` until images are available; it records
their snapshots and public/private status. An image becoming available is not
proof of successful fresh boot or Kubernetes joining. Validate new instances
from each candidate through the CAPI matrix before promoting the image.

If first-boot validation exposes a baked executable defect, use `--phase revise --service <new-executable>` with the same run arguments. This requires the
previous capture to be available, private and owned, and the builder stopped.
The helper retains the previous image record, clears old diagnostic userdata,
and starts the builder for an explicit image revision. Wait for SSM, validate
the corrected service on both guest builds, then repeat prepare, generalize,
capture, status and authorization. Recreate failed pre-launch claims with the
new AMIs; do not pair a new bootstrap checksum with an old image.

To replace k0s as well, supply a
[packaged worker artifact](../providers/k0s/README.md#build-a-complete-windows-worker)
and its independently verified identity on both revision and preparation:

```sh
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase revise --service <windows-tunnel.exe> \
  --worker <packaged-k0s.exe> --worker-sha256 <verified-sha256> \
  --worker-version v1.36.2+k0s.0.appmana.1
# Once both builders are running and reachable through SSM:
python3 aws/windows_image.py --work-dir <private-run-directory> \
  --awsnode <awsnode-binary> --phase prepare --service <windows-tunnel.exe> \
  --worker <packaged-k0s.exe> --worker-sha256 <verified-sha256> \
  --worker-version v1.36.2+k0s.0.appmana.1
```

Worker-only changes can trigger a revision without changing the tunnel binary.
The helper records the requested worker hash/version, rejects mismatched inputs
when resuming, and verifies the guest receipt before allowing capture. Omitting
the worker options preserves the builder's existing k0s executable. The shared
preparation layer refuses to modify an installed worker; use an unjoined builder
and retain the separate live-worker rollout procedure for integration tests.

For an already unpacked runtime cache, create a content-addressed recipe on the
build host. Use the runtime manifest from `images/windows/stage_k0s_runtime.py`
and repeat `--image` for every intended digest-pinned image:

```sh
python3 images/windows/cache.py --runtime /path/to/runtime.json \
  --windows-version 2022 --image docker.io/your/image@sha256:VERIFIED_DIGEST \
  --output /path/to/new-cache-recipe.json
```

Pass `--cache-recipe /path/to/new-cache-recipe.json` and one `--windows-version`
to both `aws/windows_image.py --phase revise` and `--phase prepare`, along with
the existing work directory, executor and service artifact arguments. Add
`--variant gpu` for the GPU builder. GPU cache revisions preserve the recorded
CPU base, worker and driver; revise the CPU layer first to change those binaries.
The helper retains previous CPU and GPU captures in separate histories.

If an unprepared cache revision is rejected, retain its native failure and
confirm the original operation and its cleanup are terminal. Inspect the actual
cache before reusing any partial downloads; registered images alone do not prove
native unpacking. Stop the unjoined builder, then pass
`--supersede-cache-sha256 OLD_REQUESTED_RECIPE_SHA256` with `--phase revise` and
the corrected recipe. This explicitly replaces the matching pending intent and
records it in `supersededCacheRevisions`. It rejects a running builder, prepared
or captured state, a stale intent hash, and changes to the service, worker or
runtime. The old capture must remain private, owned and available. The option
does not clean up guest state or qualify an image: native preparation, Sysprep
and fresh-clone checks remain required.

Preparation starts a temporary standalone runtime to inspect the existing
cache, then stops and unregisters it. It performs no image pull or import.
Missing layers, unexpected images, runtime mismatches or remaining workloads
fail preparation. Continue through generalize, capture, status and authorization
only after it succeeds. Cache recipe hashes distinguish capture names. This
workflow still requires a fresh CAPI clone to verify that Sysprep preserves
usable snapshots and that workload startup avoids downloading or extracting them.

Keep the k0s data directory's normal inherited Windows permissions. Do not copy
the administrator-only bootstrap directory ACL onto `C:\var\lib\k0s`: newly
created Traefik configuration inherits it, and the distro's Local Service
HostProcess container cannot read its configuration. Containerd protects its
own store separately. Cache preparation and capture require directory-permission
evidence as well as unpacked images; older readiness receipts do not satisfy
this gate. Regenerate recipes with the current helper: schema version 2 includes
the inherited-permissions layout, giving corrected captures distinct names. A manually repaired worker does not qualify the original image. The inherited ACL
also affected Konnectivity's projected service-account files; this was a file
access failure, not evidence that Windows cannot use projected tokens.

Before creating claims from a patched k0s image, configure its expected worker
identity in the controller as well:

```yaml
windowsBootstrap:
  tunnelSHA256: VERIFIED_TUNNEL_SHA256
  workerVersion: v1.36.2+k0s.0.appmana.1
  workerSHA256: VERIFIED_PACKAGED_WORKER_SHA256
```

The corresponding flags are `--windows-worker-version` and
`--windows-worker-sha256`. Supply them together with Windows bootstrap enabled.
The renderer permits a build suffix on the cluster's exact k0s release; it
rejects another Kubernetes patch version or k0s revision. The guest verifies both
the expected version and binary hash before creating node identity. Without an
override, it requires the control-plane release version. Baking a patched worker
alone does not update the controller's bootstrap contract.

## CAPI test claims from captured images

Before a fresh claim, renew the test run's private image access if its pull
sessions have expired. Existing running Pods can conceal expired credentials;
a new worker still needs the exact configured Calico and publisher images.
Run these helpers with the authorized setup environment from `source-me.sh`:

```sh
python3 aws/publisher.py --work-dir <private-run-directory> \
  --renew-pull-only --component calico-node-windows \
  --api-server https://<isolated-site-api>:6443
python3 aws/publisher.py --work-dir <private-run-directory> \
  --renew-pull-only --component windows-publisher \
  --api-server https://<isolated-site-api>:6443
```

The helpers verify repository ownership and retain the recorded immutable
digests. Only derived pull credentials enter Kubernetes. For minimal startup
downloads, include those same digests in the prepared image's verified cache;
bootstrap completion alone does not establish CNI availability.

After `windows_image.py --phase status` reports both images available, run
`windows_image.py --phase authorize` with the same run directory and awsnode
arguments. This checks live image and CAPA role ownership and adds the two exact
AMI ARNs to the run policy, retaining its existing Linux AMI permission. Then import the observed site with `cmd/importsite` and publish the derived CAPA
identity with `aws/identity.py`. Create each worker through the claim helper:

```sh
python3 aws/claim.py --work-dir <private-run-directory> \
  --api-server https://<isolated-site-api>:6443 \
  --name windows-2022 --windows-version 2022
python3 aws/claim.py --work-dir <private-run-directory> \
  --api-server https://<isolated-site-api>:6443 \
  --name windows-2025 --windows-version 2025
```

The helper selects the corresponding captured AMI, adds the infrastructure
machine OS label, and requests an encrypted 50 GiB root disk by default. It
retains CAPA’s Secrets Manager bootstrap transport. CAPA launches the instance;
the helper does not synthesize Machine or Node readiness. Image availability is
checked from the latest recorded status; AWS remains authoritative at launch.
Without `--windows-version`, the existing Linux AMI selection is retained.

Use `aws/windows_join.py` to record each fresh claim’s CAPI association, owned
single-ENI instance, guest build, physical NICs, native services and publisher
Pod readiness. Pass the same run, explicit API server, awsnode binary, claim name,
Windows version and a new `--output` JSON path. The observer installs nothing;
it returns a failing status for incomplete joins and preserves the observations.
CAPI v1beta2 identifies the associated Node by name. The observer checks its
provider ID and the Machine’s Running phase, and records the actual Node UID
for later replacement comparisons. A passing join checkpoint does not establish
traffic, replacement or NetworkPolicy isolation. Add `--require-peer-delivery`
to require a fresh native receipt matching the current Secret UID, request nonce
and payload hash, plus the publisher's exact acknowledgement in Kubernetes.
Without that option, Pod readiness does not establish peer delivery.

The observer hashes the executable of every running product tunnel service as
well as the original baked binary. Both must match the recorded image recipe
by default. For an intentional staged canary, pass
`--expected-running-tunnel-sha256 <candidate-sha256>`; this changes only the
running-service expectation. The baked worker and tunnel artifact checks still
apply, and the report records service process IDs and executable hashes. A
passing canary observation does not qualify the candidate as a baked image.

## Native Windows tunnel backend

`controller/pkg/tunneldevice` separates peer-plan validation from the Windows
WireGuardNT adapter. It consumes the existing `tunnel.PeersFileDoc` contract;
allowed IPs and node host routes remain separate. The backend owns a dedicated
adapter, sets its addresses and routes, and removes obsolete routes without
modifying the physical NIC or CNI. Its current MTU is 1420.

This backend is exercised by `cmd/tunnelprobe` and the host service described
below. The publisher and bootstrap controller deliver peer updates on joined
Windows nodes; the join observer checks native receipts against the Kubernetes
Secret. API balancing and CNI isolation require separate checks.

Peer updates explicitly remove retired peers and update surviving peers in
place. `Apply` repairs missing identity addresses as well as route drift;
`Verify` requires the configured address to exist and be preferred in the
Windows network stack. A cached identity or successful address-add call does
not establish that condition. Duplicate-address detection is disabled on the
owned WireGuard adapter, matching upstream WireGuard's static tunnel setup;
physical NIC settings remain unchanged.

Do not use that
experiment as a deployment configuration. The [handover design](tunnel-handover.md)
requires the stable address owner to outlive individual tunnel generations. The address-verification change still needs workload/image rollout validation.

To run the focused native regression, cross-compile `pkg/tunneldevice` with
`GOOS=windows GOARCH=amd64 go test -c -cover ./pkg/tunneldevice`, place the
executable alongside the signed WireGuardNT DLL on a test VM, and set
`CLDT_NATIVE_PEER_CONTINUITY=1` before running
`-test.run ^TestNativeApplyLifecycle$ -test.v`. It checks address/route repair
and rejection of duplicate ownership without disrupting the existing adapter.
The retired shared-address acceptance experiment is preserved in the native
address report's evidence archive. Use the stable-owner experiment for
generation lifetimes and retain the active duplicate-owner rejection regression.

Build the instrumented probe from `controller`:

```sh
GOOS=windows GOARCH=amd64 go build -cover \
  -coverpkg=github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice,github.com/appmana/cloud-provisioning/controller/cmd/tunnelprobe \
  -o /tmp/tunnelprobe.exe ./cmd/tunnelprobe
```

Include the command package in instrumentation so the executable emits runtime
coverage files. With the two prepared Windows VMs and the signed WireGuardNT
1.1 DLL and its license extracted from the official SDK, run from `harness/e2e`:

```sh
go build -o /tmp/awsnode ./cmd/awsnode
python3 aws/windows_tunnel.py --work-dir <private-run-directory> \
  --awsnode /tmp/awsnode --probe /tmp/tunnelprobe.exe \
  --driver <path-to-amd64-wireguard.dll> --driver-license <path-to-LICENSE.txt> \
  --family ipv6 --output <new-private-evidence-directory>
```

Repeat with `--family ipv4` and a fresh output directory. The local Python
orchestrator requires `cryptography`. It checks run ownership, OS and one EC2
network interface before dispatch, then checks the physical NIC count inside
each guest. It installs no packages at probe startup and removes its firewall
rules when the probe finishes. Coverage files are saved beside each result.

The [CAPA extension](../providers/capa/README.md) implements the Windows consumer
for CAPA-owned Secrets Manager chunks; this requires the forked CAPA controller.

## Native tunnel and API services

`controller/cmd/windows-tunnel` runs under the Windows Service Control Manager
and owns one WireGuardNT adapter for its lifetime. It reports Running after
initial peer configuration succeeds, accepts Stop/Shutdown, and closes its
adapter before reporting completion. Pending operations report checkpoints
following the [Windows service state contract](https://learn.microsoft.com/en-us/windows/win32/services/service-status-transitions).

The host service reads an immutable bootstrap identity from `--peers-file`.
`--updates-file` accepts the existing public `tunnel.PeerListDoc` shape, containing
`peers` and optional `apiServers`, without private identity fields. Valid updates
are persisted to `--cache-file` before they reach the native device. All three
paths must be distinct absolute paths within a SYSTEM/Administrators-only
directory supplied by image preparation/bootstrap. Publish updates atomically.

An explicit empty `peers` array removes all peers and remains authoritative after
a service restart. Missing or malformed peer arrays are rejected. At startup,
the service uses the update file, then the durable cache, then the bootstrap
snapshot when neither public file exists. Corrupt existing public state fails
startup; it does not silently restore bootstrap peers. During operation, an
invalid update preserves the last applied state. Unchanged polls do not reset
live peers, and failed native operations retry on subsequent polls.

The same executable runs the independent API service with `--api-proxy-only`
and `--api-proxy-port=<loopback-port>`. Install it as a separate SCM service
with its own `--service-name`, sharing the identity/cache paths. This mode never
creates an adapter. It uses `controller/pkg/apiproxy`, also used by the Linux
dialer, to forward TCP without terminating TLS. A failed backend is skipped on
new connections; endpoint updates affect subsequent connections. The proxy
retains its serving list if a cache read fails. Numeric backend endpoints are
validated before public state is persisted.

Build the instrumented service from `controller`:

```sh
GOOS=windows GOARCH=amd64 go build -cover \
  -coverpkg=github.com/appmana/cloud-provisioning/controller/cmd/windows-tunnel,github.com/appmana/cloud-provisioning/controller/pkg/tunnelhost,github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice,github.com/appmana/cloud-provisioning/controller/pkg/apiproxy \
  -o /tmp/windows-tunnel.exe ./cmd/windows-tunnel
```

With the prepared single-NIC Windows VMs running, execute from `harness/e2e`:

```sh
python3 aws/windows_service.py --work-dir <private-run-directory> \
  --awsnode /tmp/awsnode --service /tmp/windows-tunnel.exe \
  --driver <path-to-amd64-wireguard.dll> --driver-license <path-to-LICENSE.txt> \
  --coverage --output <new-private-evidence-directory>
```

The verifier creates a unique manual-start service, collects runtime coverage,
and removes successful runs' services and guest artifacts. Failures preserve
private diagnostic artifacts while attempting service removal. Stop the VMs
when testing is finished; stopped instances retain billable disks.

## Peer delivery and acknowledgment

`controller/cmd/peer-publisher` reads the Machine's adoption Secret and submits
its public payload to the host service through `--request-file`. Enable the
matching `--request-file` and `--receipt-file` on the tunnel service. These paths
are additional to the bootstrap, diagnostic update and durable cache files.
A present request takes precedence over diagnostic updates. Keep every path
distinct within the protected host directory.

Each request carries a fresh delivery nonce, Secret UID and exact payload bytes.
The host validates the full native configuration, persists desired state, applies
it, then writes a receipt containing the nonce, UID, payload hash and timestamp.
Receipts are withdrawn before applying another request and on service stop.
Native apply failures invalidate the in-memory success cache so a later revert
cannot skip repairing partially changed kernel state.

The publisher acknowledges `AppliedListAnnotation` only for a matching receipt
no older than 30 seconds. Repeated payloads after an intervening change receive
new nonces; recreated Secrets receive new UID-bound requests. The acknowledgment
patch includes the observed Secret resourceVersion and UID. An isolated real
Kubernetes API test verifies that a concurrent update causes a conflict instead
of acknowledging an obsolete list. This preserves the controller's existing
endpoint-retirement acknowledgment contract.

The publisher receives `--token-file` and `--ca-file` from pod-projected
credentials, and reads the Machine name from `--machine-name-file`. It neither
reads the bootstrap private key nor configures an adapter. Client-go reads the
bearer token through its file-based credential transport. Windows HostProcess
volume mounts, including service-account credentials, support their configured
mount paths with containerd 1.7 and later; see the
[Kubernetes HostProcess volume documentation](https://kubernetes.io/docs/tasks/configure-pod-container/create-hostprocess-pod/#volume-mounts).

Reproduce the rotation check after the publisher has run for more than one
requested token lifetime:

```sh
cd harness/e2e
python3 aws/windows_token.py --work-dir .state/aws/<run> \
  --awsnode .state/awsnode --api-server https://<isolated-site-api>:6443 \
  --name <capi-machine> --output .state/aws/<run>/token-rotation.json
```

The observer verifies the CAPI/Node association and reads the projected volume
through native SSM. `--kubelet-root` defaults to `C:\var\lib\k0s\kubelet`.
It removes only the peer acknowledgement, guarded by the Secret UID and
resourceVersion, then requires the running publisher to acknowledge the same
payload. It rejects token expiry, future issue timestamps, publisher restarts,
Secret replacement and changes to the acknowledged payload. A failed run leaves
the actual acknowledgement state visible; the observer never fabricates one.

## HostProcess publisher deployment

Set Helm value `windowsPublisher.image` to a digest-pinned publisher image.
The controller creates a separate Windows remote-publisher DaemonSet using the
existing mesh ServiceAccount and adoption-Secret RBAC. It selects Windows amd64
nodes with the cloud-worker role and tolerates the cloud-worker taint. Its Pod
uses HostProcess and the host network, with a one-hour projected API token and
the namespace's Kubernetes CA. No static credential Secret is needed.

Set `windowsPublisher.apiServer` to an API URL reachable from the Windows host
network. For k0s with native Traefik node-local load balancing, use
`https://[::1]:7443`, matching the worker's generated NLLB kubeconfig. This avoids
depending on the Kubernetes Service IP for tunnel peer delivery. An empty value
uses the Kubernetes Service environment variables. The configured endpoint must
validate against the projected cluster CA; certificate verification remains
enabled. This setting changes only the publisher's API destination and retains
the projected service-account token.

The host directory is `C:\ProgramData\CloudProvisioning\<mesh-interface>`.
Bootstrap must create it, write `machine-name`, and configure the native service
to use its `request.json` and `receipt.json`. The Pod requires this directory to
exist; it does not create an unprotected replacement. Disabling the image setting
stops reconciliation of the Windows publisher and leaves an existing DaemonSet
serving current nodes. Removing the owning release garbage-collects it.

Build the image from the repository root on a Linux buildx builder:

```sh
docker buildx build --platform windows/amd64 \
  -f controller/Dockerfile.windows-publisher \
  --output type=oci,dest=windows-publisher.oci.tar .
```

The recipe pins the Go builder and Microsoft's small HostProcess base image
by digest, following the
[SIG Windows HostProcess build pattern](https://github.com/kubernetes-sigs/sig-windows-tools/blob/master/hostprocess/csi-proxy/Dockerfile.windows).
The output contains the Windows publisher executable and requires no package
installation at pod startup. Configure a registry reference after importing or
publishing the image through your normal distribution process.

The image build and Windows/amd64 OCI metadata are verified. Both the DaemonSet
and its actual Pod pass isolated Kubernetes API admission; controller tests
verify image updates. Repeat those checks for each changed image; admission alone
does not establish successful execution or delivery.

## Repeat the native bootstrap checks

Use an isolated [AWS test environment](aws.md), with a private asset bucket and
renewed harness session. The harness IAM policy now permits both AWS-owned SSM
command documents, with the same run-tag restriction on instances.

From `harness/e2e`:

```sh
go run ./cmd/windowsprobe -render-userdata -output /tmp/cldt-windows-userdata.xml
# With the authorized AWS setup identity:
python3 aws/windows_probe.py --work-dir <private-run-directory> \
  --userdata /tmp/cldt-windows-userdata.xml
# After EC2Launch finishes and SSM becomes available:
go run ./cmd/windowsprobe -work-dir <private-run-directory> \
  -output /tmp/cldt-windows-results.json
```

Use new output paths. The launcher resolves Amazon's available, licensed Windows
base images, records exact IDs, and creates two `t3.large` diagnostic VMs with
one ENI each. It does not create CAPI workers. Cleanup uses the same recorded VPC
and run ownership tag as the Linux test environment:

```sh
python3 aws/cleanup.py --work-dir <private-run-directory>
python3 aws/verify_cleanup.py --work-dir <private-run-directory>
```

## Userdata boundaries

The shared `controller/pkg/bootstrap` package represents native payload bytes and
their format independently of the cloud and Kubernetes distribution. Windows
EC2Launch needs its own envelope around the PowerShell payload.

EC2Launch v2 passes the contents of `<powershell>` to PowerShell without decoding
XML entities. An XML round-trip test therefore gives the wrong result: escaped
quotes and newlines can become PowerShell syntax errors. The renderer instead
uses an ASCII wrapper that decodes the original script as UTF-8. It preserves
Unicode and prevents text inside the script from closing the envelope's tags.

That inner encoding is separate from API transport encoding. AWS CLI's
`RunInstances` handler encodes userdata itself, including when supplied through
`--cli-input-json`. Give it the unencoded envelope. Passing base64 text caused
EC2Launch to reject the document as an unrecognized format in the real tests.
SDK and CAPI adapters must follow their own serialization contract rather than
copying the CLI boundary.

Keep this behavior shared with the future local Windows VM bootstrap consumer.
Do not replace native first-boot execution with a successful SSM command and
call that CAPI bootstrap parity.

For licensed GPU images, runtime ownership, graphics tests and image reuse, see
[GPU workers](gpu-workers.md) and [image recipes](../images/README.md).
