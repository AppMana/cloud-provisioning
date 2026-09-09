# Native receive-overlap experiment

This experiment uses three existing QEMU VMs from the single-NIC harness:
`cp2` at `10.10.0.13` receives packets, while `w1` at `10.10.0.11` and `w2` at
`10.10.0.12` represent old and new source owners. All three must have serial
QGA access, Python 3, `ip`, and Linux WireGuard support. The runner refuses a
VM unless sysfs identifies exactly one physical NIC.

From the controller module:

```sh
CGO_ENABLED=0 go build -o /tmp/handover-vm ./pkg/tunneldevice/testdata/handover-vm
python3 pkg/tunneldevice/testdata/handover-vm/run.py \
  --binary /tmp/handover-vm \
  --outer cldt-kuberouter-runner-v1 \
  --work-dir /tmp/handover-vm-evidence
python3 pkg/tunneldevice/testdata/handover-vm/validate.py /tmp/handover-vm-evidence
```

The work directory must not exist. Each invocation assigns a fresh namespace,
executable path, keys and readiness files. Do not rerun after an observation
timeout: inspect the original runner process and its `events.json`. Native
servers have bounded lifetimes, and every spawned process is collected before
namespace cleanup. Preserve failures and cleanup errors.

The helper creates only experiment WireGuard devices in the VM's host network
namespace and moves them into a separate namespace. The encrypted UDP sockets
remain in their birthplace namespace, using the existing physical NIC. It does
not change the VM's physical addresses, product devices, CNI, or host routing.
UDP ports 54871 and 54872 must be available. New device creation fails on a name
collision. Readiness and stop files, plus the executable, remain in `/tmp` for
inspection; `run.json` identifies them. Remove only those run-owned files after
all native processes have terminated.

Each sender uses the same test source addresses (`10.254.99.11` and
`fd99:99::11`), but separate keys and receive devices. Warmup is separate from
the continuous request streams. The runner tests old-path traffic across
preparation of the second generation, rejection of unallowed source addresses,
new-path traffic across retirement of the old receive device, and rejection
of traffic through that retired path. Echo sockets are explicitly bound to
their generation's device. This does not test automatic production egress
selection, the controller receipt protocol, CNI traffic, or Windows.

Evidence includes timestamped operation events, every request and expected
rejection, receiver logs, physical NIC observations, and native process exit
codes. `keys.private.json` contains ephemeral private keys: keep the evidence
directory private and exclude that file from reports or shared archives. The
[recorded packet experiment](../../../../../docs/validation/handover-packet-results.json)
distinguishes these checks from full production handover qualification.

Use `--egress` for the extended matrix. It adds ordinary UDP sockets without
`SO_BINDTODEVICE`, selects generation B with a preferred route, rolls back to A,
selects B again, and retires A while the same sockets keep sending. IPv4 and
IPv6 route lookups record the actual selected device at every boundary. The
old route remains available during overlap for replies explicitly bound to A.
This verifies isolated Linux route selection, not production policy-routing
integration, controller receipts, restart recovery, or cross-OS behavior.
