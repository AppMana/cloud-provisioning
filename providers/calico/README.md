# Windows endpoint MTU support

`windows-mtu.patch` targets official Calico `v3.32.0`, commit
`eb1cf57823a1dd8d25c48b82fd023ea9e3e17996`. It adds an opt-in Felix endpoint MTU
check for Windows VXLAN workers. Use a source base matching the deployed Calico
version; the older local 3.31-based dual-stack fork watches admin-policy APIs
unavailable in the tested 3.32 cluster. This patch does not include that fork's
unrelated changes or add Windows dual-stack CNI support.

```sh
git apply --check /path/to/windows-mtu.patch
git apply /path/to/windows-mtu.patch
go test ./libcalico-go/lib/winmtu ./felix/dataplane/windows
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
  -o calico-node.exe ./node/cmd/calico-node
```

Set `FELIX_VXLANMTU` to an explicit positive packet budget on Windows VXLAN nodes.
Zero preserves existing behavior. The helper resolves the endpoint's HCN namespace
and applies an upper bound only to its isolated compartment and exact IP addresses.
It preserves smaller platform values, checks read-back, and installs no software
at startup. IPv6 requires at least 1280 bytes; an explicitly smaller positive
`VXLANMTUV6` also bounds MTU when IPv6 support is enabled.

Felix verifies MTU before publishing endpoint policy. An unavailable compartment
keeps that endpoint pending while other endpoints progress. Existing rules remain
unchanged on failure; a new endpoint retains the CNI's initial ingress deny. A cached ACL does not bypass MTU verification on a subsequent update. Deletion
needs no surviving interface. Endpoint replay on Felix restart reapplies the MTU
bound. While MTU enforcement is enabled, a five-second timer schedules one
active endpoint at a time for rechecking through the same reconciliation path. Each snapshot gives every active endpoint a turn. Pending updates and deletions
take precedence; stale entries are skipped. This bounds additional periodic
PowerShell work instead of checking every workload synchronously. A complete
cycle takes roughly five seconds per active endpoint plus reconciliation time;
this is eventual repair, not instantaneous enforcement after external changes. Restarting a workload's virtual adapter resets its MTU to 1500 on the tested
Windows 2022 and 2025 builds. Images built before the periodic recheck require an endpoint update or Felix
restart to repair this drift. Recheck MTU after adapter
operations; a successful Felix restart test does not establish persistence
across adapter restarts.

## k0s configuration

Package the executable using the [Windows Calico image builder](../../images/calico/README.md).
It preserves the pinned upstream layers and startup configuration, replaces the
binary at build time, and produces a reproducible image archive for cloud OCI
registries. Configure `spec.images.calico.windows.node` and the pull Secret
through k0s. This avoids requiring a manually staged file on each worker.

## Staged-binary validation

This optional validation path stages the rebuilt executable on each host and copies it into
the Felix HostProcess container during startup. Append the object in
[`k0s-mtu-validation-patch.json`](k0s-mtu-validation-patch.json) to
`spec.network.calico.patches` on every controller. Preserve existing patches and
set `spec.network.calico.mtu` to the measured path budget. Validate and restart
static-config controllers sequentially using the
[k0s configuration procedure](../../docs/windows-gateway.md#configure-the-k0s-mtu).
The strategic merge targets only the named Windows Felix container. Its startup
command requires the staged binary's exact SHA-256 before copying it. A different
build requires verifying and explicitly updating that checksum.

k0s regenerates its bundled Calico DaemonSets: a direct `kubectl` command override
is lost on controller restart. Existing workload interfaces can retain their MTU
after that reversion, so passing traffic checks alone do not establish that the
patched binary is still running. Verify its hash inside each Felix container
after the generated DaemonSet rollout, then test fresh workloads and restart
recovery. The supported patch schema is defined in
[k0s 1.36.2](https://github.com/k0sproject/k0s/blob/v1.36.2%2Bk0s.0/pkg/apis/k0s/v1beta1/patches.go).

This hash-bound override requires the staged file; fresh CAPI workers must not
be assumed to contain it. Remove this command override when switching to the
derived container image. Prepared machine image preloading and fresh CAPI boots
still require their own validation.

The 1370-byte setting is the measured IPv4 VXLAN budget over the test's 1420-byte
WireGuard interface, not a universal cloud default. Configure Linux MTU through
the distro as well.
