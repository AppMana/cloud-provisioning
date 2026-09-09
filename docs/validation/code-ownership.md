# Code ownership and supersession

Use the following history references when deciding whether an older implementation
still has callers or unique assertions. A superseded execution path can still
contain useful tests; compare the active caller before removing it.

| History | Supersession / action |
| --- | --- |
| `271ee50a` | Source-aware companion routes supersede `egressViaRelay` and its destination-only test. Native MicroK8s uses a physical VXLAN source with a tunnel-address destination; retirement moves permission for that physical source to the relay. Isolated IPv4/IPv6 kernel lookups now test both physical and explicitly tunnel-bound sources. |
| Uncommitted VM and AWS reporting work | Shared `check.Matrix.Details` supersedes the private `cmd/lab` serializer and the AWS row's separate final-result loop. Both preserve per-probe errors and excluded checks. AWS rows now journal intermediate passes and stop when evidence cannot be written; older final-only AWS reports cannot establish first-attempt success. |
| Uncommitted bootstrap-completion work; CAPA v2.12.1 documentation | `CAPALinuxCommands` replaces the generic clean-exit check only for CAPA-owned Linux workers in `cmd/awsrow`. It accepts the exact documented initial secure-include warning with completed status and the success sentinel. Generic Linux/VM checks remain strict; Windows uses its separate identity-bound receipt. The [native CAPA completion report](capa-cloud-init-completion-results.json) records the observed response and the upstream checkout's shallow blame limit. |
| `ccad9701`, `b992d330` | Standalone Cilium/Flannel/kube-router registrations in `cmd/lab` are superseded by bundled-profile selection. Removed those unused imports; preserved historical implementations and their unique tests. |
| `fe9c72c`, `75f4296` | Infrastructure templates supersede request-shaped machine builders. Removed unused `NodeRequest`, the ignored `nodeRequest` call, and Docker `InfraMachine`/config fields; the only remaining builder caller was its own test. |
| `f6250ad`, `d08703c` | Guest SSH and multiplexing were the old VM execution transport. Retained for image preparation; lab execution now uses serial. Management-link tests now assert one NIC instead. |
| `18f698d` | Go rig interfaces replace repeated shell execution helpers. Unique shell assertions have not all been ported, so the shell harness has not been deleted wholesale. |
| `44af0e4`, `ef81563` | Infrastructure observation and real CAPI installation are retained and extended with provider-owned lifecycle. |
| `a356cad` | Product matches providerID when nodeRef is unavailable. The harness does not patch nodeRef. |
| `3ac724c`, `9efe4a2` | Product readiness gating and local Docker creation were superseded by the harness infrastructure controller. Removed the unused `containernet.Provider.Running` and private binding helper; moved binding precedence cases to actual provider reconciliation tests. |
| `34095a3` | Removed node-VIP allocation supersedes stale comments; documentation now describes current tunnel-address reservations. |
| `b98d1d3b` → `ef815636` | Real InstallCAPI supersedes the unused CRD-only downloader/filter. Removed those helpers and their filter-only tests; retained embedded-provider-contract tests. |
| `4b58725d`, `ff1b9b4a` | The focused VM scenario accepts one CIDR argument and builds the working-tree dialer. Its documentation follows that interface; obsolete vulnerable/fixed three-argument commands and experiment narrative are removed. |
| `5aa1bb03` | `bringup.PrepareHost` still supplies bridges, NAT and routes to the active VM rig. The isolated runner gives this code a private network namespace; it does not supersede the external network appliances or justify deleting their setup. |
| `ef815636`, `627262a6`, `07ba2406` | Convergence retries, bounded probe passes and cancellation remain active. The pass observer supplements the final returned matrix with intermediate evidence; it does not replace those timeout or acceptance rules. |
| `5b73a6c6` | The shared `dialerImage.pullPolicy` setting replaces hardcoded Linux DaemonSet pull policies. Production defaults to `IfNotPresent`; preloaded harness images use `Never`. Windows image handling remains separate. |
| `ef815636` | `provider.AdoptNodes` can stop on a verified terminal failure supplied by the machine's optional `rig.BootstrapHealth` interface. This replaces unconditional registration polling after known startup failure; it does not change CAPI provisioning status. |
| `dd77e815`, uncommitted profile selection | Native `k0s config create` confirms Kube-router as the default. Explicit default selection replaces first-profile selection, which incorrectly chose Calico. This restores the original builder default while retaining the newer explicit bundled Calico profile. |
| Uncommitted MicroK8s pattern | The bounded snap installer and worker-specific readiness check replace a single installation attempt and the control-plane status wait. VM and AWS userdata render the same worker pattern; fixed revision selection and native worker joining remain active. |

The active AWS worker path is `aws/claim.py` → CAPA → native distribution
bootstrap. Linux rows use `cmd/awsrow`; Windows lifecycle rows use
`aws/windows_lifecycle.py`, `aws/windows_join.py` and the ordinary-pod matrix.
They do not use the local infrastructure reconciler
to launch instances. `claim.Remover` and `observe.Capture` follow each CAPI
Machine's infrastructure reference; `Claimer.Delete` supplies the local provider
reconcile callback only for VM-backed claims. The AWS commands use `rig.Fleet`
to combine serial-managed site VMs with SSM-managed EC2 workers.

`harness/e2e/adoption` owns Linux adoption validation for both VM and AWS rows.
It replaces the AWS-command-local helper; no compatibility copy is retained.
The [adoption report](microk8s-adoption-fix-results.json) records readiness,
image-policy and applied-document checks independently from traffic results.
Bootstrap failure observation belongs to machine adapters: Ubuntu VMs use serial,
and the Linux AWS command adapter uses SSM. Windows does not dispatch Linux
cloud-init commands. The [bootstrap observer report](bootstrap-health-results.json)
describes the verified contract and its limits. `rig.BootstrapCompletion` adds
a separate completed-first-boot gate for Linux VM and EC2 test rows, requiring
cloud-init success and the CAPI marker. It supplements registration and
adoption; it does not change the infrastructure provider readiness contract.

`observe/tcpip_context.py` supplements `observe/pktmon.py` with native TCP/IP
interface candidates; it does not supersede packet correlation or
`observe/udp_capture.py` request/reply appearances. These are untracked additions,
so no committed blame entry establishes a replacement relationship. The
[three-host capture report](windows-tcpip-site-capture-results.json) records their
focused unit coverage separately from native Windows behavior and whole-project
coverage. Retain the packet collectors: TCP/IP Event 1215 has no packet identifier.

The Calico fork's `felix/dataplane/windows/vxlan_mgr.go` still selects the named
HNS network and reconciles its `RemoteSubnetRoute` policies. Its local checkout
is shallow: blame reaches the `eb1cf57823` history boundary, which does not prove
the code originated there. The kube-proxy fork's `b54cd3f1` adds the in-process
health watchdog in `hostprocess/calico/kube-proxy/start.ps1`; it restarts the
container after five consecutive failures. The
[native DR inspection](windows-dr-configuration-results.json) retains current
routes, VFP rules and plugin container start times. Neither code path is
superseded by the observer helpers, and neither inspection justifies removing
the External HNS overlay.

`images/linux/prepare_cache.py` replaces the private cache-build prototype's
fixed twelve-image list and pre-existing pause-cache requirement with explicit
recipe inputs and a fresh store. Keep `observe/linux_cache_native.py` and
`observe/linux_cache.py` as independent acceptance paths; the builder does not
replace their verification. The prototype and public helper are working-tree
additions without a committed supersession hash. Preserve the prototype's
recorded native evidence, but use the documented public helper for new builds.
The [complete public-helper build](linux-full-public-cache-builder-results.json)
covers all twelve image inputs through both pull and archive paths. Its new
fixture supplements the original captured-cache fixture: the latter still
describes the existing AMI and is not superseded by a scratch-store build.

## Retained machine capabilities

| Capability | Active caller and ownership | Cleanup decision |
| --- | --- | --- |
| Local VM guest execution | `rig/vm.Node.Pipe` uses `/cldt-guest` over the QEMU serial channel. | Keep the guest-agent path; integration rows must not acquire an extra management NIC. |
| Image-builder SSH | The same `Pipe` method selects SSH only when `builderSSH` is set. `TestImageBuilderCommandsShareAnSSHConnection` covers that branch. | Keep SSH for image preparation. The older transport's history (`f6250ad4`) does not make its remaining builder caller dead code. |
| AWS guest execution | The AWS rig uses SSM for Linux and Windows and carries the caller deadline into the command execution timeout. | Keep provider-specific execution behind the machine capability. CAPI provisioning alone does not supply a universal out-of-band console. |
| Mixed-fleet observation | `rig.Nodes` and `rig.Fleet` resolve site VMs and CAPA workers for measurement. | Preserve the distinction between observing a machine and owning its provisioning or teardown. |

`git blame` attributes the original `rig.Node` interface to `18f698d1` and
guest-interface naming to `480c494b`. The serial execution branch, mixed-fleet
interfaces and AWS/Windows helpers are working-tree changes; they have no
committed supersession hash yet. Do not attribute those additions to the older
commits or delete their predecessors solely from a coverage percentage.

`cmd/tunnelobserve` shares bounded sampling and JSON reporting between Linux
netlink and Windows driver readers. The Windows-specific reader remains active;
its former sampling loop moved to the shared command instead of being copied
into a second platform implementation. These observer files are uncommitted,
so no historical blame hash establishes that refactor. The
[Linux observer report](tunnelobserve-linux-results.json) distinguishes native
Linux execution, selected unit coverage, and Windows cross-compilation. The
separate [Windows observer report](tunnelobserve-windows-results.json) verifies
the shared command natively on Server 2022 and 2025.

The [module-wide short-test coverage report](go-short-coverage-staged-withdrawal-results.json)
covers the current controller and harness sources separately. Native Windows,
GPU and VM lifecycle observations are separate evidence; coverage of a selected
validator or attachment package must not be reported as whole-project coverage.

The [k0s 1.36 kube-router lifecycle row](k0s-1.36-kuberouter-lifecycle-results.json)
exercised 1,199 of 3,192 statements in the harness packages linked into the
instrumented `cmd/lab` binary (37.56%). Fresh site setup and failed runner
packaging attempts have separate counters. The observed-resource regression
uses both k0s 1.34 and 1.36 fixtures; the newer fixture supplements the older
version rather than making its assertions obsolete.

The [kube-router outage invocation](k0s-1.36-kuberouter-outage-results.json)
covered 1,198 of 3,202 linked harness statements (37.41%), with separate
`matrix-pass` evidence for retries. Its captured failed/recovered sequence is
replayed by `check/observed_reboot_test.go`; replaying outcomes does not model
the kernel or establish why recovery took that amount of time.

For runtime coverage of the AWS removal command, build an instrumented binary
from `harness/e2e` and point the lifecycle configuration's `awsremove` field to
that exact binary:

```sh
go build -cover -coverpkg=./... -o /private/awsremove ./cmd/awsremove
mkdir -m 700 /private/removal-coverage
GOCOVERDIR=/private/removal-coverage python3 aws/windows_lifecycle.py \
  --config /private/lifecycle.json --output /private/new-trial
go tool covdata textfmt -i=/private/removal-coverage \
  -o=/private/removal.cover
```

Keep any instrumentation smoke check in a separate directory. Require both
`covmeta.*` and `covcounters.*` from the actual removal process before claiming
runtime coverage. Setting `GOCOVERDIR` cannot instrument an existing ordinary
binary; the [Windows 2022 staged-removal result](windows-capi-staged-withdrawal-2022-results.json)
explicitly records that limitation. A trial that stops before removal will not
produce removal counters. Instrumented harness-command coverage does not cover
the separately running endpoint controller, Windows services or kernel behavior.
The [Windows 2022 GPU trial](windows-capi-gpu-startup-cache-2022-results.json)
collected both metadata and counters from its actual removal command: 201 of
1,416 linked instrumented statements executed. This limited runtime result
does not replace the module-wide short-test coverage report.

GPU image publication and workload selection have separate responsibilities:
`aws/publisher.py` verifies the OCI platform before publishing and records each
Windows release; `aws/windows_gpu_workload.py` binds the selected publication to
the native cache recipe. Neither replaces `observe/windows_first_application.py`,
which checks the fresh Node and submits the original workload once. These are
working-tree additions, so there is no committed supersession hash for them.

`observe/survivor_native.py` owns the original native sampler processes and
`observe/survivor_stream.py` independently validates their completed bytes.
`finish_preflight_failure` handles a baseline rejected before readiness;
`finish` still owns completed lifecycle windows. Neither path supersedes the
other. These helpers are uncommitted additions and have no historical blame
entry. The [preflight failure report](windows-native-preflight-cleanup-results.json)
records native cleanup and selected validator coverage separately from CAPI
runtime coverage, which cannot be collected when no claim was submitted.
The [diagnostic-mode checks](survivor-diagnostic-mode-results.json) replay that
native loss while preserving default readiness rejection. Diagnostic mode is
selected at sampler creation for independent observations and always produces
an unqualified combined result; it does not supersede normal lifecycle gating.
`cmd/udpprobe/stream.go` calls the shared finite probe with one attempt per
iteration, generating a fresh application nonce each time. Finite batches retain
one application nonce for the batch. Both modes remain useful; the stream mode
provides the continuous process and sequence evidence used in the
[Windows 2022 offload comparison](windows2022-ena-native-stream-results.json).
These probe files are also untracked additions, so Git cannot establish a
committed supersession relationship between them.

`aws/image_policy.py::linux_builder_parent` validates reusable Linux image
layers for `aws/bake.py start --base-image-id`. The explicit parent supplements
the original Ubuntu base path; it does not replace worker authorization or
promotion. The recipe remains OS-specific and cloud capture remains in the AWS
adapter. Both files are working-tree additions without committed blame history.

`images/runtime_payload.py` now owns the shared hash-pinned k0s runtime
extraction. Linux and Windows entrypoints select explicit component and file-mode
profiles; Windows keeps its existing CLI and receipt format. These are
working-tree additions, so there is no historical commit to blame for the
extraction move. [Linux runtime evidence](linux-runtime-staging-results.json)
compares the emitted components with a live QEMU worker; this helper does not
supersede either OS's cache-preparation and fresh-image acceptance gates.

`observe/linux_image_reuse.py` also validates the composed GPU image using the
verified child AMI and its unchanged parent k0s receipt. The
`linux-k0s-gpu-image-reuse.json` fixture preserves that native observation. Its
regression test rejects the CPU parent AMI even when executable hashes match;
this prevents binary reuse alone from qualifying the wrong machine image.
GPU execution and lifecycle coverage remain separate in the
[fresh GPU CAPI evidence](linux-k0s-gpu-baked-capi-results.json).

`images/linux/prepare_runtime.py` installs the shared extractor's Linux output
only after comparing it with the installed k0s payload. It preserves the mode,
size and timestamp contract used by k0s `pkg/assets/stage.go` to skip extraction.
It does not replace k0s's runtime supervisor or configuration generation. Like
the extractor, this helper is an uncommitted addition without a historical
supersession commit. `aws/bake.py` now clears an omitted recipe for a new builder
instead of silently reusing the prior build's verifier; the previous recipe's
hashes are retained in builder history.

`1f8c28f2` introduced the unconditional k0s installer download in the Linux join
pattern. Its working-tree version check preserves the installer as a fallback
and reuses a matching baked executable. `images/linux/prepare_k0s.py` supplies
that artifact independently of the provider and GPU driver recipe; it does not
replace distribution joining or runtime-cache preparation. The [native image-layer
result](linux-k0s-image-preparation-results.json) records this narrower boundary.

`harness/e2e/aws/bake.py` is an uncommitted addition, so `git blame HEAD`
cannot establish a historical owner for its capture path. Its bound SSM
verification receipt replaces unconditional command resubmission within that
helper. It does not replace the Windows image pipeline or the Linux recipe's
acceptance checks. The [capture-resume tests](linux-bake-capture-resume-results.json)
cover host interruption and command identity with mocked AWS responses; they do
not establish native capture success. The subsequent [native Linux capture](linux-k0s-image-capture-results.json)
exercises normal SSM verification and stopped-instance capture. Its initial
recipe-generation failure supplies the minimal heredoc fixture for
`aws/linux_recipe.py`; this parser check supersedes accepting unchecked shell
recipes, while retaining native image acceptance and fresh-worker gates.

`cmd/lab` defaults to `-rig vm`, and `cmd/campaign` explicitly selects that rig.
The legacy VM harness retains focused routing assertions and points to the Go
harness for the current matrix. The image publishing workflow builds artifacts;
it does not execute the VM campaign or establish distribution/CNI support.

`aws/image_policy.py` owns the shared available/private/account/run/OS checks
used by Linux authorization, Linux promotion and Windows authorization. It
replaces those helpers' duplicated ownership predicates. Additional image
selection in `aws/claim.py` and `cmd/awsrow` replaces the assumption that every
Linux worker must use the single run default; EC2 identity and physical-NIC
checks remain required. These AWS paths are uncommitted additions (`git blame
HEAD` has no entry), so no historical commit establishes this supersession.
The [baked-k0s CAPA evidence](linux-k0s-baked-capi-results.json) separates native
IAM readback, executable reuse, matrix/removal coverage and diagnostic survivor
traffic from the new unit checks. The older placement and replacement tests
remain necessary; this single add/remove run does not supersede them.

`observe/pktmon.py` retains flow/IP-ID/time-window correlation and now records
IPv4 traffic-class changes from native event bodies. The native-stream fixture
adds distinct TCP/IP and VFP drop cases; it does not replace the fragmented
reply or unfragmented reply fixtures. Ordinary-PCAP checksums cannot describe
bytes at an excluded drop event: retain the raw ETL and compare the native
drop-only export as shown in the [stream-capture evidence](windows-native-stream-drop-results.json).

`observe/linux_cache.py` owns validation of native Linux containerd inventory,
content completeness, unpacked snapshots and shutdown receipts. Its fixtures
come from the pause-cache experiment and the 23-reference GPU builder cache.
`observe/linux_cache_native.py` collects that evidence without pulling or
importing images. It provides an independent acceptance check for the private
cache-build prototype; it does not replace image preparation, capture, or fresh
worker cache validation. Both files are uncommitted additions, so `git blame
HEAD` has no historical entry for them. The
[builder-cache evidence](linux-gpu-builder-cache-results.json) reports validator
unit coverage separately from native execution; it does not claim coverage of
the native collector or qualify a cloned image.

`aws/linux_transfer_cleanup.py` handles explicitly identified historical SSM
transfer scripts. It requires successful AWS command receipts and independently
rejects paths still referenced by guest processes. Its native path fixture comes
from the Linux cache builder. This new helper does not replace cloud-init image
generalization or clean agent logs; capture still needs those separate checks.
It is also uncommitted, with no `git blame HEAD` entry.

The Linux bake helper's bound `captureCheck` adds acceptance for incremental
layers without changing the original `imageRecipe` verifier. Capture resumes
from the saved script and SSM identity; it rejects late additions and changed
saved content. This replaces manual mutation of the original acceptance hash
for that use case. The [cache capture evidence](linux-gpu-cache-capture-results.json)
separates the mocked interruption checks from the native successful capture.

`observe/linux_cache_reuse.py` adds fresh-worker content and snapshot acceptance
to the builder-only cache checks. Its fixture records 161 hashed blobs, the
descriptor closures for 12 Linux amd64 images, and native committed snapshot
metadata from the fresh CAPI worker before harness imports. It compares runtime
file identities with the capture receipt because k0s can preserve mtimes while
extracting files again. This uncommitted addition has no historical blame entry.
It does not replace `linux_image_reuse.py`'s AMI, node, NIC and containerd-server
checks, or the real GPU workload, network matrix and removal tests.
The [fresh cached-image trial](linux-gpu-cache-capi-results.json) reports validator
unit coverage separately from instrumented row/removal coverage. It also retains
the initial SSM inspection timeout and the successful checkpointed inspection;
the timeout is not evidence of missing cache content.

`observe/udp_capture.py` replaces the private dual-capture script's inline UDP
appearance counting with one parser for the documented tshark export. Its native
Windows2025 fixture includes a failed client exchange with a visible reply and
a request observed without a reply. These are distinct from native drop reasons,
which remain owned by `pktmon.py`. Both receivers' TCP/IP drop fixtures extend
that correlator without replacing the earlier VFP and fragmented-packet cases.
The new parser is uncommitted and has no historical `git blame HEAD` entry.
The [dual receiver result](windows-dual-receiver-drop-results.json) records the
captures, exact artifact hashes, fixture coverage and cleanup separately from
network qualification.

`observe/windows_vfp_trace.ps1` and `observe/vfp_context.py` add bounded native
VFP collection and IPv4 flow decoding. They supplement `pktmon.py`'s packet-drop
analysis and `tcpip_context.py`'s interface context; neither older observer is
superseded. These files are uncommitted and have no historical blame entry.
The [native Windows collector result](windows-vfp-collector-results.json) binds
the script and unchanged XML fixtures to captures on both Windows builds. Its
37/37 statement coverage applies only to the new Python decoder, not the
PowerShell collector or networking implementation. Forwarding-fallback events
must not become synthetic packet-drop fixtures.
The collector's default raw-ETL return supersedes its initial automatic full XML
export: the [paired capture](windows-vfp-paired-results.json) showed that export
latency can destroy the retained overlap with a companion circular capture.
Explicit XML export now has an event limit, verified natively on both Windows
versions. The earlier changed-traffic-class VFP fixture remains valid; the new
unchanged-class drop adds a distinct observation rather than replacing it.

`observe/windows_vfp_events.ps1` adds bounded export from a retained ETL. Its XPath
selection and exact timestamp check replace the initially attempted hashtable
query, which missed known records on both tested Windows versions. The
[decapsulation comparison](windows-vfp-decap-context-results.json) records that
native discrepancy and the successful bounded-query checks. `vfp_context.py`
keeps protocol-252 encapsulation selectors separate from transport ports and
adds exact-port inbound-flow selection. Its 53/53 covered statements are observer
coverage; the native Ignore/Pop fixtures do not simulate or qualify a classifier
fix. These additions are uncommitted and have no historical blame entry.

The collector's `IncludeControl` mask now includes both native `Control` and
`ControlValidation`. The latter owns IOCTL/object-processing events 700–706 on
both tested Windows builds. This corrects the earlier control-only mask; it
does not invalidate its packet observations or provide missing IOCTL evidence
retrospectively. The [native keyword validation](windows-vfp-control-keywords-results.json)
binds the correction to provider metadata and actual VM captures. The existing
packet, interface and flow observers remain complementary.

The retained-ETL exporter also accepts an optional bounded event-ID list.
Native flow/control captures showed that wider windows can exhaust even a
10,000-event query, while selecting new-flow event 600 retained the relevant
records within the bound. Selection occurs in XPath and is checked again
before export; the existing exact-time, provider and query-limit checks remain.
This supplements the unfiltered context query rather than replacing it.

The VFP collector's optional focused profile selects only flow lifetime and
control-validation keywords. It supplements the broader profiles when event
volume threatens retained context; the native smoke test does not establish
an improvement in retention. For HNS dynamic records, native `tracerpt`
supersedes the unsuccessful `Get-WinEvent` decoding path in the
[HNS collector validation](windows-hns-collector-results.json). This is a
provider-specific observation, not a replacement for the validated VFP XPath
exporter. These collector additions remain uncommitted with no historical
blame entry.

`observe/hns_context.py` supplements the packet and VFP-flow decoders with
ordered HNS layer-operation pairing. Its [native fixtures](../../harness/e2e/observe/testdata/windows-hns.md)
cover activity-ID reuse and incomplete capture boundaries. The 52/52 statement
coverage applies to this observer only. Six combined drop/flow/layer cases
preserve the observed failure window without simulating a fix. This new source
is uncommitted and has no historical blame entry.

In the reviewed Calico fork, commit
`bb26dec4f21c1e12cfd5cd34ad290c77871b3b16` introduced the explicit
`External`-network type guard in
`cni-plugin/pkg/dataplane/windows/hns_types.go`: cleanup removes the L2Bridge
placeholder while preserving Overlay networks. That cleanup does not supersede
an Overlay network merely because it shares the name. The
[layer-gap report](windows-hns-layer-gap-results.json) records this source review
separately from native behavior; running-binary equivalence is not inferred.

The endpoint controller's Calico address writer dates to `34095a32`; site-node
physical-address pinning was added by `3b8176a8`. Commit `2f1015f1` changed its
comparison to tolerate Calico's interface prefix length. That avoids mask-only
churn, but cannot prevent Calico's address monitor from selecting another IP.
The existing uncommitted k0s managed-address patch disables that competing
monitor through the distribution's manifest configuration. It complements
the per-node writer. The [w2 rollout observation](calico-w2-address-rollout-results.json)
found an old `IP=autodetect` pod beneath the updated `OnDelete` template;
replacing that pod applied the configuration. Its focused configuration tests
cover 12/12 statements in `calico_config.go`, not the whole k0s package or
native packet delivery. Neither address writer nor the native Windows
transport ownership journal is superseded by this rollout.

`hns_context.network_space_removals` complements the layer observer with
native evidence of deferred dataplane cleanup after API deletion. Shared
provider and field validation still rejects undecoded HNS records rather than
returning a misleading empty result. The expanded observer has 80/80 statements
covered by the 205-test observer suite; its new fixture preserves 15 native
event elements. This coverage concerns decoding and context, not Windows
network convergence. The source remains uncommitted and has no historical
blame entry. Immediate-removal evidence must not replace a test of the settled
state merely because `Get-HnsNetwork` no longer returns the deleted object.

The [running bootstrap binding](windows-calico-bootstrap-source-results.json)
ties the Windows CNI image to the staged MTU build's `node-service.ps1` by
byte hash. That script creates an Overlay placeholder in its VXLAN branch.
The reviewed fork's guarded L2Bridge handling is attributed to `b84aaa62d1a`,
but differs from the running script. Its existence does not establish that it
supersedes the active VXLAN bootstrap. Preserve the backend distinction when
preparing a replacement image and validating fresh workers.

The UDP probe's fixed-cadence foreground mode complements its serial and
background modes; it does not supersede their acceptance checks. It reuses the
exact fresh-socket echo implementation and shared input validation, while
bounding outstanding requests and retaining missed send slots as failures.
Race-enabled package tests pass; the new scheduler has 35/37 statements covered
(the whole probe package reports 59.3%). Native Windows 2022/2025 and Linux VM
results are recorded in the [cadence comparison](windows-fixed-cadence-results.json).
These probe sources are uncommitted, so there is no historical blame attribution
for this addition. The serial receive timeout remains useful evidence about the
application probe, but cannot by itself establish an equally long network outage.

The MicroK8s worker and kubelet checks have native single-VM fixtures for snap
revision 9063. Race-enabled regression cases cover 24/87 package statements;
native prerequisite calls cover 18/87. The
[prerequisite report](microk8s-prerequisite-results.json) keeps installation,
selected-method coverage and full lifecycle qualification distinct. These
MicroK8s sources are untracked in the current checkout, so `git blame` has no
committed attribution for them. Their distro-specific snap and local API
checks supplement the common machine interfaces; they do not supersede the
k0s, image-builder SSH or serial guest-agent paths.

The updated MicroK8s kubelet invariant replaces substring matching with parsed
current-context resolution and exact local endpoint comparison. Its
[regression report](microk8s-kubeconfig-results.json) covers 31/94 package
statements with the race detector. The earlier native prerequisite report
remains evidence for the earlier source revision; it does not establish native
validation of the new parser. No broader distribution implementation is
superseded by this correction.

The [two-VM MicroK8s check](microk8s-native-join-results.json) now exercises that
active-context validator on both a native control plane and joined worker.
Credential-free projections preserve their actual context and cluster names.
They supplement the earlier server-line fixture and synthetic rejection cases.
The native join and basic Node deletion evidence does not replace the CAPI
provisioning, tunnel or survivor-traffic gates.

The chart-copy path attributed to `d5509933` assumed generated dependency
archives were already present. The dependency declaration from `387972fc`
remains active, even when its condition disables deployment: Helm still checks
the package dependencies. `install.stageChart` supersedes the cache assumption
by building the shipped lock in a private copy before the existing bastion
transfer. Real Helm tests use a loopback repository and an empty dependency
cache, including stale-lock rejection and unchanged source files. The
[native failure and regression report](chart-clean-checkout-results.json)
keeps those checks separate from the full campaign retry.

`cmd/nodeimage` now grows the stopped standalone export before copying it out.
This extends the flatten/export sequence attributed to `e14ddd6b`; it does not
replace image preparation or resize retained guests. The new sizing helper
has 29/29 statements covered by race-enabled tests, including real qcow2
growth and preservation of a larger disk. Whole-command coverage is 29/98.
The [native capacity report](nodeimage-capacity-results.json) verifies that a
fresh single-NIC guest expands its root partition and filesystem on the grown
platform copy. It does not claim a complete builder or Kubernetes lifecycle run.

The claim endpoint's fixed port from `ef815636` is superseded by the shared
`kube.Client.ServingPort` setting. MicroK8s serves on 16443; imported CAPI
association through the Kubernetes Service can succeed despite an incorrect
infrastructure endpoint. The [endpoint regression report](microk8s-endpoint-results.json)
records that distinction and the default-port regression case. The association
comment from `16b6d074` is also obsolete: imported mode supplies a workload
connection and waits for real CAPI association, while tunnel reachability
remains a separate check.

The placement walk attributed to `9c14b60f` now invokes the shared remote
lifecycle routine at each requested placement. This supersedes running the
cycles only before the placement walk. Lifecycle events include the placement,
and the existing membership/device invariant guards removal and rejoin. The
[per-placement campaign report](microk8s-placement-lifecycle-results.json)
distinguishes this expanded scope from earlier placement-only matrices.
The infrastructure reconciliation shortcut from `ef815636` now follows
endpoint publication, so a retained Ready cluster can receive a corrected
distribution port without rewriting its status.

The physical kubelet address policy from `e102974b` remains in the distribution
patterns that select it. MicroK8s now selects its allocated WireGuard address
while sharing provider identity rendering. This supersedes applying the old
multi-NIC address workaround to MicroK8s AWS workers, whose private NIC address
is unreachable from the site. The out-of-band workload checks from `dc083738`
remain useful for isolating pod-network failures; their assumption that kubelet
access is outside qualification is superseded by `check.KubeletAccess` and the
AWS row's separate exec/logs report. The VM lab records the same gate at
lifecycle and placement boundaries and the final matrix. Each control plane
is checked directly.
See the [native address and management report](microk8s-kubelet-address-results.json).

The uncommitted Windows `tunneldevice.Native` implementation now treats its
cached address solely as an immutable-identity guard. Native address read-back
and repair supersede using that cache as evidence of a configured address.
The code has no committed blame attribution yet. DAD configuration follows
`golang.zx2c4.com/wireguard/windows@v1.0.1/tunnel/addressconfig.go`; the
[Windows address report](windows-generation-address-results.json) records the
native failure, repair coverage and remaining overlap limitation. Existing
route repair and peer update behavior remain active and are not superseded.

The opt-in `stable_owner_windows_test.go` experiment is also uncommitted and has
no historical blame entry. Its native transport-route guard follows observed
Windows endpoint routes in [the stable-owner report](windows-stable-owner-results.json).
It supersedes the experiment's assumption that a remote instance address is
a direct underlay path; it does not supersede production acknowledgment or
single-owner checks. No legacy production functionality is removed by this test.

The stable-owner experiment's independent sampler supersedes sampling in the
route-mutation loop: traffic continues during native route and adapter calls.
The [packet report](windows-stable-owner-packet-results.json) preserves the
initial failed run and records the replacement sampler's IPv4/IPv6 evidence.
This test-only change does not supersede any production handover implementation.

The uncommitted `generation_address_windows_test.go` acceptance experiment has
been retired. It expected an ownership model that `Native.Apply` deliberately
rejects. Its exact source remains in the original native-address evidence
archive, with SHA-256 recorded in the [isolation report](windows-stable-owner-isolation-results.json).
`TestNativeApplyLifecycle` retains the duplicate-owner rejection contract;
`TestNativeStableOwnerPackets` tests the replacement model with a separate
address owner. Neither file has a historical committed blame entry, so no
commit attribution is inferred. This removes an obsolete failing design test,
not production ownership validation.

`pkg/handover` is an uncommitted receipt-binding primitive. It does not supersede
`pkg/tunnelhost` delivery receipts or `pkg/peerpublisher` acknowledgment writes.
The production safeguards introduced by `ea1c7a1` (native application) and
`ca6e69b` (remote acknowledged egress view) remain required. The new layer binds
phase attempts, direction and complete membership before eventual integration;
its [validation report](handover-round-results.json) explicitly excludes native
phase execution and durable controller history.
