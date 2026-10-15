# k0s provider patches

## Windows Traefik configuration replacement

`windows-traefik.patch` targets k0s `v1.36.2+k0s.0`, commit
`bdf1c22c23a5af23ec6a1ff764179f6a6cf6f7dc`.

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
binary rollout.

## Build a complete Windows worker

Use the patched source checkout and its Makefile to retain the distribution's
component version metadata and build flags. Give the modified worker a distinct
version. The following builds its executable without rebuilding bundled runtimes:

```sh
make DOCKER= GO=go GO_ENV_REQUISITES= EMBEDDED_BINS_BUILDMODE=none \
  VERSION=v1.36.2+k0s.0.appmana.1 k0s.exe
```

That output alone is a bare worker. `package-windows.py` attaches the embedded
runtime ZIP from a checksum-verified official executable. For this source version
the upstream asset is `k0s-v1.36.2+k0s.0-amd64.exe`, SHA-256
`81d05aec71b1d34a8d8ae73641293c8b2f45abf9aaa2d55e332ed41103654c65`.
Obtain it from the matching official k0s release and verify the bare build's hash
independently before packaging:

```sh
python3 providers/k0s/package-windows.py \
  --bare /path/to/patched-checkout/k0s.exe --bare-sha256 VERIFIED_BUILD_SHA256 \
  --upstream /path/to/official-k0s.exe \
  --upstream-sha256 81d05aec71b1d34a8d8ae73641293c8b2f45abf9aaa2d55e332ed41103654c65 \
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
