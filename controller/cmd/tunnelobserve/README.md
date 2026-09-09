# Read-only tunnel observations

`tunnelobserve` samples an existing `cldt` WireGuard interface. Linux uses
netlink through `wgctrl`; Windows uses the WireGuard driver API. It does not
create an adapter, configure peers, or require a `wg` executable.

Build the binary for the guest OS from the controller module:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /private/tunnelobserve ./cmd/tunnelobserve
GOOS=windows GOARCH=amd64 go build -o /private/tunnelobserve.exe ./cmd/tunnelobserve
```

Copy the matching binary through the machine's management adapter and run it
with administrator/root privileges. Substitute the existing interface name and
a new report path:

```sh
/private/tunnelobserve -interface cldt0123abcd \
  -report /private/new-observation.jsonl -samples 10 -interval 1s
```

The report must not already exist. Sampling is bounded to 3,600 observations
and one hour, with intervals between 100 ms and one minute. Each JSONL record
contains the observation time, interface state, hashed public peer identities,
peer endpoints, byte counters and last handshake timestamps. Both platforms use
`lastHandshakeFiletime`: 100 ns intervals since 1601-01-01 UTC; zero means no
valid handshake. Linux timestamps are converted to that existing Windows schema.
Native configuration objects, private keys and preshared keys are never serialized.

An absent interface or failed query returns a nonzero exit code. Do not treat an
empty or failed report as evidence that an interface has no peers. Keep the
command status with the observation. A snapshot taken after a failed network
check describes its capture time, not necessarily the instant of packet loss.

For temporary VM diagnostics, stage the binary outside system installation
paths, record its SHA-256, and remove the owned binary and reports when finished.
Do not change a worker image or install packages merely to make this observer run.

The [Linux observation result](../../../docs/validation/tunnelobserve-linux-results.json)
records native sampling on a single-NIC k0s remote and the selected-field
regression tests. The separate [Windows observation result](../../../docs/validation/tunnelobserve-windows-results.json)
records three native samples on each of Windows Server 2022 and 2025, with
temporary files removed afterward. Neither report qualifies network reliability.
