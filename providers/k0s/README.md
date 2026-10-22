# k0s provider patches

## Exec and attach stream completion

`exec-streaming.patch` targets k0s `v1.36.4+k0s.1`, commit
`c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60`. It pins the API-server,
client-go and streaming modules to the Kubernetes 1.36.4 backport at
`c274d6abd2003705d0c71504aeb0fbccda470a48` of
[upstream PR #142461](https://github.com/kubernetes/kubernetes/pull/142461).
The server lets clients drain buffered output before closing, and clients
reject a missing command status instead of reporting success.

The k0s module patch alone does not update its embedded executables. Build
both the API-server and kubelet from the same Kubernetes revision using the
upstream Makefile and the distribution's build flags. With
`ExtendWebSocketsToKubelet`, nodes advertise WebSocket support and the API
server forwards exec requests to the kubelet's stream translator. Updating
only the API-server leaves that path unpatched. Replace the embedded kubelet
on every worker platform, including Windows, and on controllers that also
run workloads. Containerd also needs the
patched CRI-streaming server: the 2.3.5 integration is pinned at
`7b2fc110dd8a63fdc87a6809c256066c1082d7c1`. Use k0s's existing ZIP builder
and embedding targets to assemble the complete controller and worker
packages, retaining component hashes and distinct versions. The Windows
packager below preserves official runtime bytes and cannot package this
runtime replacement.

Qualify the complete packages with native exec/attach transfers over SPDY
and WebSocket, including translation at the API-server and at the kubelet.
Module tests and a bare k0s build do not prove that the installed API-server,
kubelet and containerd contain the fix. Verify all three component hashes
against the build manifest rather than relying on the outer k0s version.

For live transfer checks, verify the exact stdout length and digest as well as
the command status. An older SPDY client can report success after truncated
output. Retain baseline, candidate and restored-runtime measurements through
the same API and workload identities. Measure upgrade continuity separately
from steady-state transfers: restarting a worker can replace its node-local
load balancer and interrupt management streams even when workload containers
remain running. A passing transfer after startup does not qualify that restart.

## Static-pod identity across worker restarts

`static-pod-listener.patch` targets `v1.36.4+k0s.1`, commit
`c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60`. Kubelet includes the HTTP
manifest source URL in static-pod identities. Choosing a new listener port on
each worker start therefore replaces an unchanged node-local load-balancer pod.

The patch reuses the loopback listener address recorded in the generated
kubelet configuration. A first start, or a previous configuration without a
static-pod URL, still allocates a port normally. Invalid previous configuration
or an occupied port fails startup rather than silently changing pod identities.
The manifest contents and kubelet's identity calculation remain unchanged.

```sh
git -C <k0s-checkout> apply --check <absolute-path>/static-pod-listener.patch
git -C <k0s-checkout> apply <absolute-path>/static-pod-listener.patch
cd <k0s-checkout>
go test ./pkg/component/worker ./pkg/component/worker/nllb ./cmd/worker
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c \
  -o worker.test.exe ./pkg/component/worker
```

Execute `worker.test.exe -test.run=^TestStaticPods -test.timeout=2m` on each
supported Windows version. The tests exercise real HTTP listeners, reuse after
an HTTP request, occupied-port failure and retry, and rejected configurations.

Rebuild the complete runtime package and verify existing static-pod UIDs and
container identities throughout the guarded restart and subsequent reconciliation,
not only in the first successful API snapshot. Compare the complete manifest,
including metadata: the node-local balancer carries the k0s build version in
`app.kubernetes.io/version`, and changing that label changes kubelet's identity
hash even when the PodSpec and listener URL are identical. Listener reuse alone
does not preserve the pod across such a version change. Reuse depends on the
previous generated configuration remaining available; it does not establish
identity preservation after a reboot that clears the runtime directory or a
rollback to an unpatched executable. It also does not preserve active exec
streams through a kubelet restart. Qualify management continuity separately.

## Runtime logging during controller maintenance

The embedded containerd daemon stops with k0s. Container processes can survive
while their synchronous stdout or stderr writes block because the daemon is
no longer draining CRI logs. Preserved Pod UIDs and container IDs therefore do
not prove workload continuity. Measure replies with ordinary workload logging
enabled throughout restart and rollback; do not increase request deadlines to
hide a stalled log writer.

k0s supports an [independently managed CRI runtime](https://docs.k0sproject.io/v1.36.4%2Bk0s.1/runtime/#using-custom-cri-runtimes)
through `--cri-socket=remote:unix:///path/to/containerd.sock`, including controllers
with `--enable-worker`. In that mode k0s does not manage the runtime lifecycle
or configuration. Before using it for maintenance isolation, qualify ownership
transfer, service ordering, runtime binary and configuration preservation, shim
survival, reboot, rollback and runtime upgrades. Merely supplying the socket
flag does not provide those guarantees. Keep runtime maintenance separate from
controller maintenance: an independently managed runtime can still block logs
when it is itself unavailable.

`k0s-containerd.service` adapts containerd's upstream systemd unit for the
default Linux k0s paths. It preserves the runtime root, state, socket and
configuration and keeps shim processes alive when the daemon stops. The
`external-cri-ordering.conf` drop-in orders k0s after that service without
coupling their stop or restart operations. These files require an already
staged, verified containerd, shim, runc and complete runtime configuration;
they do not install or migrate them. Custom data directories and Windows
require their own configuration.

Install the unit as `/etc/systemd/system/k0s-containerd.service` and the
ordering drop-in under the actual `k0scontroller.service.d` or
`k0sworker.service.d` directory only as part of a guarded ownership transfer.
Retain every existing k0s invocation argument when adding
`--cri-socket=remote:unix:///run/k0s/containerd.sock`. Never run both runtime
owners against the same root or socket. Keep the original unit and runtime
configuration available for rollback, and verify the daemon's cgroup and
process identity across each subsequent k0s restart.

External ownership also transfers responsibility for runtime binary staging,
drop-in configuration reloads, sandbox-image configuration and image imports;
k0s's embedded-runtime component no longer performs those operations. Pin
the whole runtime payload and preserve required registry and CNI settings.
Do not assume that replacing the outer k0s executable upgrades an externally
managed runtime. These reference units remain subject to the migration and
failure tests above before deployment.

A diagnostic that pauses the daemon must have guest-local automatic resumption
bound to the original process identity. Capture workload thread stacks and
syscalls, application and CRI log timestamps, and original continuous observers.
Retain all request failures and verify daemon, workload and cluster recovery.
Such a diagnostic establishes a failure mechanism, not deployment readiness.

Before a subsequent maintenance action, verify authenticated exec and log
access to an unchanged workload on the restarted node through each required
API endpoint. API `/readyz`, CRI readiness and a Node's existing `Ready`
condition do not establish that the restarted kubelet is serving requests.
Repeated restarts before that recovery can combine individually short outages
into one continuous management outage. Keep the original observer running and
measure the complete failure-to-recovery interval; do not reset it at restart
boundaries. Qualify repeated failures separately from maintenance operations
that wait for full recovery, retaining failures in both scenarios.

## Static-pod metadata across build changes

`static-pod-metadata.patch` targets the same k0s base as the listener patch.
Apply both when packaging a runtime that must keep an unchanged node-local
balancer across supervisor build changes. The metadata patch omits the k0s
supervisor version label from Envoy and Traefik static Pods. It retains their
component and ownership labels and leaves other resources' version labels
unchanged. The proxy's actual image remains in the PodSpec, so changing that
image still changes the Pod identity.

Removing the old version label is itself a manifest change. The first upgrade
from an unpatched runtime, or rollback to one, can therefore replace the Pod.
This patch does not qualify that transition or preserve exec streams during a
kubelet restart. Fresh workers and subsequent patched-to-patched upgrades need
native identity and continuity measurements before deployment qualification.

Run `go test ./pkg/component/worker/nllb` after applying the patch. The
`TestStaticPodBuildMetadata` test reports complete manifest digests for both
backends; compare them across test builds with distinct
`-ldflags '-X github.com/k0sproject/k0s/pkg/build.Version=VERSION'` values.
Matching manifests establish metadata stability, not native runtime continuity.

Inspect the manifest served at the current kubelet `staticPodURL` separately
from its API mirror Pod. During replacement, the API can temporarily retain
the previous mirror's labels and hash while reporting the new container's
status. Wait for the mirror hash to match the native static Pod before using
it as a restart baseline. Retain the native sandbox UID, container ID and
served manifest across the restart; a Ready API mirror alone does not prove
identity continuity. Record management failures during reconciliation as well
as after it.

## Worker releases with patched controllers

Give patched controllers a version beginning with their upstream release, such
as `v1.36.4+k0s.1.egress.2`. To provision workers against that controller build,
set `worker-release` to `v1.36.4+k0s.1` in the existing `k0s-provider-config`
Secret in the controller's namespace. Preserve any other configured keys.

The join provider requires every observed Autopilot `ControlNode` to report
the same complete controller version and the configured upstream base. Missing
observations, mixed builds, malformed versions and a different base fail
selection. Without this explicit setting, custom controller versions remain
rejected. An unreadable configuration also stops selection.

This selects the upstream release used by the normal Linux bootstrap path.
To use a complete patched Linux worker baked into an image, add `worker-version`
and `worker-sha256-amd64` and/or `worker-sha256-arm64` to that same Secret.
The version must retain the exact selected upstream base, and each configured
checksum must be a 64-digit SHA-256 of the complete executable. For example:

```yaml
stringData:
  worker-release: v1.36.4+k0s.1
  worker-version: v1.36.4+k0s.1.custom.1
  worker-sha256-amd64: REPLACE_WITH_VERIFIED_COMPLETE_EXECUTABLE_SHA256
  worker-sha256-arm64: REPLACE_WITH_VERIFIED_COMPLETE_EXECUTABLE_SHA256
```

Bake the corresponding executable at `/usr/local/bin/k0s`. Bootstrap validates
its checksum before executing it, checks its exact version before installing
the tunnel or worker service, and uses that absolute path for installation and
startup. Missing executables, unconfigured architectures, incorrect hashes or
versions fail bootstrap; an explicit override never falls back to downloading
an upstream worker. Omit both version and checksum keys to retain the existing
upstream installation path. These settings affect new bootstrap data and do
not upgrade existing workers.

A patched Windows worker still requires `windowsBootstrap.workerVersion` and
`windowsBootstrap.workerSHA256`, with that same upstream base. Qualify the
controller and worker artifacts together, including fresh CAPI boots on each
admitted architecture, before enabling provisioning.

## Empty optional manifests with patches enabled

`empty-manifest-patches.patch` targets k0s `v1.36.4+k0s.1`, commit
`c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60`. Its manifest patcher treats
comment-only and empty YAML documents as resources missing a `kind`. Optional
Calico templates produce these documents when a resource is disabled, so
enabling Calico patches can cause rendering errors for unrelated templates.

The patch preserves documents that decode to YAML null without applying
resource patches to them. Nonempty malformed resources still fail. Apply it to
the matching source checkout and run `go test ./internal/pkg/patches
./internal/pkg/templatewriter ./pkg/component/controller`. Tests include the
distribution's actual optional Calico templates. Rebuild and qualify the
controller before deployment; source tests do not qualify a running binary.

## Windows Traefik configuration replacement

`windows-traefik.patch` targets these upstream worker revisions:

| Release | Commit |
| --- | --- |
| `v1.36.2+k0s.0` | `bdf1c22c23a5af23ec6a1ff764179f6a6cf6f7dc` |
| `v1.36.4+k0s.1` | `c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60` |

The native Traefik node-local balancer writes its public configuration with
POSIX mode `0444`. On Windows this sets the read-only file attribute, preventing
an atomic rename over the existing file. API backend updates then fail with
`Access is denied`, even when the worker and initial proxy are running.

The patch clears the attribute on existing Windows files and writes subsequent
files without it. Windows ACL inheritance still controls access. Linux retains
mode `0444`. The regression test replaces an existing read-only file twice,
covering both upgrade from the original writer and subsequent updates.

Apply to a separate source checkout:

```sh
git -C <k0s-checkout> apply --check <absolute-path>/windows-traefik.patch
git -C <k0s-checkout> apply <absolute-path>/windows-traefik.patch
```

Run the focused tests on Linux and cross-compile them for execution on each
Windows VM:

```sh
go test ./pkg/component/worker/nllb \
  -run 'TestWriteTraefikConfigFiles|TestTraefikReplacesExistingReadOnlyConfig'
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c \
  -o nllb.test.exe ./pkg/component/worker/nllb
```

Run `nllb.test.exe` through the VM's native management channel with the same
`-test.run` expression. Testing this patch does not replace a rebuilt-worker
rollout, API backend-change/failover tests, or fresh prepared-image CAPI boots.
Prepared-image boots remain a separate qualification from an existing-worker
binary rollout. Cross-compilation does not exercise Windows file replacement;
execute the tests natively for each source version and Windows version being
qualified.

## Build a complete Windows worker

Use the patched source checkout and its Makefile to retain the distribution's
component version metadata and build flags. Give the modified worker a distinct
version. The following builds its executable without rebuilding bundled runtimes:

```sh
make DOCKER= GO=go GO_ENV_REQUISITES= EMBEDDED_BINS_BUILDMODE=none \
  VERSION=v1.36.4+k0s.1.winfix.1 k0s.exe
```

That output alone is a bare worker. `package-windows.py` attaches the embedded
runtime ZIP from a checksum-verified official executable. Use the asset matching
the source release:

| Upstream asset | SHA-256 |
| --- | --- |
| `k0s-v1.36.2+k0s.0-amd64.exe` | `81d05aec71b1d34a8d8ae73641293c8b2f45abf9aaa2d55e332ed41103654c65` |
| `k0s-v1.36.4+k0s.1-amd64.exe` | `13806ad31bfce926ce8bbfbf073231a87fb5e97b9b9ed5dc5c4525101c3dc919` |

Obtain it from the matching official k0s release and verify the bare build's hash
independently before packaging. For the 1.36.4 build above:

```sh
python3 providers/k0s/package-windows.py \
  --bare /path/to/patched-checkout/k0s.exe --bare-sha256 VERIFIED_BUILD_SHA256 \
  --upstream /path/to/official-k0s.exe \
  --upstream-sha256 13806ad31bfce926ce8bbfbf073231a87fb5e97b9b9ed5dc5c4525101c3dc919 \
  --output /path/to/new-package-directory
python3 -m unittest discover -s providers/k0s -p 'test_*.py'
```

The packager requires amd64 PE inputs, rejects an already packaged worker and
unexpected payload entries, and verifies that containerd, its Windows shim and
kubelet retain their original bytes. `package.json` records input, payload,
output and component hashes. It does not sign the modified worker or imply an
upstream release. Preserve upstream licensing when distributing these artifacts.
Version/source matching is the caller's responsibility; matching hashes alone
does not establish that a different release's runtimes are compatible.

Bake the packaged executable into the machine image to avoid installing these
runtime components from the network during startup. Verify its hash and version
natively on both Windows versions, then test service restart, API backend updates
and fresh CAPI boots. Keep the previous executable available for rollback during
existing-worker testing.

## Native Linux default routes for API egress

`native-linux-egress.patch` targets these upstream controller revisions:

| Release | Commit |
| --- | --- |
| `v1.36.2+k0s.0` | `bdf1c22c23a5af23ec6a1ff764179f6a6cf6f7dc` |
| `v1.36.4+k0s.1` | `c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60` |

The native Konnectivity manifest retains the existing node-address agents and
adds `konnectivity-agent-default-route-linux`, using the same distribution
image, ServiceAccount and projected credentials. Its Linux-only placement and
separate health/admin ports, 18095/18096, allow it to coexist with node agents.
The controller selects `destHost,defaultRoute`, without the random `default`
fallback that can send Service traffic through an unsuitable Windows host.
An unavailable default-route pool therefore fails unmatched connections rather
than selecting a node-address agent. Windows kubelet destinations retain their
explicit host routing.

```sh
git -C <k0s-checkout> apply --check <absolute-path>/native-linux-egress.patch
git -C <k0s-checkout> apply <absolute-path>/native-linux-egress.patch
cd <k0s-checkout>
go test ./pkg/component/controller
```

Build and qualify a distinct controller release containing the patch. Source
and manifest tests do not establish native Service reachability, uninterrupted
upgrade, or mixed-OS support. Exercise webhook requests through every API
server, Windows kubelet access, default-agent loss/recovery and controller
restart before promotion. The distribution must own and reconcile the new
agent manifest; installing an equivalent DaemonSet from the test harness does
not qualify this product change. See [API egress qualification](../../docs/k0s-windows-egress.md).

## Konnectivity lease discovery

`konnectivity-discovery-regression.patch` is a test-only patch for upstream
Konnectivity revision `e8432610cab5a696fcbdb37969ac1aacb8bcf7f8`.
It reproduces agents retaining a two-controller response header when a third
controller returns. Repeated connection attempts remain suppressed with the
default count-based discovery; continuous discovery connects to the returned
server. Apply it to the Konnectivity checkout and run
`go test ./pkg/agent -run TestStaleTwoControllerHeaderAfterThirdControllerReturns`.
This tests agent discovery behavior, not live cluster recovery or continuity.

`konnectivity-discovery-preparation.patch` applies to `v1.36.4+k0s.1` after
`native-linux-egress.patch`. It enables `--sync-forever=true` on both agent
variants without changing server headers or server lease mode. It removes the
agent's count-change watcher and the count-only environment variable so
membership fluctuations no longer replace otherwise healthy agent pods. Build a distinct preparation release and let the elected k0s
controller reconcile its agent manifests. Install that release on every
controller so a leadership change cannot restore the old agent template.
Verify every agent's effective arguments and readiness, then exercise leader
changes and rollback before proceeding.

`konnectivity-leases.patch` applies after the preparation patch. It adapts the lease-discovery change proposed by
[fooxlj07 in upstream PR #7438](https://github.com/k0sproject/k0s/pull/7438)
(head `3129898a57cedc4867d60ec80119a1868d8b686b`, closed without merging).
The relevant controller-count failure is described in
[upstream issue #8416](https://github.com/k0sproject/k0s/issues/8416).

The server publishes its lease using Konnectivity's built-in lease controller.
Both node-address and Linux default-route agents count those leases and keep
discovering servers with `--sync-forever=true`. Continued discovery prevents an
incomplete lease count during rolling upgrades from suppressing connections
to older controllers. k0s no
longer restarts the server or changes the agent pod template when its active
controller count changes. The patch retains existing authentication resources
and grants the agent ServiceAccount read-only lease access in `kube-system`.
The embedded Konnectivity v0.36.0 supports these flags.

```sh
# Preparation release (with the existing native egress patch applied):
git -C <k0s-checkout> apply --check <absolute-path>/konnectivity-discovery-preparation.patch
git -C <k0s-checkout> apply <absolute-path>/konnectivity-discovery-preparation.patch

# Lease-mode release, only after the preparation rollout has been qualified:
git -C <k0s-checkout> apply --check <absolute-path>/konnectivity-leases.patch
git -C <k0s-checkout> apply <absolute-path>/konnectivity-leases.patch
cd <k0s-checkout>
go test ./pkg/component/controller ./cmd/controller
```

Lease discovery changes membership semantics: older controllers do not publish
these leases, and older agents stop discovery when the last server-count header
is satisfied. A lease-enabled server uses the upstream default header count of
one. Existing agents must therefore have continuous discovery enabled before
any server switches to lease mode. Enabling it only in the new release's
manifest does not establish that ordering: only the elected k0s controller
applies manifests, so a non-leading controller can switch its server first.
Qualify an agent-first transition and resource rollback before enabling this
patch on an existing cluster. Restoring the executable alone does not prove
restoration of the agent manifests, lease permissions, or membership. Native validation must verify server leases, agent
RBAC, stable agent pod identities across membership changes, Windows kubelet
access, Service egress, and continuous management availability. This patch does
not make a restarted API-server endpoint continuously available or qualify a
single-controller runtime upgrade.

## Preparing a complete maintenance runtime

Build discovery preparation from the complete patch set so it retains the
worker restart and streaming fixes. Apply these patches in order to
`v1.36.4+k0s.1` (`c37fe960bdee93ca14c5a5b9f2ab9700f8e82e60`):

1. `empty-manifest-patches.patch`
2. `windows-traefik.patch`
3. `native-linux-egress.patch`
4. `exec-streaming.patch`
5. `static-pod-listener.patch`
6. `static-pod-metadata.patch`
7. `konnectivity-discovery-preparation.patch`
8. `calico-cni-api-endpoint.patch`

Keep lease-mode activation separate until preparation has been qualified.
Run the controller, worker, worker/nllb and both command packages' tests against
this combined source tree. Cross-compile each supervisor on the build host,
then use the upstream ZIP and embedding targets to append its matching runtime
payload. Verify the complete payload inventory and each component hash on every
platform; a rebuilt supervisor alone does not update embedded executables.

The complete streaming payload includes both the API-server and kubelet from
Kubernetes revision `f76f8bc3ff781afa28e0dded98227515f9c25e2c` and containerd
revision `7b2fc110dd8a63fdc87a6809c256066c1082d7c1`. The Kubernetes revision adds
translator integration tests to the production changes pinned by
`exec-streaming.patch`. Preserve the distribution's other runtime components.
Record source and payload digests, architecture and the outer runtime version
with each package. A build or native version readback is not deployment
qualification: runtime replacement, first-upgrade static-pod changes, rollback,
CNI endpoint reachability and reboot recovery still need native observations.

## Calico CNI API access during boot

`calico-cni-api-endpoint.patch` targets `v1.36.4+k0s.1`. It configures
Linux `install-cni` to write its kubeconfig against the existing control-plane
endpoint, or the configured Envoy/Traefik localhost API port when node-local
load balancing is enabled. CNI cleanup and creation can then contact the API
before kube-proxy has restored Kubernetes Service routing. The patch uses the
existing CA and credentials; it does not disable TLS verification or pin an API
group to bypass discovery.

Apply this patch to the runtime source and run
`go test ./pkg/component/controller -run TestCalico`. The tests render and decode
the real DaemonSet for direct IPv4, external DNS, IPv6 and both local proxy
variants, including custom ports and invalid proxy configuration. These checks
establish endpoint selection, not native CNI startup, TLS reachability or the
60-second boot-recovery bound.

Roll out through the runtime's manifest owner. Verify the generated CNI
kubeconfig's server on each worker without exposing its credentials, and observe
workload and management continuity during the resulting DaemonSet rollout.
Changing local load-balancer configuration still requires the existing worker
configuration rollout; the CNI manifest cannot start that proxy itself.
Worker-specific profile overrides and rollback require their own checks.
Before rollout, check authenticated API discovery through the selected endpoint
on every participating node using its existing CNI CA and credentials, without
logging credential values. Keep TLS verification enabled. This read-only check
can establish steady-state reachability only; the boot test must still show that
the local proxy becomes usable before CNI cleanup and creation need it.
Repeat full reboot tests with a stable workload Service, preserving all failed
requests and measuring both Node and workload recovery. Removing the Service
routing dependency does not remove other host boot or CNI delays.

## DaemonSet rollout with retired nodes

The controller-manager dependency must include Kubernetes change
`b791867865b7483feea2b65468b731e9a3a73f49`, based on `v1.36.4`, from branch
`fix/daemon-retired-node-rollout` in the existing Kubernetes fork. The change
excludes terminating-only pods on absent or no-longer-selected nodes from the
non-surge rollout's unavailable count. Terminating pods on still-selected nodes
continue to consume the configured availability budget. Surge cleanup behavior
is unchanged.

Run the full `./pkg/controller/daemon` test package, including
`TestDaemonSetRolloutWithTerminatingRetiredPod`. Its cases cover selector
exclusion, untolerated taints, Node deletion and a still-selected node. Use
owned, terminating Running pods so the fixture exercises the controller's
actual pod-discovery path.

Cross-compile `cmd/kube-controller-manager` for each target architecture using
the Kubernetes build targets. Include it in the existing k0s runtime ZIP and
embedding workflow, preserving the other components selected for that release.
Record the component source revision and payload hashes. A component-only
package that preserves the supervisor also preserves its version string;
verify the full package and running controller-manager hashes rather than
using that string to identify the dependency.

Qualify the installed runtime with an actual blocked DaemonSet rollout while
observing workload and management continuity. Preserve retired-node pods and
the configured availability budget during the test. Unit tests and artifact
inspection do not establish native rollout progress, architecture-specific
execution or deployment readiness. If agent rollout resumes during runtime
maintenance, qualify agent-only upgrade continuity separately.

Keep direct per-controller API readiness failures separate from exec/log
maintenance interruptions. Bounded exec/log recovery does not satisfy a
zero-interruption API requirement. Also observe the actual client-facing HA
endpoint through controller replacement; healthy surviving controllers alone
do not prove client failover or request continuity. Retain both sets of samples
and describe their endpoint scope in the scenario.
