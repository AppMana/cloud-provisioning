# k0s Windows Traefik configuration replacement

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

## Native worker validation

[Full-worker evidence](../../docs/validation/windows-k0s-traefik-worker-results.json)
records the packaged binary running as the worker service on Windows Server 2022
and 2025. Existing read-only routing files become writable after startup. Stopping
one of the isolated cluster's three controllers reduces both Windows proxies'
API backend lists from three to two; restarting it restores all three through
normal worker-profile updates. Worker service processes remain unchanged during
those updates.

Authenticated readiness probes use the node kubeconfig through the IPv6 loopback
proxy, with TLS server-name verification. Both versions pass all probes after
backend removal converges and after restoration. During removal, Server 2022
passes 40/40 requests and Server 2025 passes 39/40. This demonstrates routing-file
replacement and recovery; the failed request leaves continuous availability
unqualified. Prepared-image CAPI boots have separate
[fresh-image checks](../../docs/validation/windows-fresh-image-join-results.json).
See the [Windows validation boundaries](../../docs/windows.md#validation-boundaries)
for the scoped lifecycle results and remaining release gates.
