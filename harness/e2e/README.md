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

Distribution bootstrap builders and cloud provisioning operations are still
part of this project; this change does not claim their full extraction or a
qualified Calico/k0s pod network.
