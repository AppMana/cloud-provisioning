# Node-local configuration rollback

`network-guard` bounds a migration of explicitly owned Linux configuration
files. Run it as an independent root systemd service. It captures original and
candidate bytes and pins both apply and restore executables; it does not
need Kubernetes, SSH, Tailscale, or a remote heartbeat. It is a building block,
not a complete CNI migration transaction or deployment authorization.

The deadline begins at preparation, lasts at most four minutes, and never
extends on service restart. Restoration has a separate 30-second command
limit, leaving margin within the five-minute recovery budget. Time uses the
boot clock, not wall-clock timestamps. A different boot ID triggers immediate
restoration when the enabled service starts.

## Prepare and arm

Build `controller/cmd/network-guard` for the node's architecture and install it
at `/usr/local/sbin/network-guard`. Install the
[service template](../deploy/network-guard@.service) in `/etc/systemd/system`.
Create root-owned apply and restore executables for only the selected configuration.
It must be idempotent and must not flush shared routing tables, firewall rules,
or unrelated interfaces. Freeze other writers to the selected files until the
transaction is committed or rolled back.

The plan lists existing files and the SHA-256 of their exact candidate bytes:

```json
{
  "apply": "/usr/local/libexec/apply-owned-network-config",
  "restore": "/usr/local/libexec/restore-owned-network-config",
  "files": [
    {"path": "/etc/example/owned-network.conf", "candidatePath": "/root/owned-network.candidate", "candidateSHA256": "REPLACE_WITH_64_HEX_DIGITS"}
  ]
}
```

The executable and configuration must be regular files without symlink paths,
group/world write permission, or extended attributes. ACL/label preservation
and creation/deletion of configuration files are unsupported. State contains
private snapshots and an approval token; keep it root-owned with mode 0700.

```sh
install -d -m 0700 /var/lib/network-guard
network-guard -state /var/lib/network-guard/change-001 \
  -plan /root/change-plan.json -within 4m prepare > /root/change-token.json
systemctl daemon-reload
systemctl enable --now network-guard@change-001.service
network-guard -state /var/lib/network-guard/change-001 status
```

Use a fresh transaction directory every time. The service installs the captured
candidate bytes and runs the pinned apply executable within the original
deadline. It kills the command process group before restoration on failure or
timeout. Keep the service's `KillMode=control-group`: systemd must kill remaining
children before restarting the guard. Commands must finish synchronously and
must not delegate candidate writes to other services, schedulers, or detached
process groups. Service-managed configuration needs the additional reconciliation
proof described below; process cleanup alone does not cover it.

Before measuring the candidate, require an active service, `ready.json`,
`outcome: armed`, and enough remaining observation time. Readiness is emitted
only after application succeeds. The ready file remains after completion, so
it alone is insufficient. Keep configuration ownership exclusive; digest checks
cannot lock out software that ignores the transaction. A restart during
application restores immediately and records an application failure, without
replaying the candidate. Application failures remain visible after restoration
and prevent approval of that transaction.

## Observe, approve, or restore

Continuously observe management and existing workloads from outside the node.
Approve only after the deployment-specific qualification succeeds:

```sh
network-guard -state /var/lib/network-guard/change-001 -token "$TOKEN" approve
```

Approval requires the current token, unchanged candidate bytes and metadata,
and time remaining. The command waits for `outcome: committed`; writing an
approval request alone does not disarm the watchdog. Disable the completed
unit after observing committed or rolled-back state; retain its state and logs
outside the source tree.

Without approval, the guard restores the captured bytes and metadata, then runs
the pinned executable. Changed files or command hashes stop restoration rather
than overwrite another owner's work. Failed restoration exits nonzero so the
host service retries without losing its snapshot or extending the deadline.
Attempt and failure counts remain in state even if a later attempt restores
connectivity; detailed command errors remain in the service journal. Qualification requires zero apply and restore failures; eventual restoration
cannot turn a failed migration into a passing one.

The restore executable must finish synchronously and may use local netlink;
network socket families are denied by the service. Validate command process
cleanup, boot ordering, distribution-managed manifests, CNI reconvergence,
and preserved external routing for each actual site before using this on a
live management path. Kubernetes resource rollback needs additional design;
file restoration alone does not revert API objects.

## Transit-only agent upgrades

`controller/cmd/transit-guard` provides a separate Linux routing fallback for
upgrading an older agent that removes its transit lookup rules on exit. It
copies the current nondefault relay routes into an explicitly reserved table
and installs fallback rules immediately after the original lookup priority.
A second rule pointing at the original table is insufficient when the old
agent removes every rule referencing that table.

The owned tunnel interface must be absent. Both source lookup rules must be
ordinary, unconditional lookups, and all source routes must use a relay.
Occupied fallback tables or priorities, rules targeting the fallback priority,
unsupported policy, and changes from the captured plan block installation.
Freeze placement and allocation changes throughout this operation. Reserve a
fresh private plan file and a shadow table unused by any other component:

```sh
transit-guard -plan /root/transit-upgrade.json -interface "$TUNNEL_INTERFACE" \
  -source-table "$TRANSIT_TABLE" -shadow-table "$UNUSED_TABLE" prepare
transit-guard -plan /root/transit-upgrade.json install
transit-guard -plan /root/transit-upgrade.json verify
```

Journal the plan and successful verification before stopping the predecessor.
The fallback survives process exit; it has no automatic expiry. Retain the plan
if any operation fails. After the replacement restores the original routing
unchanged and continuous workload/management observations pass, remove it:

```sh
transit-guard -plan /root/transit-upgrade.json remove
```

Removal refuses to withdraw the fallback while primary routing is missing or
changed. It preserves unexpected shadow state and can resume an interrupted
cleanup of unchanged state. It does not restore a failed agent, roll back
Kubernetes objects, or prove live-upgrade continuity. This helper still needs
deployment integration and upgrade qualification for each supported site,
IP family, and endpoint placement; it is not a replacement
for the autonomous rollback and deployment qualification requirements.

## Disposable-VM qualification

Use [the native test](../harness/e2e/safety/network-guard-native.sh) inside a
throwaway Linux VM with systemd. It owns a dedicated dummy interface, changes
its MTU, and requires autonomous restoration. Cases include guard restart,
apply timeout, and termination during application, with delayed child writes
and an independent management ping stream. It never
changes the VM's management interface or default route. That scope proves the
local watchdog mechanism; it does not qualify a real CNI or cluster migration.

## k0s reconciliation rehearsal

Run [the k0s MTU fixture](../harness/e2e/safety/k0s-mtu-native.py) against the
isolated three-node native lab. It requires a separate Docker daemon and
network namespace, a built Linux guard binary, and a fresh artifact directory:

```sh
python3 harness/e2e/safety/k0s-mtu-native.py \
  --runner "$ISOLATED_RUNNER" --guard "$GUARD_BINARY" \
  --artifacts "$PRIVATE_ARTIFACT_DIRECTORY" --repetitions 3
```

The fixture snapshots the controller's k0s configuration, changes only the Calico
MTU, and asks k0s to regenerate its managed manifests. The normal rendered
pod-template change triggers the rollout; no additional revision annotation
forces it.
It verifies CNI installation on all three nodes and creates new pods to check
actual interface MTU before and after autonomous restoration. Independent
IPv4/IPv6 management and existing-workload probes span the change. A dropped
probe, failed command, missing candidate, or incomplete restoration fails the
run; subsequent convergence does not erase that failure. Failed runs retain
workloads and guard state for diagnosis.

This fixture deliberately exercises a service-managed configuration. Restarting
k0s starts reconciliation outside the guard's process group, so command exit
and `rolled-back` state alone do not prove CNI restoration. The fixture requires
observed convergence of installed CNI configuration and newly created pods.
For multiple controllers, budget both the serial controller restarts and the
normal CNI DaemonSet rollout in each direction before arming any guard. A
single-controller fixture's timing does not establish a multi-controller
schedule. Candidate convergence must leave time for verification before the
first original rollback deadline. Keep each node's deadline immutable and
record source-file restoration separately from complete CNI reconciliation.
If the schedule cannot meet the required recovery bound, revise the deployment
procedure; do not force faster DaemonSet updates, shorten traffic observation,
or report late convergence as an on-time pass.
Production use additionally requires exclusive ownership across all controllers,
reconciliation after API loss, BGP resource restoration, and each site's routing
and workload compatibility. Do not substitute this isolated MTU rehearsal for
those deployment gates.

The fixture's apply and restore commands restart `k0scontroller`. On a
single-controller cluster this interrupts the API server even when host and
pod pings continue without loss. Its ICMP measurements therefore cannot prove
API, exec or logs availability. Measure those operations with the existing
[management observer](deployment-qualification.md#continuous-management-observations)
through both application and restoration. Any failed management sample fails
deployment qualification, including when configuration and connectivity are
subsequently restored within the rollback deadline.

Do not use this restart-based transaction as a zero-interruption migration for
a single-controller deployment. Qualify a configuration update path that keeps
the management endpoint available before applying the change. k0s supports
[dynamic configuration](https://docs.k0sproject.io/head/dynamic-configuration/),
but it must already be enabled on every controller; a static configuration file
transaction does not update the API-owned configuration or provide its rollback.
Changing configuration mode is a separate migration requiring its own ownership,
availability and autonomous recovery proof.
