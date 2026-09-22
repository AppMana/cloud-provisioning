# Labcontainers test runtime

The container and single-NIC VM harnesses use
`github.com/appmana/labcontainers` for topology ownership, isolation checks,
leases, cleanup, and Containerlab lifecycle. The harness still owns product
configuration and assertions.

`cmd/lab` builds `labd` from the pinned Go module on first use when no `labd`
is installed. Set `LABCONTAINERS_LABD` to test a specific daemon binary.
Persistent session metadata lives under the selected work directory in
`.labcontainers/`; a later `-down` process reconnects to and destroys that
session. Labs created before this migration retain a direct-destroy fallback.

The Calico and Kubernetes forks are systems under test, not topology owners:
their images and binaries are loaded into this harness. Consequently those
large upstream repositories do not import a lab SDK. Their shared network/VM
validation now uses Labcontainers through this repository, which is the single
orchestration boundary.
