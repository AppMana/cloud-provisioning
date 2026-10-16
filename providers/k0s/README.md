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
the API-server from the same Kubernetes revision using the upstream
Makefile and the distribution's build flags. Containerd also needs the
patched CRI-streaming server: the 2.3.5 integration is pinned at
`7b2fc110dd8a63fdc87a6809c256066c1082d7c1`. Use k0s's existing ZIP builder
and embedding targets to assemble the complete controller and worker
packages, retaining component hashes and distinct versions. The Windows
packager below preserves official runtime bytes and cannot package this
runtime replacement.

Qualify the complete packages with native exec/attach transfers over SPDY
and WebSocket. Module tests and a bare k0s build do not prove that the
installed API-server and containerd contain the fix.

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

This selects the upstream Linux worker release used by the normal bootstrap
path. A patched Windows worker still requires `windowsBootstrap.workerVersion`
and `windowsBootstrap.workerSHA256`, with that same upstream base. Qualify those
controller and worker artifacts together before enabling provisioning.

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

`konnectivity-discovery-preparation.patch` applies to `v1.36.4+k0s.1` after
`native-linux-egress.patch`. It enables `--sync-forever=true` on both agent
variants without changing server headers, lease mode, or existing membership
handling. Build a distinct preparation release and let the elected k0s
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
