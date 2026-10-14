# Node-local configuration rollback

`network-guard` bounds a migration of explicitly owned Linux configuration
files. Run it as an independent root systemd service before changing those
files. It uses local snapshots and a pinned restore executable; it does not
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
Create a root-owned executable that reapplies only the selected configuration.
It must be idempotent and must not flush shared routing tables, firewall rules,
or unrelated interfaces. Freeze other writers to the selected files until the
transaction is committed or rolled back.

The plan lists existing files and the SHA-256 of their exact candidate bytes:

```json
{
  "restore": "/usr/local/libexec/restore-owned-network-config",
  "files": [
    {"path": "/etc/example/owned-network.conf", "candidateSHA256": "REPLACE_WITH_64_HEX_DIGITS"}
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

Use a fresh transaction directory every time. Before applying the candidate,
require an active service, `ready.json`, `outcome: armed`, and enough remaining
time for the change and its measurements. A ready file alone is insufficient
because it remains as evidence after completion. Keep configuration ownership
exclusive; digest checks detect drift but do not provide a lock against other
software that ignores the transaction. The migration executor must finish or
cancel all candidate writes before restoration; this guard does not cancel an
external apply process. Integrating that ownership boundary is required before
using it for live migration.

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
connectivity; detailed command errors remain in the service journal. Qualification requires zero restore failures; eventual restoration
cannot turn a failed migration into a passing one.

The restore executable must finish synchronously and may use local netlink;
network socket families are denied by the service. Validate command process
cleanup, boot ordering, distribution-managed manifests, CNI reconvergence,
and preserved external routing for each actual site before using this on a
live management path. Kubernetes resource rollback needs additional design;
file restoration alone does not revert API objects.

## Disposable-VM qualification

Use [the native test](../harness/e2e/safety/network-guard-native.sh) inside a
throwaway Linux VM with systemd. It owns a dedicated dummy interface, changes
its MTU, requires autonomous restoration in three repetitions, kills the guard
in one repetition, and checks an independent management ping stream. It never
changes the VM's management interface or default route. That scope proves the
local watchdog mechanism; it does not qualify a real CNI or cluster migration.
