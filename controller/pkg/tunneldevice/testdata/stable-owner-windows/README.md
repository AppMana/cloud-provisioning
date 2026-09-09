# Windows stable address owner experiment

`TestNativeStableOwnerPackets` is an opt-in native Windows test that uses two
single-physical-NIC VMs. Run it as Administrator with the signed WireGuardNT DLL
beside the compiled test executable. Each VM owns three temporary adapters:
one address owner with no peers, and two generation adapters with no assigned
identity address. The existing `Native` backend still rejects duplicate owners.

Compile from `controller`:

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -cover \
  -o stable-owner.test.exe ./pkg/tunneldevice
```

Give each VM a fresh directory restricted to Administrators and SYSTEM. Set
`CLDT_NATIVE_STABLE_OWNER_PLAN` to its `plan.json`, then run:

```powershell
.\stable-owner.test.exe -test.v -test.run '^TestNativeStableOwnerPackets$' `
  -test.timeout 330s -test.coverprofile native.cover
```

The JSON plan contains `Names` (three distinct `cldt` plus eight hexadecimal
character names), `Run` (shared experiment identifier), `Remote` (remote test
address), `Owner` (a `PeersFileDoc` with no peers), and `Generations` (two
`PeersFileDoc` objects). All three local documents specify the same unused host
address. Each generation uses a distinct keypair and allows/routes exactly the
other VM's test host address. Never reuse production keys or addresses. Plans
contain private keys and must not enter logs, reports, or evidence archives.

Generation A listens on UDP 54871 and B on UDP 54872. Permit those ports only
between the two VM underlay addresses. Permit UDP for the test executable in
Windows Firewall; the echo server binds only to the stable test address on port
54900. Verify one physical NIC, the expected OS build, and the DLL signature and
hash before launching. Keep management independent of the tunnel under test. The native preflight rejects
an endpoint whose selected route uses an existing `cldt` adapter; inspect both
private and public endpoint routes before choosing the topology.

Wait until both processes publish `ready.json`. Then atomically publish
`start.json` on both VMs, with `Run` matching the plan and `Start` an RFC3339 UTC
time far enough in the future for delivery (for example, 40 seconds). A missing
barrier fails after 180 seconds. No successful warmup exchange also fails.

The experiment reuses one source-bound UDP socket across five ten-second phases:
A selected, select B, roll back to A, reselect B, and retire A. Windows
`GetBestRoute2` verifies selection after each change. The address must remain
preferred on its original owner throughout. Every sampled exchange records its
sequence, source, destination, timestamps and error in `events.jsonl`; warmup
samples are separate. `summary.json` counts measured exchanges and failures.

The sampler runs independently of route mutation and records a `phase-start`
event before each transition. The echo server remains available for three
seconds after sampling stops, beyond the peer's one-second request timeout;
this is test teardown, not a production drain policy. Retain the maximum sample
gap alongside failed exchanges. This test does not qualify production controller
receipts, delayed participants, source spoof rejection, CNI workloads, HNS/VFP
policy, or GPU workloads.

After collection and independent cleanup verification, run `validate.py` against
the directory containing `raw-2022-*`, `raw-2025-*` and their
`cleanup-YEAR-result.json` management receipts. It fails on a native error,
missing phase, changed source socket, sequence gap, failed exchange or missing
cleanup evidence. Sample-gap output is measured evidence, not a production SLA.

Record the native PID and original management command ID immediately. Collect
stdout, stderr, native exit code, coverage, public driver counters and JSON artifacts in a separate
management command after the process exits. A management observation timeout
must not launch another copy. Verify that all three owned adapters and the
process are absent, then remove only the experiment firewall/underlay rules and
private plans. Preserve failed attempts alongside successful attempts.

## Independent encrypted transport through a relay

If a retained cluster routes every peer address through its legacy mesh, use a
separate Linux VM whose address is absent from that mesh. `relay.py` relays opaque
WireGuard datagrams between exactly two permitted source IPv4 addresses, with
one socket per generation. Both Windows plans point generation A at the relay's
UDP 54871 and B at UDP 54872. The Windows nodes retain their own keys and decrypt
each other's packets; the relay has no WireGuard keys.

```sh
python3 relay.py --work-dir /var/tmp/cldt-relay-unique \
  --peer WINDOWS_2022_PUBLIC_IP --peer WINDOWS_2025_PUBLIC_IP --seconds 300
```

Permit the two ports only between the relay and the two Windows source addresses.
Verify both Windows endpoint route lookups choose the physical network. Wait for
the relay's `ready.json` before launching the Windows tests. The relay learns NAT
source ports independently for each generation; initial handshake packets can
arrive before the other endpoint is known, so retain the explicit warmup period.

The relay stops at its deadline or when a `stop` file appears in its work
directory. Preserve its PID, original management command ID and `summary.json`,
then verify termination and remove the owned ingress rules. This topology tests
cross-VM encrypted transport and host address/route behavior. It does not qualify
a direct cloud mesh or a production relay implementation.

## Source isolation with a positive control

Set both `Spoof` and `RemoteSpoof` in each protected plan to distinct, unused
addresses in the same family as the stable identities. The first is an
additional address on the local stable owner; the second is the other VM's
alternate source. Neither generation adapter owns these addresses.

The experiment initially permits the remote alternate source and installs its
return host route on both generations. Warmup must include a successful exchange
from that source through each generation, and the receiver records its arrival.
The test selects A, then B, then restores A; use a start barrier about 40 seconds
in the future so each control window can execute. Native route read-back uses
the actual alternate source address. Both normal and alternate-source return
routes follow each selection. This verifies that the
source address, return path, application, and experiment firewall rule can carry
the traffic before testing rejection.

At measurement start, the test removes the alternate source from both peers'
AllowedIPs and reads back the native peer key and sole remaining host allowance.
After a two-second application window, each phase sends two rejection probes
while the independent authorized sampler continues. A rejection requires a
successful UDP write followed by a read timeout. Any forbidden source reaching
the echo application fails the native test, even if a missing WireGuard return
allowance prevents its reply. Return host routes remain installed until adapter
cleanup, so removing a route is not used as the rejection mechanism.

Use `validate.py WORK_DIR --require-isolation` for these runs. It additionally
requires positive-control evidence through both generations at sender and receiver, ten rejection probes
per VM spanning all phases, native allowance read-back for both generations,
and successful authorized sampling during every rejection probe. This is native
WireGuard host-source qualification; pod policy and HNS/VFP workload isolation
need their own tests.
