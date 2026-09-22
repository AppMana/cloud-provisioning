# Harness SDK integration

The harness constructs upstream Containerlab Go topology objects and passes them
to Labcontainers. The Kubernetes API-access client is shared from
`labcontainers/pkg/kubernetes/kube`; product-specific CAPI identity assertions
remain in this project. Probe Pods, Services, and their Namespace use native
Kubernetes objects, not manifest strings. Probe images must be preloaded.

This migration currently requires a separate Go workspace containing this
module and the Labcontainers `feature/native-typed-sdk` worktree. The released
Labcontainers requirement in `go.mod` predates these APIs; an independent
release/pseudo-version pin is still pending. Do not mistake workspace test
success for an independently installable consumer module.

With that workspace selected, run:

```sh
GOWORK=/absolute/integration/go.work go test -p 2 ./...
```

Build the matching SDK daemon with `make build` in its worktree and set
`LABCONTAINERS_LABD` to its absolute `bin/labd` path for live tests. VM recovery
also requires the isolated corrected Containerlab binary described in the SDK
README, selected using `LABCONTAINERS_CONTAINERLAB`. Neither setup replaces the
host installation.

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

Fresh RKE2 builds require `--rke2-artifacts-dir` containing `install.sh`,
`rke2.linux-amd64.tar.gz`, and `sha256sum-amd64.txt`, with
`--rke2-installer-sha256`, `--rke2-archive-sha256`, and
`--rke2-checksums-sha256`. All three files are verified before any node is
modified. The builder uses the staged native installer through
`labcontainers/pkg/kubernetes/rke2`, with explicit native tar-installation
environment arguments. It neither downloads an installer nor silently reuses
an arbitrary existing RKE2 executable. This extraction has unit-test coverage;
it is not a new live RKE2 qualification.

The native Go configuration dependency's pseudo-version resolves the
`v1.36.2+k0s.0` release commit `bdf1c22c23a5`; it does not select the binary
under test. This requires Go 1.26.3 or newer.

Full distribution-builder extraction and Calico/k0s pod-network qualification
are still pending. These changes do not claim either is complete.
