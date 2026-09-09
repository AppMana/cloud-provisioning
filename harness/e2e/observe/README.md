# Native packet-drop analysis

## Ordinary-pod matrix

`pod_matrix.py` tests existing ordinary agnhost `netexec` pods on distinct nodes.
It runs every directed node pair through TCP echo bodies of 1 KiB, 16 KiB and
1 MiB, Service and DNS hostname checks, and five UDP payload sizes with ten
attempts each. Three nodes produce 60 checks, including 300 requested UDP echoes.
Each pod needs a same-name Service on TCP 8080, a UDP listener on 8081, and curl
(`curl` on Linux or `C:\curl\curl.exe` on Windows). The runner selects the command
from the observed Node OS. It does not create pods, choose images or install CNIs.

```sh
python3 observe/pod_matrix.py \
  --api-server https://ISOLATED_API:6443 --bastion TEST_BASTION \
  --namespace PROBE_NAMESPACE \
  --target LINUX_POD=LINUX_NODE_UID \
  --target WINDOWS_2022_POD=WINDOWS_2022_NODE_UID \
  --target WINDOWS_2025_POD=WINDOWS_2025_NODE_UID \
  --output NEW_EVIDENCE_FILE.json
python3 -m unittest discover -s observe -p 'test_*.py'
```

Use the explicit Node UIDs from the lifecycle stage being tested. The runner
requires Ready Nodes and ordinary single-container pods, then rechecks Node,
Pod, container and Service identities after traffic. A replaced or restarted
container invalidates the result even if its traffic succeeded. Failed executor
commands, wrong bodies and incomplete UDP responses fail the gate. Evidence
retains hashes and raw UDP responses; an existing evidence file is not overwritten.

This runner requires working Kubernetes exec. For k0s Windows nodes that includes
the Konnectivity listener and agent; validate that path independently. A failed
exec is a failed observation, not proof that the pod network dropped packets.
Use the VM/SSM runtime executor described below when diagnosing an unavailable
exec path. Policy enforcement, own-pod Service hairpin, sustained reliability and
gateway failover require separate tests.

For a fragmentation-boundary sweep, retain the same explicit API, namespace,
target and output arguments and add:

```sh
--udp-only --tries 100 \
  --udp-payload-bytes 1280 --udp-payload-bytes 1337 \
  --udp-payload-bytes 1338 --udp-payload-bytes 1340 \
  --udp-payload-bytes 1400 --udp-payload-bytes 1800
```

These sizes straddle the observed IPv4 request boundary for a 1,370-byte workload
MTU: the `echo ` prefix, UDP header and IPv4 header add 33 bytes. Measure the
actual workload interface MTU first; these sizes are not universal network
limits. Repeated size arguments replace the defaults. Sizes must be unique and
between 1 and 2,043 bytes so the command prefix fits agnhost's receive buffer.
The result records parameters and each UDP case's start/end time for capture
correlation. Two targets produce 12 cases and 1,200 requested echoes with the
arguments above. `--udp-only` excludes TCP, Services and DNS traffic checks but
retains their existing target identity preflight. A successful finite sweep
does not supersede earlier failures or establish sustained reliability.

### UDP socket and pacing diagnostics

`cmd/udpprobe` sends sequential echoes from an ordinary container to an agnhost
UDP listener. Unlike agnhost's `/dial` loop, it can retain one socket across
attempts. Each body contains a random run identifier and sequence number, so a
late reply from a previous attempt fails comparison. It reports each attempt's
local socket address, timestamps, exact-match result and error, and exits nonzero
if any attempt fails.

Build for the target guest and stage the executable in the disposable probe
container before running it there:

```sh
GOOS=windows GOARCH=amd64 go build -o udpprobe.exe ./cmd/udpprobe
# Inside the ordinary Windows probe container:
udpprobe.exe -destination TARGET_POD_IP:8081 -payload-bytes 1400 -tries 100
udpprobe.exe -destination TARGET_POD_IP:8081 -payload-bytes 1400 -tries 100 -reuse-socket
udpprobe.exe -destination TARGET_POD_IP:8081 -payload-bytes 1400 -tries 100 -reuse-socket -interval 10ms
```

Use `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build` for a portable Linux
executable: the ordinary probe image may not contain the build host's dynamic
loader. Preserve Node, Pod and container identity
snapshots before and after the comparison, and remove the staged executable when
done. The `interval` is a delay after each completed attempt, not an enforced
packet rate. Each receive has a five-second deadline. These are diagnostics;
pacing or socket reuse must not replace the unpaced connectivity gate.
The [native socket comparison](../../../docs/validation/windows-udp-socket-reuse-results.json)
observed timeouts after successful requests on reused sockets on both Windows
versions. That run did not capture packets and does not locate its timeouts.

For independent send scheduling, use the foreground diagnostic mode:

```sh
udpprobe.exe -fixed-cadence -destination TARGET_POD_IP:8081 \
  -payload-bytes 96 -tries 4500 -interval 20ms
```

This uses a fresh socket and exact nonce echo for each request while earlier
requests may still be awaiting replies. `scheduledAt`, `startedAt` and
`finishedAt` distinguish scheduling delay from reply latency. Results remain
ordered by sequence, even when replies finish out of order. A missed send slot
or the 512-request concurrency bound produces an explicit skipped failure with
no source socket; it is never counted as a transmitted packet. The run permits
at most 10,000 slots over ten minutes of scheduling, followed by outstanding
reply deadlines. It cannot be combined with socket reuse or background stream
mode. This diagnostic does not change the sequential survivor acceptance gate:
a serial probe's five-second pause after a missing reply alone is not evidence
of a five-second network outage.
The [native fixed-cadence comparison](../../../docs/validation/windows-fixed-cadence-results.json)
retains seven lost Windows replies and 41 unsent slots separately. Following
successful requests started about 16–24 ms after the lost requests and completed
before those requests' receive deadlines. The Linux retry used a separate
observation window after correcting an incompatible dynamic binary; it is not
a simultaneous control for the Windows results.

### Bounded background survivor samples

`udpprobe -stream-dir NEW_DIRECTORY` starts a bounded child inside the ordinary
probe container. The launch command exits while the child records fresh-socket
UDP echoes locally, avoiding an SSM round trip per packet. Supply the observed
Pod IP with `-source-ip`; a different source socket fails the sample.

```text
udpprobe.exe -stream-dir C:\uploads\survivor-run-1 \
  -destination TARGET_POD_IP:8081 -source-ip SOURCE_POD_IP \
  -payload-bytes 64 -interval 100ms -max-duration 1h -max-gap 10s
udpprobe.exe -stream-dir C:\uploads\survivor-run-1 -status
# After the observed operation finishes:
udpprobe.exe -stream-dir C:\uploads\survivor-run-1 -stop
udpprobe.exe -stream-dir C:\uploads\survivor-run-1 -status
udpprobe.exe -stream-dir C:\uploads\survivor-run-1 -dump > samples.jsonl.gz
```

Use Linux paths and the Linux binary on Linux. Directories must be new. Status
returns a bounded JSON document; inspect its fields, because a successful status
read does not imply healthy traffic. Observe advancing counts before starting an
operation. Request stop after it finishes and wait for a terminal result: the
child sends one final echo after reading the stop request. Duration expiry is a
failed observation without an explicit stop acknowledgement. Preserve the
original process and evidence if a control command times out.

Each log row carries a run nonce, process ID, sequence, actual source socket,
monotonic start/finish offsets and cumulative health. Failures remain recorded.
The terminal result hashes the complete JSONL log. Collect compressed logs after
termination through a binary-safe transport. Kubernetes exec requires a working
kubelet path; capture stdout as bytes and avoid Windows PowerShell 5.1 text
redirection. Linux hosts can use the bounded out-of-band collector below because
SSM inline output is too small for a complete log. `survivor_stream.evaluate` independently checks hashes, sequence,
source identity, timing, counts and the final stop sample. The caller must also
verify stable Node/Pod/container identities and host-side ordering of progress,
operation and stop; comparing guest wall clocks alone is insufficient.

The [native qualification](../../../docs/validation/survivor-native-stream-results.json)
passed all 1,303 64-byte echoes across six directions between ordinary Linux,
Windows Server 2022 and Windows Server 2025 Pods. Background processes survived
launch-command exit, and atomic stop/result publication worked on both Windows
versions. Maximum sample start interval was 0.241 seconds against a preset
10-second limit. Temporary executables and logs were removed after collection.
This short qualification used Kubernetes exec for control. Out-of-band lifecycle
integration remains required; it does not supersede the earlier lifecycle
cadence failure or fragmented-UDP failures. Native logs also provide regression
fixtures for the independent validator.

### Out-of-band native control

`survivor_native.Session` controls pre-staged native probes using
`survivor_oob.OutOfBand.exec_result`. Supply the same trusted Node UID to OS/CRI
argv mapping used by the existing observer, plus a Node UID to container-local
probe executable mapping. The runtime command always selects the full
containerd container ID from the ordinary Pod identity snapshot. Both QEMU
serial management and AWS SSM use this interface.

For Linux hosts with Python 3, `survivor_dump.dump_linux` collects completed
sampler logs through the host executor and exact-container CRI command. Supply
`hostCommand` in each executor entry: it is the argv prefix for running a host
command, and `command` extends that prefix with crictl and its runtime flags.
Pass a callback invoking `dump_linux(executors, source, argv)` to `Session.finish`.
This path keeps collection available when Kubernetes exec fails.

Responses contain at most 12,000 binary bytes encoded as base64, keeping each
JSON response below SSM's inline limit. Every chunk binds its offset, total size,
and the whole compressed-file SHA-256. Collection rejects truncation, a changing
file, or a hash mismatch; the existing validator then checks the decompressed
samples against the original sampler receipt and terminal result. The collector
does not restart or stop samplers. It supports Linux host execution only; Windows
requires a separately qualified binary-safe transport.
The [native collector report](../../../docs/validation/survivor-oob-dump-results.json)
records the byte-for-byte AWS comparison and unit coverage.

Create a new Session output directory with `start`, then call `progress` twice
with distinct labels and `ready` with those labels. Readiness requires increasing
counts from the original nonce/PID and cumulative healthy traffic. `stop` requests
a final sample on each original process. Every control command has an exclusive
intent and response document; a repeated start or stop is rejected locally.
A timeout leaves the original intent for inspection. Processes retain their
configured maximum lifetime even if the controller exits.

`assess_window` checks that host-side readiness preceded an operation and every
stop request followed it. It is only an ordering check: independently validate
terminal logs with `survivor_stream.evaluate` and recheck the complete Pod/Node/
container identity snapshot before accepting a lifecycle result. The legacy
`survivor.py` log format is not interchangeable with native Session evidence.
For `aws/windows_lifecycle.py`, set `observerMode` to `native-oob` and
`observerExecutors` to the trusted executor-mapping JSON path; keep the existing
`observerDirectory` and `survivorTargets` configuration. Start the native Session
with pre-staged probe executables before running the trial. Native mode verifies
the complete directed lane matrix, reads two advancing OOB progress rounds, and
checks live Pod identities before both addition and removal. After convergence
it stops the original probes, retrieves gzip logs, validates all samples and
host control ordering, and writes `survivor-result.json`. A false result fails
the trial even after successful machine removal. Retain and inspect guest files
if stop or collection fails; remove only owned probe files after collection.
`operation-window.json` records the completed lifecycle timestamps before
collection. `operation.json` is finalized afterward and cannot report combined
success when native traffic failed or when the legacy external observer is still
pending. `lifecyclePassed` records successful resource convergence independently.

Omitting `observerMode` preserves the existing observer. Native integration has
unit coverage and replay of the actual 3,603/3,604 failed run, including sticky
failure after traffic recovery. The [real native lifecycle trial](../../../docs/validation/windows-capi-native-survivor-2022-results.json)
passed 19 join checks and its 72-check network matrix, then failed the native
survivor gate. Cleanup removal preserved all 13 original Ready Nodes and five
gateway attachments. All six native lanes bracketed the operation with a maximum
start interval of 5.105 seconds against the 10-second limit, but only 82,924 of
82,933 echoes succeeded. The nine losses remain failures; cleanup does not turn
this into a successful uninterrupted lifecycle qualification. All temporary
probe and management tools were removed after terminal log collection.

The [native OOB qualification](../../../docs/validation/survivor-native-oob-results.json)
completed all launch/status/stop commands through VM management and preserved
stable Pod identities. Traffic failed: 3,603 of 3,604 64-byte echoes succeeded;
one Windows 2022 to Windows 2025 receive hit its five-second deadline. Readiness
correctly failed before any lifecycle operation. Subsequent successful samples
remained cumulatively unhealthy. This failed native log is a regression fixture;
without packet capture it does not identify where the request or reply was lost.
All six original samplers stopped and temporary Pod and Windows host tools were
removed after collection.

## Packet Monitor correlation

`pktmon.py` reads Windows Packet Monitor text exports and correlates an observed
UDP flow with nearby drop events. It accepts native UTF-16 exports and UTF-8
copies. Use captures taken on the actual worker VMs; replaying the fixtures tests
the correlator, not Windows networking.

```sh
python3 observe/pktmon.py capture.txt \
  --source SOURCE_POD_IP --port SOURCE_UDP_PORT --destination TARGET_POD_IP
python3 -m unittest discover -s observe -p test_pktmon.py
```

The default destination port is the agnhost UDP listener at 8081. The JSON result
reports whether the original flow was observed, matching drops, component names,
IPv4 identification values and fragment offsets. Correlation uses endpoint
addresses, protocol, IPv4 ID and a bounded time window. It does not require
unchanged packet-group identifiers or trust the ports rendered in a drop event;
these differ in the native fixtures. Review the packet sequence and capture
completeness alongside its output. It does not diagnose the driver's internal
reason for rejecting a packet.

An echo timeout can also mean the reply was dropped. Inspect both directions;
for the reply, use the listener's source port and the client's ephemeral
destination port from the failed attempt:

```sh
python3 observe/pktmon.py RECEIVER_CAPTURE.txt \
  --source SERVER_POD_IP --port 8081 \
  --destination CLIENT_POD_IP --destination-port CLIENT_EPHEMERAL_PORT
```

Read `vfpctrl.exe /get-frag-cache-stats /port PORT_GUID` before and after
traffic, using the VFP port mapped to the exact ordinary pod's network adapter.
Retain both directions' raw counters and capture evidence. Cumulative counters
alone do not attribute a timeout or identify an internal driver defect. The
[fragment-cache fixture](../../../docs/validation/windows-udp-collision-capture-results.json)
retains the packet-level evidence separately from those counters.

A first IPv4 fragment can legitimately display a UDP length larger than that
fragment's payload. Require an actual drop event; the decoder annotation alone
is not evidence of malformed traffic. Duplicate appearances across Windows
networking components are not duplicate application requests. Test fragmented
and unfragmented traffic, and sequential and concurrent streams separately.
A passing case cannot locate a failure from an earlier run.

Capture with the built-in `pktmon` tool, preserving preexisting monitor/filter
state. Include both communicating Windows hosts and the site-side host when a
path crosses the site. Stop the owned captures before exporting, check each
stop result for lost events, and compare downloaded artifact hashes with native
hashes before removing guest files. Remove only the test's filters and files.
Retain the raw ETL, verbose text, ordinary PCAP and drop-only PCAP:

```sh
pktmon etl2txt capture.etl --out capture.txt --verbose 3
pktmon etl2pcap capture.etl --out capture.pcapng
pktmon etl2pcap capture.etl --drop-only --out drops.pcapng
```

Ordinary PCAP appearances and native drop events are separate populations.
A header change visible only at a drop must be compared with successful packet
appearances, including decapsulated packets. Keep the native text alongside
PCAP: IP-only and Ethernet records can require different decoding. Command
details are in [Microsoft's Packet Monitor documentation](https://learn.microsoft.com/en-us/windows-server/networking/technologies/pktmon/pktmon-syntax).

For decoded UDP request/reply comparison, export the fields expected by
`udp_capture.exchange(text, source_ip, source_port, destination_ip)`:

```sh
tshark -r capture.pcapng -Y 'udp.port == CLIENT_EPHEMERAL_PORT' \
  -T fields -E occurrence=a -e frame.number -e frame.time_epoch \
  -e ip.src -e ip.dst -e ip.id -e ip.dsfield \
  -e udp.srcport -e udp.dstport -e udp.payload > packets.tsv
```

Replace `CLIENT_EPHEMERAL_PORT` with the port recorded by the failed native
sample. The helper selects the inner datagram when VXLAN is decoded and reports
request/reply appearances separately. An absent appearance does not prove
non-delivery. A client timeout can coexist with a reply in a receiver capture;
continue along the reply path. Do not compare different VM wall clocks as
transit latency.

The [dual-receiver fixtures](../../../docs/validation/windows-dual-receiver-drop-results.json)
cover TCP/IP `Not locally destined` drops on both Windows versions and a failed
client exchange with a visible reply. The
[unfragmented VFP fixture](../../../docs/validation/windows-small-udp-vfp-drop-results.json)
retains a different drop path. Keep these cases distinct until native evidence
establishes a common cause; none qualifies Windows networking or a mitigation.

### TCP/IP interface context

Capture the Windows TCP/IP provider alongside Packet Monitor when a drop needs
interface context. Preserve the raw ETL and native `Get-WinEvent.ToXml()` records,
plus `Get-NetIPInterface -IncludeAllCompartments` and
`Get-NetIPAddress -IncludeAllCompartments` snapshots. Keep the event provider,
version, timestamp and named data fields. An exporter can finish after its SSM
observer expires: inspect the original process and retained files before retrying.

`tcpip_context.nearby(events_jsonl, pktmon_timestamp, outer_source,
outer_destination, interface_snapshot)` returns candidate Event 1215 records
from the same host within the preceding ten microseconds. JSONL records contain
an `xml` field; snapshots contain `interfaces` and `addresses` arrays. Use the
outer VXLAN endpoints, and pass the native Packet Monitor drop timestamp. The
helper supports the observed Windows 2022 version 1 and Windows 2025 version 2
schemas. It preserves multiple candidates and missing interface mappings instead
of guessing. Snapshots describe their own observation time, not a guaranteed
interface state at every packet event.

Do not require `IPTransportProtocol == 17`: the
[three-host native fixtures](../../../docs/validation/windows-tcpip-site-capture-results.json)
report protocol `0` for TCP/IP events immediately preceding both request and
reply VXLAN drops. Their interface indices map to compartment 2 link-local
adapters in the accompanying snapshots. Event 1215 lacks a packet identifier;
outer-address/time correlation supplies context, not independent packet identity
or proof of the cause. The Linux site-path timeout remains a separate finding.
The post-capture `Get-NetCompartment` observation identifies compartment 2 as
`DR` on both hosts. HNS network mappings are retained in the report; the presence
of both `Calico` and `External` overlay networks does not establish obsolete code
or justify deleting either network.
Inspect the corresponding VFP port's parser settings, address information,
layers and inbound decapsulation rules before choosing a configuration change.
The [native DR configuration reference](../../../docs/validation/windows-dr-configuration-results.json)
contains both Windows versions' snapshots. Enabled ports, matching routes and
present rules describe static configuration; they do not explain a transient
packet failure. Keep those checks separate from packet-level qualification.

```sh
PYTHONPATH=observe python3 -m unittest test_tcpip_context test_pktmon test_udp_capture
```

### VFP flow context

Run `windows_vfp_trace.ps1` as Administrator inside the selected Windows VM:

```powershell
.\windows_vfp_trace.ps1 -Directory C:\diagnostics\new-vfp-run -Seconds 15
```

The directory must be new. The collector reads native provider metadata, records
up to 60 seconds of VFP ETW events into a circular file (64 MiB by default,
configurable with `-MaxFileMiB` from 16 through 256), and stops its
uniquely named session in a `finally` block. It requires trace filtering to be
disabled and verifies that this setting remains unchanged. Forwarding rules and
flow settings are untouched. It writes the ETL, provider schemas and a bounded
result containing artifact hashes. XML export is deferred by default so a paired
packet capture can stop promptly. For a small diagnostic sample, add
`-ExportXml -MaxXmlEvents 10000`; the compressed XML JSONL contains at most that
many events, and `xmlEventLimitReached` explicitly marks a reached limit. A
zero `eventCount` with `xmlExported=false` means no XML was requested, not that
the ETL contains no events. Collect the files through
out-of-band management, verify their hashes, then remove the owned directory.
If the management command times out, inspect its original invocation and named
session before starting another capture. Forced process termination can prevent
the `finally` block from running. A circular trace and successful export alone
do not prove that no events were lost.

Add `-IncludeControl` when investigating configuration changes. It enables both
`Control` (65536) and `ControlValidation` (4194304), using mask `0xcd0e0f`.
The native provider assigns IOCTL/object-processing events 700–706 to
`ControlValidation`; enabling `Control` alone does not select them. Control
timer events can be extremely frequent, so use exact time windows for export
and retain any query-limit indication when interpreting the results.
The [corrected-keyword validation](../../../docs/validation/windows-vfp-control-keywords-results.json)
checks native provider keyword values and bounded collection on Windows Server
2022 and 2025. It validates the collector, not a network mitigation.

For flow-lifetime and IOCTL context, use `-FlowControlOnly` instead of
`-IncludeControl`. Its `0xc80000` mask selects `UnifiedFlow`, `Flow`, and
`ControlValidation`, omitting per-layer and control-timer events. The switches
are mutually exclusive. The [native focused-profile smoke test](../../../docs/validation/windows-vfp-focused-collector-results.json)
verifies keyword values and collection on both Windows versions. It does not
establish retention improvement or network qualification.

HNS dynamic events need a different decoder. In the
[native HNS collector validation](../../../docs/validation/windows-hns-collector-results.json),
`Get-WinEvent` produced XML `ProcessingErrorData` with code 15003 for many HNS
records. Treat these as undecoded records. After stopping the named HNS and
Host Network Management trace sessions, native
[`tracerpt`](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/tracerpt)
decoded all HNS provider records from the same ETLs:

```powershell
tracerpt C:\diagnostics\RUN\hns.etl C:\diagnostics\RUN\hnm.etl `
  -o C:\diagnostics\RUN\hns.xml -of XML
```

Use a new output filename, verify counts by provider, and retain decoding errors
from other providers. Keep request payloads private. An `ActivityError` task
name alone is not a failed-operation result; inspect the associated result
code. Match request, flow and packet timestamps before attributing a drop to
an HNS operation.

`hns_context.layer_rebuilds(xml, port)` pairs native `RemovingLayer` and
`AddingLayer` records for one host's exact DR port. It uses activity IDs and
event order because a single activity ID may cover multiple rebuilds. Incomplete
end-of-capture removals remain incomplete; undecoded HNS events, repeated
unmatched removals and reversed timestamps fail analysis. Native timestamps are
preserved; reported durations use microsecond resolution.

`hns_context.network_space_removals(xml, port, network_id)` locates removal
of a network's IPv4 and IPv6 VFP spaces on that exact port. Native records
show this work occurring more than 30 seconds after the HNS API object was
deleted. Preserve the API deletion, space-removal and measurement timestamps
separately when evaluating a removal experiment. This function reports observed
operations; an empty result does not prove that dataplane cleanup has finished.

The [paired layer-gap result](../../../docs/validation/windows-hns-layer-gap-results.json)
contains six packet drops inside HNS encapsulation-layer rebuild intervals on
Windows Server 2022/2025. Matching inbound flows recorded `Ignore` during each
interval. The [native fixtures](testdata/windows-hns.md) test this combined
observation and reused activity IDs. They validate analysis, not a workaround.
Do not delete the `External` overlay network based on its name: the reviewed
Calico fork deliberately preserves that network type.

`vfp_context.flow_events(text)` decodes the observed version-0 IPv4 rule,
processing and unified-flow events. Feed it the decompressed `events.jsonl.gz`.
Native `win:IPv4` and `win:Port` XML fields expose integer storage values; the
decoder converts their byte order explicitly and preserves the original fields.
Event 354 indicates an inner-header forwarding fallback, so it must not be
counted as a packet drop. Correlate flow tuples and timestamps with same-host
Packet Monitor events and ordinary-Pod samples; a tuple match alone is not
packet identity.

The [Windows 2022/2025 native collector validation](../../../docs/validation/windows-vfp-collector-results.json)
contains artifact hashes, unchanged-node checks and fixture provenance. It used
background traffic to verify collection and decoding. Controlled UDP loss
reproduction and causal analysis require a separate paired capture.
The [paired-capture result](../../../docs/validation/windows-vfp-paired-results.json)
documents why capture and export are separated: full XML conversion delayed
Packet Monitor shutdown enough to overwrite earlier packets. Bound both capture
windows and stop both before doing bulk conversion. The observed unchanged
traffic-class VFP drop extends the earlier fixtures without establishing its
cause.

For targeted context after both captures stop, use `windows_vfp_events.ps1` with
the receiver's Packet Monitor timestamp:

```powershell
$drop = [DateTimeOffset]::Parse('2026-09-09T01:43:49.6395951Z')
.\windows_vfp_events.ps1 -Path C:\diagnostics\run\vfp.etl `
  -Output C:\diagnostics\run\drop-window.jsonl.gz `
  -StartUtc ($drop.AddMilliseconds(-1)) -EndUtc ($drop.AddMilliseconds(1)) `
  -MaxEvents 1000
```

The output filename must be new, and the window must be ordered and no longer
than one second. The exporter uses XPath for native selection, then checks the
exact UTC bounds and VFP provider GUID. This matters on the observed Windows
builds: a combined hashtable query missed known ETL events, and native XPath
selection included some records just before a sub-millisecond lower bound.
`outsideWindowEventCount` records those excluded events. They still consume the
query limit: `eventLimitReached=true` means the result is incomplete even when
`eventCount=0`. Verify the output hash and `sourceUnchanged` before analysis.
Use `-EventId 600` for new-flow records, or `-EventId 700,701,702,704,706`
for selected control records. The optional list accepts up to 20 numeric IDs,
filters before the query limit, and checks IDs again before export. Omit it for
all VFP event types. Preserve `eventIds` with the result: an empty selection
result does not establish absence of other event types. Busy windows can still
exceed the limit; narrow the event selection or time window before interpreting
them as complete.
The [native IOCTL-context result](../../../docs/validation/windows-vfp-ioctl-context-results.json)
validates event-ID selection, including reached limits, an empty result and
known records on both Windows versions. Its Windows 2022 drop coincided with
HNS-issued VFP operations and preceded a native encapsulation-layer add. The
record preserves the timing and process evidence without identifying the
upstream HNS caller or claiming a fix. Existing HNS policy logs in that result
begin after capture stop and cannot explain the earlier drop.

`vfp_context.inbound_vxlan_creations(text, source, source_port, destination,
destination_port, port_name)` selects newly created inbound UDP VXLAN flows on
the explicitly observed DR port. It preserves the raw transformation and reports
its encapsulation action. Protocol 252 records use encapsulation selectors;
their values are preserved separately from transport ports.
The [native failed/control comparison](../../../docs/validation/windows-vfp-decap-context-results.json)
found `Ignore` in three failed inbound flows and `Pop` in their six immediately
neighboring successful exchanges on the same paths. This narrows the investigation
to the classifier decision; it does not identify why that decision differed or
qualify a mitigation.
The [native tuple simulations](../../../docs/validation/windows-vfp-tuple-simulation-results.json)
subsequently produced `Pop` for all six tested historical failed/control tuples,
with unchanged layer listings. These simulations exercise current classifier
state; they do not reproduce the intermittent live failures.
The [subsequent paired observation](../../../docs/validation/windows-vfp-control-context-results.json)
recorded 10 failures in 27,426 UDP attempts. Two correlated drop windows again
contained matching inbound flows with `Ignore`. Its older control mask omitted
`ControlValidation`, and its broad control exports reached their limits before
the drops; neither supports conclusions about concurrent IOCTL activity.

## UDP result gate

`udp.py` evaluates the raw JSON body from an agnhost pod's loopback `/dial`
request. Run curl through the source VM's guest executor and container runtime
so the UDP traffic originates inside the ordinary pod. The executor command is
passed as an argument list without a shell; use the VM or AWS adapter appropriate
to the source machine. The checker does not provision pods or select a CNI.

```sh
python3 observe/udp.py --payload-bytes 1400 --tries 10 --body raw-dial.json
python3 observe/udp.py --payload-bytes 1400 --tries 10 --timeout 90 -- \
  NODE_EXECUTOR RUNTIME_EXEC SOURCE_CONTAINER CURL --silent --show-error \
  --max-time 60 --noproxy '*' \
  'http://127.0.0.1:8080/dial?host=TARGET_POD_IP&port=8081&protocol=udp&tries=10&request=echo%20LOWERCASE_X_PAYLOAD'
python3 -m unittest discover -s observe -p 'test_*.py'
```

Replace the command placeholders with the machine's actual executor, runtime,
container ID, curl path, destination and the specified number of lowercase `x`
characters. The probe must enable its UDP listener. Payload plus the five-byte
`echo ` prefix must fit agnhost's 2048-byte buffer. Give the outer executor enough
time for all attempts, including the five-second timeout of each failed attempt.
Record the executor status with a saved body and pass it using `--exit-code`.

Exit zero requires every requested response to match exactly, no reported errors,
and a successful executor. Counts describe reported application echoes, not wire
packet loss. [The tested agnhost implementation](https://github.com/kubernetes/kubernetes/blob/v1.36.2/test/images/agnhost/netexec/netexec.go)
can omit earlier successful responses when the last attempt fails. The checker
reports that omission explicitly and fails the gate; it does not infer unseen
successes or treat a null value as a response. Preserve raw bodies for replay.
These parser tests validate result handling, not the Windows network dataplane
or the complete CAPI add/remove matrix.

[Native checker evidence](../../../docs/validation/windows-udp-checker-results.json)
records successful raw responses from Windows 2022, Windows 2025 and Linux pods,
plus a deliberate unused-port timeout. The timeout fixture comes from the
Windows 2022 source pod: curl exited successfully while the gate correctly
failed. It is not an example of the intermittent VFP drops.


## Windows GPU application observations

`windows_gpu.py` observes an existing render/NVENC Job without creating or
retrying it. Supply the expected Node UID, Pod UID, Job name and the immutable
probe identity file (`configMapName`, `configMapUID`, `binarySHA256`,
`scriptSHA256`). Omit `configMapName` only for the original `windows-gpu-probe`.
Use a new immutable ConfigMap and identity file when revising the probe. The observer
checks the pinned image, ordinary-container contract, one-device request,
unchanged probe bytes, terminal Job/Pod/container status and validated hardware
render/encode JSON. Pending image preparation is a failed acceptance check,
not a passing GPU result. Keep each failed attempt before applying a correction.

```sh
python3 observe/windows_gpu.py --api-server https://ISOLATED_API:6443 \
  --bastion TEST_BASTION --namespace cldt-windows-gpu \
  --pod POD --pod-uid POD_UID --node-uid NODE_UID --job JOB \
  --probe-identity PROBE_IDENTITY.json --output NEW_OBSERVATION.json
```

The current probe source and deployment are in
[Windows GPU probes](../../../images/probes/windows/README.md).

## Windows image-cache observations

`windows_cache.py` evaluates the JSON produced by the read-only
[`inspect-cache.ps1`](../../../images/windows/inspect-cache.ps1) on an unjoined
image builder. Supply the staged runtime manifest, target Windows version and
each expected digest-pinned image. It requires matching runtime hashes, the
exact image set fully downloaded and unpacked, and no workload containers, tasks,
worker service or known cluster identity files. Missing fields fail the check.
See the [image preparation commands](../../../images/README.md#windows-layers).

The native unfinished-pull fixture contains a registered image that is not yet
unpacked. It must fail acceptance. Passing this observer establishes running
builder readiness only; it does not establish service shutdown, capture hygiene,
Sysprep success or reuse of cached layers on a fresh CAPI worker.

## CAPI removal and survivor observations

`removal.py` compares snapshots containing native `nodes`, `machines`, and
`configmaps` List responses from the same isolated cluster. Supply the completed
`cmd/awsremove` receipt for the removed worker. The comparison verifies that the
receipt identifies the original Machine/Node/provider, that all other Node and
Machine identities survive, that every surviving Node is Ready, and that the
removed worker's gateway leases are Complete while the other active leases
return to Ready with unchanged identities and plans.

```sh
python3 observe/removal.py --before BEFORE.json --after AFTER.json \
  --receipt REMOVAL.json --output COMPARISON.json
```

Before deleting a worker in a Ready-attachment test, capture the baseline and
run this read-only preflight with the Machine UID recorded for that test:

```sh
python3 observe/removal.py --preflight --before BEFORE.json \
  --claim "$claim" --machine-uid "$machine_uid" --output PREFLIGHT.json
```

The preflight requires healthy Nodes, the original Machine/Node association,
and Ready target and survivor gateway attachments. Node readiness alone does
not establish attachment readiness. Re-capture and re-check immediately before
deletion if state changed. Testing deletion during Publishing is a separate
scenario; it cannot satisfy the Ready-baseline comparison by rewriting its
original snapshot. Every comparison and preflight requires a new output path.
[Native preflight validation](../../../docs/validation/windows-removal-preflight-results.json)
records acceptance of the current Ready baseline and rejection of the earlier
Publishing target despite all Nodes being Ready.

Capture a baseline with healthy Nodes before deletion. Wait for the existing
removal operation and gateway acknowledgements; a `Publishing` lease does not
pass the final check. Preserve intermediate failed comparisons. Run separate
application and network probes on surviving workers: unchanged identities and
Ready conditions alone do not demonstrate working traffic or GPU workloads.

For Windows tunnel continuity, build `controller/cmd/tunnelobserve` for Windows
and place it beside the signed WireGuardNT DLL on the disposable test VM. It
opens an existing `cldt` adapter and samples its state without applying
configuration, creating adapters, or changing routes:

```powershell
.\tunnelobserve.exe -interface cldt01234567 -samples 600 -interval 1s -report C:\protected-test-directory\peers.jsonl
```

The report must be new. Each JSONL record contains the UTC observation time,
adapter state, hashed public peer identities, endpoints, byte counters, and
native handshake timestamps in Windows FILETIME units. Private and preshared
keys are never serialized. Sampling is bounded to one hour and 3,600 records.
Keep the guest report when a local SSM observer times out; inspect the existing
command before deciding whether collection has ended. Large reports exceed
SSM's inline output limit and must be transferred in bounded chunks or analyzed
on the VM. Counter resets can demonstrate peer-state churn, but attributing
packet loss requires matching packet/probe timestamps and further evidence.

To test the Windows peer-update configuration against WireGuardNT, build the
`controller/pkg/tunneldevice` test binary with `GOOS=windows GOARCH=amd64 go test
-c`, then place it beside the signed DLL on a disposable Windows VM. Run as
administrator with an explicit opt-in:

```powershell
$env:CLDT_NATIVE_PEER_CONTINUITY = '1'
.\tunneldevice.test.exe '-test.run=TestNativePeerHandshakeContinuity' '-test.v'
```

This test creates two temporary adapters with random names, establishes a real
loopback handshake, and checks that adding/removing another peer preserves the
survivor's handshake and counters. It also checks unchanged updates and removal
of the final peer, and requires a subsequent authenticated keepalive to arrive
without a new handshake. It assigns no IP addresses or routes and closes both adapters
on exit. Verify that the logged adapter names are absent afterward. This checks
native peer-update behavior; cross-host traffic, route reconciliation, and live
worker rollout require separate integration tests.

`TestNativeApplyLifecycle` uses the same opt-in but exercises the production
backend on one owned adapter. It assigns `192.0.2.220/32`, manages a route to
`192.0.2.221/32`, and refuses to start if either exact route exists. It checks
initial apply, repair after deleting its own route, rejection of an address
change, and removal of the final peer and route. Run it only on the disposable
VM, and verify that its logged adapter and both test routes are absent afterward.

## First application on a Windows GPU VM

A warm GPU probe cannot qualify first-application startup. On a fresh CAPI VM,
retain Pod inventories from registration onward, check the WDDM build profile
with `windows_plugin_placement.py`, and submit the pinned GPU Job once with
`backoffLimit: 0`. Keep failed Job and native HCS evidence before diagnostic
controls change the startup order.
The GPU observer requires a new output path and refuses to overwrite an existing
observation, including a failed startup result.

Use the reusable submission command on a newly registered VM after enabling its
build-selected GPU plugin. It waits for plugin readiness and advertised GPU
capacity, retains each all-namespace Pod inventory, and refuses an existing
evidence directory. Supply the intended single-container Job with a pinned image,
`backoffLimit: 0`, `restartPolicy: Never`, and a Windows/hostname node selector.

```sh
python3 observe/windows_first_application.py \
  --api-server https://10.10.0.10:6443 --bastion clab-cldt-bastion \
  --node "$node_name" --node-uid "$node_uid" --job first-gpu-job.json \
  --evidence first-gpu-submission --timeout 900
```

A create timeout is ambiguous: inspect that exact Job name and retained evidence
before further action. The command never retries creation, deletes evidence, or
repairs the worker. A successful submission does not prove first-process or GPU
success; retain HCS events and use the GPU observer separately. Snapshot checks
also cannot exclude concurrent workload creation or previously deleted Pods.
Retain observations from registration onward. The
[native rejection result](../../../docs/validation/windows-first-application-submission-results.json)
confirms that an already-used GPU VM is rejected before creation. The
[fresh Windows 2025 GPU trial](../../../docs/validation/windows-first-gpu-gated-2025-results.json)
also exercised the wait from a Ready plugin without advertised capacity to a
qualified Node, then completed its first GPU workload. Removal and broader
repeatability remain separate checks.

For offline replay, validate the captured node and all-namespace Pod inventory:

```sh
python3 observe/windows_startup.py --node node-before-application.json \
  --pods pods-before-application.json --node-uid "$node_uid" --require-gpu \
  --output first-application-guard.json
```

The guard accepts only HostProcess containers already assigned to the node,
checking regular, init and ephemeral containers individually. An ordinary Pod
that never started still invalidates the clean inventory; record and investigate
it. The native regression fixture contains the misplaced Linux NFD Pod observed
on Windows. A snapshot cannot establish that a deleted Pod or an out-of-band
container never ran, so retain the earlier observations and native startup
records as well. Build-selected plugin readiness and GPU execution remain
separate checks. The GPU gate also requires positive, consistent WDDM capacity
and allocatable resources on the Node. In the refreshed Windows 2025 trial, the
plugin Pod became Ready before kubelet reported that resource; submitting then
produced an `Insufficient directx.microsoft.com/display` scheduling warning.
Wait for this gate before submitting the first GPU Job.

## Analyze Windows peer membership captures

Start `tunnelobserve` on each survivor and confirm live baseline samples before
creating the CAPI claim. Peer publication can precede guest bootstrap, so
starting the sampler after Node registration can miss the addition entirely.
Capture the target Machine UID and the SHA-256 of its decoded WireGuard public
key from `peer-public-key-<machine>` in the product peer Secret. Do not export
the whole Secret or native WireGuard configuration.

After collecting the complete JSONL capture, run:

```sh
python3 observe/windows_peers.py --input peers.jsonl --output peer-analysis.json \
  --require-addition --require-removal --expected-peer-sha256 "$peer_sha256"
```

The analyzer requires chronological samples from the same adapter, rejects
counter decreases and duplicate identities, and checks the requested target
transitions. It never treats an already-present peer as an observed addition.
Use a new report path for every analysis. The result describes sampled state;
it does not prove packet delivery, changes between samples, or causality. Keep
CAPI identity, cleanup and application-traffic evidence alongside it.

### Correlating a Windows container startup failure

Keep the failed Pod JSON and a private HCS event capture with `at`, `id`, and
`message` fields. Generate an identity-bound timeline without exporting raw
workload parameters or environment values:

```sh
python3 observe/windows_hcs.py --pod failed-pod.json --events hcs-events.json \
  --container render-nvenc --output startup-timeline.json
```

The output file must not exist. The analyzer includes only events carrying the
exact containerd container ID and distinguishes asynchronous operation-pending
results from process-start results. It does not infer a root cause, establish
capture completeness, or qualify GPU execution. Capture events before the first
ordinary application on a fresh VM; keep a later warm retry as separate evidence.

For a cold control with the same probe but no GPU resource assignment,
`windows_process.evaluate` binds the Node, Pod, owning Job and native HCS
container identities. It requires an HCS creation specification without assigned
devices and separates successful process creation, an explicit HCS error and an
unobserved result. A later probe exit because hardware is absent does not undo
successful process creation, and neither result qualifies GPU rendering.
First-workload eligibility still requires `windows_startup.evaluate` and retained
Pod inventories from registration onward. Native control fixtures exercise these
boundaries in `test_windows_process.py`.

When testing a refreshed workload base, pass `--image` with its explicit
SHA256-pinned registry reference to `windows_gpu.py`. The default remains the
historical image. `windows_process.evaluate(..., image=EXPECTED_DIGEST_REF)`
uses the same validation for a no-GPU control. A matching image reference alone
never establishes successful rendering or encoding.

## Linux explicit-empty peer withdrawal

The [isolated Linux VM canary](../../../docs/validation/linux-empty-peer-withdrawal-results.json)
verifies actual WireGuard peer and route deletion by the Linux reconciler for an
explicit `peers: []` document. Missing/null lists are not withdrawal instructions.
The test also reconstructs the interface from the empty durable input. It does
not reboot the VM or exercise Kubernetes Secret delivery. The site renderer now emits an explicit empty set after its last custom worker
is removed. This differs from the remote last-path protection: no available
cluster endpoint still means no new remote adoption document. Existing malformed, missing-field or null peer caches now fail reconciliation
without restoring bootstrap peers; only a missing cache permits first boot.
The native canary covers this rejection after withdrawal. Its adoption case uses
a controlled HTTP server with the real Kubernetes Go client and native kernel:
a current update repairs the cache, a simulated API outage retains the withdrawn
state, and re-addition preserves the WireGuard private key. The server checks
native peer count before accepting the applied-document acknowledgement. The
acknowledgement is conditional on the UID and resourceVersion read before apply.
A controlled race rejects the stale version, then accepts the next reconciliation;
isolated real-API checks also verify matching, stale-version and recreated-UID
patch behavior. Full worker Secret delivery, authorization, concurrent replacement
under traffic, receipt convergence and VM reboot remain separate gates.

`controller/cmd/dialer/native_empty_peers_test.go` skips ordinary unit runs.
Compile it as a static test executable, transfer it through the QEMU guest
agent, and run it **inside the VM** using a fresh network namespace:

```sh
unshare --net env CLDT_NATIVE_EMPTY_PEERS=1 ./dialer-native.test \
  -test.run='^TestNativeEmptyPeerWithdrawal$' -test.v
```

The test refuses the guest's host network namespace and any namespace containing
an interface other than loopback. It uses fresh private keys, a temporary
identity file, an isolated routing table and a test-owned WireGuard interface.
No cluster Secret or live mesh identity is needed. Capture the test exit status
and native coverage before removing the transferred executable.

The optional `adoption-api` native case starts real API server/etcd processes in
the same isolated guest namespace. Copy the installed envtest assets into a
private guest directory, set `KUBEBUILDER_ASSETS` to that directory, and select
`-test.run='^TestNativeEmptyPeerWithdrawal$/adoption-api'`. The API advertises
`10.253.253.1`, the canary interface address, because this namespace intentionally
has no default route. A certificate-authenticated test User receives only
`get`/`patch` access to the named adoption Secret. The canary verifies rejection
of an unrelated Secret read, cache repair, native withdrawal/re-addition and
actual API acknowledgements. The recorded run uses API server 1.35.0 and etcd
3.6.6; it does not qualify ServiceAccount token delivery or a full cluster reboot.
Retain process logs on startup failure and verify that no copied API executables
remain running before removing their files from the guest.

Set `CLDT_NATIVE_SERVICEACCOUNT=1` for the real-API case to use a TokenRequest-issued
ServiceAccount token instead of the certificate-authenticated test User. The
worker client retains only API CA trust and the bearer token, and the Role still
permits `get`/`patch` only for the named adoption Secret. This variant passed cache
repair, native withdrawal/re-addition, identity preservation and API acknowledgement
checks with unrelated Secret access denied. It does not mount a kubelet-projected
token volume or test token rotation; those require a deployed worker test.

The real-API case also exercises the site endpoint: mesh Secret worker fields
are added, removed, re-added and removed again through the actual site reconciler.
It checks native peer/route counts, private-key preservation and `SiteConverged`
against the API-assigned Node UID. Recreating the Node object under the same name
invalidates the old receipt; another reconciliation produces the current receipt.
The ServiceAccount receives named-Secret `get`/`patch` access and read-only Node
`get`/`list` permissions. This is API identity and kernel-state validation, not a
CAPI VM replacement, VM reboot or uninterrupted traffic test.

### Durable peer withdrawal across a full VM reboot

The [full reboot validation](../../../docs/validation/linux-empty-peer-reboot-results.json)
uses a disposable QEMU guest on a separate bridge with one physical NIC, no
default route, and serial guest-agent management. An enabled systemd unit runs
the candidate dialer with a generated identity, a loopback peer endpoint, and
an isolated routing table. This test requires a disposable guest because it
reboots the whole operating system.

The sequence is: apply one peer and host route; atomically write an explicit
`{"peers":[]}` cache beside the identity file; observe native withdrawal; request
`systemctl reboot`; require a different guest boot ID and an active service;
check zero peers and routes with the same identity. Finally, replace the cache
with the original public peer list and then an empty list, verifying native
re-addition and withdrawal without changing the private identity.

The cache filename is `<interface>.peers-cache.json`. Missing cache allows
bootstrap peers; an existing empty cache is authoritative. Preserve the identity
file and cache across reboot and never include private key contents in evidence.
Record native state, boot IDs, physical NICs, executable hashes and exit status,
then destroy only the owned VM and bridge. This validates durable host-service
behavior; CAPI replacement, deployed Secret delivery and survivor traffic remain
separate integration gates.

## Survivor traffic during lifecycle operations

`survivor.py` runs a concurrent 64-byte UDP echo lane for each directed pair of
existing ordinary pods. Targets are bound to explicit Node UIDs and the initial
Pod, container and Service identities. Every attempt is retained in a separate
lane JSONL file with timestamps, executor status and diagnostics. A failed
attempt stays failed even if later probes recover. `maxStartIntervalSeconds`
measures the sampling interval including executor time; `maxGapSeconds` measures
the idle interval between commands. Neither proves delivery between probes.

```sh
python3 observe/survivor.py \
  --api-server https://10.10.0.10:6443 --bastion clab-cldt-bastion \
  --namespace TEST_NAMESPACE \
  --target LINUX_POD=LINUX_NODE_UID --target WINDOWS_POD=WINDOWS_NODE_UID \
  --duration 1800 --max-gap 5 --output .state/survivors-NEW
```

Use a new evidence directory. Wait for `READY.json`, confirm the observer is
still running and its lane files are advancing successfully, then start the
CAPI operation. Record timezone-aware `startedAt` and `finishedAt` timestamps in
an operation JSON file. Allow probes to run after completion, create the owned
output directory's `STOP` file, and wait for the original observer to finish.
The duration is a maximum observation window; its expiration does not mean the
CAPI operation completed and is not a reason to recreate a VM.

```sh
python3 observe/survivor.py --output .state/survivors-NEW \
  --assess-window operation.json --window-output operation-traffic.json
```

The assessment requires successful samples before and after the operation in
every lane, stable identities, and the configured maximum sampling interval.
Keep lifecycle completion/cleanup evidence alongside it: successful traffic does
not establish that deletion or replacement finished. This observer uses the
Kubernetes exec path; a transport failure is a failed observation, not an exact
packet-loss count. Use VM out-of-band observation to distinguish that failure
from workload networking. Larger fragmented packets require their own matrix.

The [cp2 full reboot result](../../../docs/validation/survivor-cp2-reboot-results.json)
records a failed survivor observation despite node recovery. Its original helper
discarded stderr; the reusable helper now retains executor diagnostics. The
outage harness's existing convergence checks remain useful for settled state:
`git blame` attributes the deliberate post-outage waiting phase to `a356cad7`.
They do not supersede probes running throughout a lifecycle operation.

The [diagnostic repeat](../../../docs/validation/survivor-cp2-reboot-diagnostics-results.json)
retained WebSocket closures, missing Konnectivity agents and kubelet backend
timeouts. All original Node identities recovered, but the survivor observation
remained failed. These API exec failures require out-of-band VM observation for
workload-traffic qualification; retrying a failed probe must not erase its result.

### Out-of-band pod execution

Use `--oob-executors executors.json` when Kubernetes exec would share the fault
being tested. API reads still bind initial/final pod identities, but every traffic
command enters the existing container through the VM's native CRI endpoint.
The mapping keys are exact Node UIDs, not hostnames. Example entry for a QEMU VM:

```json
{
  "NODE_UID": {
    "os": "linux",
    "runtime": "cri",
    "command": ["docker", "exec", "clab-cldt-w1", "/cldt-guest", "exec", "25s", "0",
                "crictl", "--runtime-endpoint", "unix:///run/k0s/containerd.sock"]
  }
}
```

For EC2, the argv prefix is `awsnode -timeout 30s -work-dir PRIVATE_RUN
-instance-id INSTANCE_ID -- WINDOWS_CRICTL --runtime-endpoint
npipe:////./pipe/containerd-containerd`. Use `"os": "windows"` and the observed
runtime endpoint. The adapter appends `exec`, the complete observed container ID,
and the OS-specific curl executable. This mapping is trusted local command
configuration; it must not contain credentials. Renew the restricted harness
session before the observation window and allow for measured SSM command latency.
No Kubernetes exec call is used for a traffic attempt.

Verify that the CRI client exists before the run. If a test image lacks it, stage
a pinned, checksum-verified client in a new test-owned directory using the existing
SSM file-transfer path. Remove only that directory after the original observer
finishes. This is temporary harness tooling, not a dependency to install at GPU
worker startup. Windows `k0s ctr tasks exec` is not an accepted substitute here:
a native attempt started the image's original server command instead of curl and
failed on occupied ports. The adapter rejects `runtime: ctr`.

The [out-of-band cp2 reboot validation](../../../docs/validation/survivor-oob-cp2-reboot-results.json)
passed 539/539 sampled echoes across all six Linux/Windows 2022/Windows 2025
directions. Every lane bracketed the full guest reboot and recovery with the
same Node, Pod and container identities. The maximum start-to-start sampling
interval was 5.588 seconds, including SSM execution. Both temporary Windows CRI
clients were removed. This qualifies sampled traffic for that reboot; CAPI
addition/removal/replacement and fragmented UDP remain separate gates.

The [Linux CAPI lifecycle trial](../../../docs/validation/linux-capi-survivor-results.json)
uses this observer across actual worker addition and removal. All 1,455 sampled
echoes passed, with a maximum sampling interval of 6.473 seconds, alongside
102/102 worker matrix checks and native adoption. The original CAPA removal
command confirmed termination and product cleanup. An expired artifact URL
required repair before launch; the same claim then continued. The same-name replacement qualification below covers operation without an
in-window setup repair.

### Native sampler preflight failures

`survivor_native.Session.live_ready` requires advancing original processes,
unchanged Pod identities, and healthy traffic before submitting a lifecycle
mutation. If it fails before a readiness receipt exists, first verify that no
claim or candidate machine was created. Then call
`finish_preflight_failure(reason, dump)` to stop the original samplers once and
collect their terminal logs. `dump(source, argv)` must return binary-safe stdout
from the original container, as for `finish`.

The collector validates sample hashes, sequences, process identities, terminal
acknowledgements and final Pod identities. Its result remains failed and has no
qualified operation window. Do not manufacture a readiness receipt, restart the
samplers to clear earlier losses, or report this as a CAPI removal test. Remove
owned diagnostic files only after terminal collection. A session that already
became ready must use `finish` with its actual operation timestamps.

For investigation of an independent gate, such as GPU image cache reuse while
baseline networking is known to fail, select `diagnostic_reason` explicitly when
calling `Session.start`. The reason is recorded before any sampler starts.
Diagnostic readiness accepts recorded packet losses but still requires advancing
original processes, unchanged Pod/container identities, valid counters, and the
preset maximum sampling gap. It never clears a failed sample.

Call the usual `finish` with the actual operation window. Its diagnostic result
always has `qualificationEligible: false` and `ok: false`, even if every collected
echo succeeds. Workload, cache, joining and removal observations can be reported
individually; this mode cannot qualify the combined lifecycle scenario. Normal
sessions continue to reject unhealthy traffic before a mutation. Do not switch
an existing failed session into diagnostic mode or overwrite its evidence.

### Verifying staged gateway withdrawal

Start the read-only recorder while the original attachment is active and Ready,
before requesting removal:

```sh
python3 harness/e2e/observe/gateway_capture.py \
  --api-server https://ISOLATED_API:6443 --bastion VM_BASTION \
  --binding /private/original-withdrawal-binding.json \
  --output /private/new-withdrawal-observation --timeout 1200
```

The binding contains `machine`, `machineUID`, `node`, `nodeUID`, `meshUID`,
`secretUID` and `lease`. Read the original mesh UID from
`cloud-provisioning/cloud-provisioning-peers`, and the worker Secret UID from
`cloud-provisioning/NODE-tunnel-peers`. Use the committed attachment record's
`id/lease/digest` as the projection lease key. Do not infer identities from names
after deletion starts. The recorder creates a new private directory, rejects
same-name replacements, and preserves incomplete samples when its deadline
expires. It neither deletes machines nor retries a deletion. Only public state
hashes and identities are written; raw Secret data is not retained.

For staged gateway withdrawal, `gateway_withdrawal.evaluate(rows, secret_uid)`
checks the retiring worker's public-document transition separately from traffic
and provider cleanup. Rows contain observation time, mesh UID/resourceVersion,
projection presence and `retiringWorker`, worker Secret UID, desired/applied
document hashes, Machine presence/deletion state and Node readiness. Keep the
original worker Secret UID as an explicit input.

An old desired/applied hash pair can still match after staging begins. The
validator requires the changed document to be observed pending, then acknowledged
before global withdrawal. A following sample with the same mesh resourceVersion
brackets the acknowledgement with an unchanged staged projection. Missing
samples are insufficient evidence; do not substitute the sampler's simpler
self-consistency summary. The [native Windows 2025 result](../../../docs/validation/windows-capi-gpu-staged-withdrawal-2025-results.json)
supplies the regression fixture and keeps its failed UDP gate separate.
The [Windows 2022 recorder qualification](../../../docs/validation/gateway-withdrawal-capture-results.json)
adds a second observed transition and records the focused tests and coverage.
Its [complete lifecycle result](../../../docs/validation/windows-capi-staged-withdrawal-2022-results.json)
retains the failed native survivor echoes despite successful withdrawal and cleanup.

### Reusing a claim name after removal

For a delete-and-recreate replacement, keep survivor probes running from before
old-claim deletion until after replacement adoption and its worker matrix pass.
Wait for the original removal command to verify instance termination and product
cleanup before applying the same claim name with the same immutable template.
Keep the original and replacement row reports separate.

`replacement.py` compares native identity snapshots and the original removal
receipt. The snapshots contain `claim`, `claimUID`, `machineUID`,
`infrastructureUID`, `templateUID`, `nodeUID`, `instanceID`, `providerID`,
`bootstrapSecretUID`, `adoptionSecretUID`, `adoptionPodUID`, `publicKeySHA256`,
`desiredPeerSHA256`, `appliedPeerSHA256`, and `eniCount`. Use API-assigned UIDs,
the real EC2 binding, and the observed adoption receipt. Hash only the public
WireGuard key; never persist private identity or bootstrap Secret contents.

```sh
python3 observe/replacement.py --before original-identity.json \
  --after replacement-identity.json --removal original-removal.json \
  --output replacement-identity-check.json
```

The claim name and template UID must remain the same. The new claim, Machine,
infrastructure object, Node, instance, bootstrap/adoption Secrets, adoption Pod,
and public WireGuard identity must be fresh. Both generations must have one
NIC and matching desired/applied peer hashes. A hostname or private IP may be
reused; it is not an identity check. Separately assess the survivor observer
against the replacement window. This sequence tests claim deletion/recreation,
not automated Machine repair while retaining an existing claim object.

The [native replacement result](../../../docs/validation/linux-capi-replacement-results.json)
passed all 17 identity checks, both 102-check worker matrices, and all 1,966
sampled survivor echoes. The largest sampling interval was 7.983 seconds.
Both temporary instances, the retained template and the Windows diagnostic
clients were removed. Native snapshots show identical desired remote-peer
content across generations: unchanged endpoints can legitimately produce the
same hash. The new Secret UID and matching acknowledgement are required; a
changed content hash is not. The recorded snapshots drive the regression tests.
