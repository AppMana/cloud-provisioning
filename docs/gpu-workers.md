# GPU workers and reusable images

Use remote GPU workers for burst rendering, interactive Unity sessions, video
encoding or compute when the on-premises cluster should retain scheduling and
application state. For workers lasting 10–30 minutes, bake expensive setup into
an image and measure time to the first usable frame or job result.

This document defines deployment choices and image acceptance criteria. The
[validated AWS matrix](validation/aws-k0s-calico.md) establishes Linux CPU worker
lifecycle and networking. It does **not** establish GPU, Windows Server 2022 or
Windows Server 2025 support. Promote those combinations only with corresponding
VM evidence; an example configuration is not a passing integration result.

## Choose by workload

| Scenario | Image and allocation requirements | Acceptance check |
| --- | --- | --- |
| Interactive Linux Unity/Vulkan streaming | Graphics-capable driver, Vulkan/EGL userspace, hardware encoder, sufficient VRAM; allocate through the device plugin | Render a real scene, encode its frames with NVENC, receive the stream across the tunnel |
| Windows graphics application | Licensed Windows image, matching container base, appropriate WDDM driver and a supported Windows device allocation mechanism | Execute the actual DirectX application and encoder inside the scheduled workload |
| Batch rendering/transcoding | Pinned renderer/codec libraries and recoverable output storage | Complete representative input; verify output and interruption recovery |
| CUDA inference or batch compute | Driver compatible with the application's CUDA libraries | Execute representative kernels and verify numerical output |

Start fractional-GPU trials with the smallest partition that can hold the real
workload. Test memory pressure, encoder availability and concurrent sessions;
advertised GPU presence or a successful `nvidia-smi` does not prove rendering
or streaming support. Do not equate time-slicing with dedicated VRAM isolation.

For Linux rendering images, include the application's Vulkan ICD discovery and
graphics/video libraries. Where the selected NVIDIA injection path uses
`NVIDIA_DRIVER_CAPABILITIES`, request `graphics,video,utility` and add `compute`
or `display` only when needed. Do not set `NVIDIA_VISIBLE_DEVICES=all` on ordinary
application pods to bypass allocation. Verify the actual libraries/devices
injected by the selected CDI configuration.
[NVIDIA driver capabilities](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html).

## Driver ownership and scheduling

Use taints to reserve GPU capacity. Use explicit component ownership to protect
baked software. GPU Operator's driver opt-out is
`nvidia.com/gpu.deploy.driver=false`; `driver.enabled: false` applies to the
entire Operator installation.
[NVIDIA installation configuration](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html).

For a mixed Linux cluster, register baked workers with that label **before**
GPU discovery can schedule driver operands. Applying it after registration can
race installation. The following illustrates the desired Node metadata; it is
not a claim field or an instruction to create a Node manually:

```yaml
metadata:
  labels:
    nvidia.com/gpu.deploy.driver: "false"
    cloud-provisioning.appmana.com/gpu-image: baked
spec:
  taints:
    - key: nvidia.com/gpu
      value: present
      effect: NoSchedule
```

Put registration labels/taints into the selected distribution's native worker
configuration. Verify the renderer supports them before admitting workers into
an existing Operator-managed cluster. A taint added later is not an installation
barrier. The toolkit DaemonSet explicitly tolerates the GPU taint and selects
`nvidia.com/gpu.deploy.container-toolkit=true`; a per-node value of `"false"`
excludes baked toolkit workers from that DaemonSet.
[Pinned toolkit DaemonSet](https://github.com/NVIDIA/gpu-operator/blob/v26.3.3/assets/state-container-toolkit/0500_daemonset.yaml).

An application Pod spec can request one allocated GPU as follows. Add this to
your pinned Linux application manifest, with its actual image and command:

```yaml
nodeSelector:
  kubernetes.io/os: linux
  cloud-provisioning.appmana.com/gpu-image: baked
tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  - key: cloud-provisioning.appmana.com/internet-facing
    operator: Exists
    effect: NoSchedule
containers:
  - name: application
    image: REPLACE_WITH_APPLICATION_IMAGE_AT_SHA256_DIGEST
    resources:
      limits:
        nvidia.com/gpu: 1
```

For GPU Operator's bundled Node Feature Discovery, restrict workers to Linux
and configure their tolerations separately from `daemonsets.tolerations`.
Without the tolerations, NFD does not discover
GPU hardware on workers with the product taint, and no GPU becomes allocatable:

```yaml
node-feature-discovery:
  worker:
    nodeSelector:
      kubernetes.io/os: linux
    tolerations:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
      - key: nvidia.com/gpu
        operator: Exists
        effect: NoSchedule
      - key: cloud-provisioning.appmana.com/internet-facing
        operator: Exists
        effect: NoSchedule
```

Apply the equivalent configuration to an independently managed NFD installation.
This behavior was observed with Operator v26.3.3 on the k0s 1.36 VM/AWS test site.
The operator's own `nodeSelector` does not constrain the bundled NFD worker
DaemonSet. Without the worker selector, its Linux image can be scheduled onto
Windows nodes and fail with `no match for platform in manifest`. The Windows
WDDM device plugin is configured separately.
The [complete Helm values example](../examples/gpu-operator-preinstalled-driver.yaml)
combines these selectors and tolerations with preinstalled drivers and
operator-managed Container Toolkit. It matches the validated mixed-OS
[NFD scheduling configuration](validation/nfd-mixed-os-results.json).

The Linux NVIDIA resource name is not a Windows allocation recipe. Ensure GPU
management operands tolerate the product's internet-facing taint too. Inspect
rendered DaemonSets instead of assuming an application's tolerations cover them.

## Two Linux runtime configurations

Choose one owner for each installed component. These are Helm values fragments
for a deliberately configured Operator installation, not commands to patch an
existing cluster globally. Pin the chart, operand images and CRDs together.

The harness defaults to **k0s v1.36.2+k0s.0**, matching the version declared in
AppMana's cluster configuration. This release bundles **containerd 2.3.2**.
Use a fresh VM site after changing this pin; site reuse checks reject an older
release. Historical k0s 1.34 evidence remains specific to that version.
[k0s release notes](https://github.com/k0sproject/k0s/releases/tag/v1.36.2%2Bk0s.0).

### Baked driver with Operator-managed NRI

```yaml
driver:
  enabled: false
toolkit:
  enabled: true
cdi:
  enabled: true
  nriPluginEnabled: true
```

CDI describes device injection; NRI provides runtime hooks. NVIDIA's NRI plugin
runs in the toolkit DaemonSet, so this configuration still deploys toolkit
components. It avoids the Operator rewriting containerd configuration. Do not
label these nodes `nvidia.com/gpu.deploy.container-toolkit=false`.

Check the **bundled runtime**, not just Kubernetes: NVIDIA lists containerd
1.7.30, 2.1.x, 2.2.x and 2.3.x, or CRI-O 1.34+, and requires CDI/NRI enabled.
Its documentation marks NRI as not GA. Enabling this mode can delete the
Operator's `nvidia` RuntimeClass; account for an existing GitOps owner before
changing modes. Validate the pinned chart's runtime-class behavior in isolation.
[NVIDIA CDI/NRI configuration](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/cdi.html).

### Baked driver and toolkit

```yaml
driver:
  enabled: false
toolkit:
  enabled: false
cdi:
  enabled: true
  nriPluginEnabled: false
```

This disables Operator installation of both components. The image must supply
working runtime integration, device discovery and CDI refresh, including the
management-container injection path. With NRI disabled, configure the NVIDIA
runtime handler and matching RuntimeClass needed by management operands. Test
the pinned chart's management pods against those baked paths. Disabling the
toolkit is not equivalent to installing NRI elsewhere.

For a mixed fleet, use per-node component opt-outs instead of globally disabling
installation that existing nodes need. Keep their current runtime ownership.
Do not disable the device plugin or monitoring simply to suppress an installer.

## DRY image recipes with cloud-specific outputs

Share recipes and version locks across clouds; produce native images per cloud,
OS, architecture and driver compatibility family. A single cloned system disk
is not the portability boundary. Use this composition for Packer build targets:

| Layer | Shared content | Target-specific parameters |
| --- | --- | --- |
| OS preparation | One Linux recipe and one Windows recipe, update/reboot sequencing, hardening, verification | Base image ID, OS build, kernel, architecture |
| Worker software | Version-locked distribution binaries, tunnel client, container runtime and service definitions | Distro/CNI combination and OS support |
| GPU software | Download verification, installation/reboot checks, rendering/encoding probes | GPU/vGPU family, vendor package, license source and runtime mode |
| Cloud adaptation | Common interface for bootstrap, diagnostics, generalization and capture | EC2Launch/SSM, GCE agents, Azure agents, or provider cloud-init/guest tooling |
| Publication | Manifest, provenance, image acceptance and retention rules | Regional AMI, GCE image, Azure Compute Gallery version, provider snapshot or QEMU qcow2 |

Implement OS steps once and call them from each Packer source. Keep provider
adapters small; they select inputs and native capture mechanisms. Keep distro
adapters separate from cloud adapters. An AWS k0s worker and a Vultr k0s worker
should reuse the worker recipe, while Windows has its own native service and
bootstrap implementation. Do not infer the OS from an image's display name.

Build on disposable VMs. A Kubernetes build Job may orchestrate Packer against
cloud builders or dedicated KVM hosts; a container alone cannot validate guest
boot, a Windows driver, or GPU passthrough. Local images retain serial QGA and
one physical NIC. Do not detach GPUs in use by the workstation.

Bake drivers, completed reboots, runtime integration, worker binaries and likely
workload layers. Preload Linux layers into the runtime namespace the distro
actually uses; Windows layers must match its container runtime and host/base
compatibility. At launch perform hardware discovery, device-spec refresh,
short-lived bootstrap retrieval, unique tunnel identity creation and joining.
Never snapshot a joined worker's credentials, tunnel key, machine identity or
device UUIDs as reusable configuration. Generalize Windows with Sysprep and
reset Linux cloud-init/machine identity before capture.

Give each output manifest its recipe commit, immutable base ID, artifact hashes,
OS/kernel/runtime/distro/CNI versions, driver source/version/license scope,
required GPU family, bootstrap format, region and image ID. Publish a candidate
only after a fresh VM launched from the captured image passes validation.
Use immutable image IDs in machine templates; promote by creating a new template
revision. This follows the separation of image building and controlled rollout
described in [Google's image management guidance](https://docs.cloud.google.com/compute/docs/images/image-management-best-practices).

The current [AWS preparation script](../harness/e2e/aws/image-prepare.sh) prepares
Linux secure-bootstrap prerequisites. It is not yet a shared GPU/Windows image
builder. The composition above is the implementation contract for extending it,
not a claim that all Packer targets already exist.

## Cloud and Windows boundaries

| Target | Image strategy | Qualification needed |
| --- | --- | --- |
| AWS | Private regional AMIs from provider-licensed bases; choose drivers for the actual instance family | CAPA secure bootstrap, one ENI, SSM, graphics and encoding; account for Spot interruptions |
| GCE | Same OS/worker recipes with GCE agents and licensed GPU package | Native image lifecycle, selected machine/GPU pairing and bootstrap adapter |
| Azure | Same recipes with Azure agents and versioned gallery outputs | Selected GPU SKU/driver licensing, image replication and bootstrap adapter |
| Discount VM providers | Same recipes only where the provider permits native snapshots and a compatible base | Real VM API, userdata fidelity, one NIC, snapshot reuse, GPU exposure and licenses |

AWS's GRID downloads are restricted to AMIs used in AWS with specified hardware.
Do not copy those drivers into a GCE, Azure or discount-provider image. AWS also
requires Sysprep-standardized custom Windows AMIs for GRID and specifies driver
ranges for fractional G6f/Gr6f instances.
[AWS GRID requirements](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/nvidia-GRID-driver.html).

For fractional L4 instances, pin the driver branch as well as the package hash.
AWS limits G6f/Gr6f to GRID 18.4–19.5. The
[initial Windows recipe](../images/aws/windows/nvidia-grid-19.5.json) therefore
uses GRID 19.5 / 582.53, rather than the bucket's changing `latest` object.
The [download policy](iam/windows-grid-download.json) grants the image-build
principal access to that one object; downloading a known key does not require
bucket listing or access to unrelated S3 buckets. Runtime workers using a
completed image do not need this download permission. IAM access does not grant
rights to use the package outside AWS's license scope.

The shared Windows preparation and verification scripts are in
[worker image recipes](../images/README.md). Their post-reboot check establishes
driver version, WDDM mode and device health; it does not establish license
activation, device allocation, DirectX rendering or encoding in containers.

For Windows device allocation, evaluate the
[Microsoft Windows GPU device plugin](https://github.com/Azure/Kubernetes-Windows-GPU-Device-Plugin).
Its WDDM resource is `directx.microsoft.com/display`; it assigns individual PCI
location paths and can mount driver runtime files. Non-DirectX APIs such as NVENC
still need workload validation for the selected driver. Pin the plugin image,
restrict its HostProcess DaemonSet to GPU test nodes, and request the resource
from an ordinary process-isolated application Pod. A HostProcess diagnostic does
not prove application-container GPU access. The
[native socket preflight](validation/windows-gpu-device-plugin-preflight.json)
found the upstream kubelet socket on the Windows k0s worker, so no socket-path
fork is currently justified.

### Windows GPU qualification

The tested AWS profile uses one physical NIC and an ordinary process-isolated
application Pod. Its smoke test renders 30 checked Direct3D 11 frames on NVIDIA
hardware, encodes them with NVENC and decodes 30 frames. Windows Server 2022 uses
the pinned upstream device plugin. Server 2025 requires the
[runtime-mount option](../providers/windows-gpu-device-plugin/README.md): explicit
CUDA DLL mounts combined with PCI assignment prevented container creation.
These results qualify the named observations, not a release or an untested image.

| Profile or gate | Native evidence | Qualification and remaining limits |
| --- | --- | --- |
| Windows 2022 GPU access and startup cache | [Earlier incomplete-cache trial](validation/windows-capi-gpu-startup-cache-2022-results.json), [corrected canonical-reference cache](validation/windows-gpu-startup-cache-canonical-2022-results.json) | A fresh VM from the corrected 18-reference image passed all 19 join checks and all 10 container cache-hit checks. Its first ordinary workload rendered, encoded and decoded 30 hardware frames, with NVIDIA license activation observed. Both Docker Hub names for the Windows 2022 device plugin are cached. The diagnostic trial's network and removal stages remain separate; this does not qualify uninterrupted lifecycle networking or startup-cost savings. |
| Windows 2025 complete startup cache and first GPU workload | [Fresh single-NIC GPU VM](validation/windows-capi-gpu-staged-withdrawal-2025-results.json) | All 19 join checks, all 16 cache references, the first ordinary 30-frame render/NVENC workload and native AWS driver-license status passed. The application had no earlier warm-up container. Repeated first-start reliability remains unqualified. |
| Staged gateway withdrawal | [Windows 2025 GPU](validation/windows-capi-gpu-staged-withdrawal-2025-results.json), [Windows 2022 GPU](validation/windows-capi-gpu-startup-cache-2022-results.json), [Windows 2022 CPU](validation/windows-capi-staged-withdrawal-2022-results.json) | Workers acknowledged changed fallback routes before global withdrawal and remained Ready through that transition. Original CAPI deletion completed without repair, preserving 13 Nodes and five surviving leases. |
| Uninterrupted lifecycle networking | [Windows 2022 complete-cache diagnostic](validation/windows-capi-gpu-startup-cache-canonical-2022-results.json), [earlier Windows 2022 trial](validation/windows-capi-gpu-startup-cache-2022-results.json), [Windows 2025 GPU](validation/windows-capi-gpu-staged-withdrawal-2025-results.json) | Not qualified. The complete-cache diagnostic lost 12/84,971 small UDP echoes and passed 119/120 matrix checks, retaining one missing 1,800-byte UDP response. Staged withdrawal and cleanup preserved all 13 original Ready Nodes and five gateway leases. Earlier trials' losses remain evidence; GPU success and cleanup do not supersede them. |

For the refreshed Windows 2025 application, use the shared
[Windows rebase builder](../images/probes/windows/README.md#refreshing-the-workload-base)
with pinned base inputs. It preserves the encoder/application layer and runtime
configuration and accepts either Windows release. Select the resulting workload
digest explicitly with the GPU observer's `--image` option. Require advertised
GPU capacity with the startup observer's `--require-gpu` gate before submitting
the workload.

Keep observation failures distinct from application failures. In the ungated
refreshed-base trial, the first API log read timed out; a later read verified the
same completed Pod. Submission preceded advertised GPU capacity and generated a
scheduling warning. That trial's removal required credential renewal and
controller restarts after the original command timed out. Those interventions
remain in its evidence and do not apply to the capacity-gated trial.

Preinstalled host drivers alone do not establish fast startup. Validate
pre-unpacked application, infrastructure and runtime pause layers through Sysprep
and a fresh CAPI launch before using an image for short GPU jobs. Digest-only
cache entries do not establish reuse of tag-based requests. Use the
[shared alias recipe](../images/README.md) to preserve the distro's observed
references without changing its defaults. The
[complete Windows 2025 cache](validation/windows-startup-cache-build-results.json)
contains 11 pinned references and five aliases, including pause. Its
[fresh-VM validation](validation/windows-capi-gpu-staged-withdrawal-2025-results.json)
verified the complete cache and ten container cache-hit observations.

Cache verification must check native Windows platform selection, unpacked
snapshots and build-service shutdown, not just registered image names. Earlier
[platform-unpacking failures](validation/windows-infrastructure-cache-build-results.json)
and the [missing-pause result](validation/windows-cache-reuse-results.json) remain
negative regression evidence. A later prepared image does not make those earlier
attempts pass. Windows 2022 requires its own complete-cache and fresh-GPU-worker
observations.

Network qualification is also separate: the GPU removal observation's matrix
included a fragmented UDP case with 9 of 10 responses. A successful render/encode
probe does not supersede that failure.

For a failed first application, preserve its Pod and container identities and
correlate HCS events before running another workload. The
[retained Windows 2025 startup timeline](validation/windows-gpu-start-error-results.json)
shows the silo starting before `CreateProcess` failed with RPC unavailable.
`0xC0370103` denotes an asynchronous operation pending in
[Microsoft's hcsshim implementation](https://github.com/microsoft/hcsshim/blob/main/internal/hcs/errors.go),
so that code alone does not establish a hypervisor failure. The cause of the
RPC failure remains unresolved; do not treat a warm retry as first-start proof.

Use the [first-application observation procedure](../harness/e2e/observe/README.md#first-application-on-a-windows-gpu-vm)
for fresh-image qualification. Require the correct build-selected GPU plugin
before submitting the application, retain the first failure, and test without
an earlier warm-up container. Keep the Linux GPU Operator's NFD workers off
Windows nodes. Historical [startup-order controls](validation/windows-cold-start-order-results.json)
encountered both a misplaced NFD Pod and a wrong live plugin profile, so those
controls cannot establish a cold-start fix.

Do not infer GPU isolation from a resource limit. AWS documents that Windows
containers may see all GPUs on a host despite requesting fewer devices. The
single-GPU tests cannot measure that boundary. Use dedicated workers for workloads
requiring isolation until the actual configuration has been qualified.
[AWS Windows GPU limitations](https://docs.aws.amazon.com/eks/latest/userguide/ml-eks-windows-optimized-ami.html).

Keep Windows 2022 and 2025 as separate image and workload-base targets. Microsoft
documents DirectX acceleration with process-isolated Windows containers and
WDDM 2.5+; that does not establish Vulkan or arbitrary GPU APIs inside them.
Run the real application on each target.
[Windows container GPU requirements](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/gpu-acceleration).

Publish shared recipes and redistributable metadata. Keep licensed Windows and
cloud-restricted driver images private unless their distribution terms permit
sharing. A cheaper advertised GPU is not eligible until the VM, image, license,
network and application requirements all pass.

Compare cost per successful session: billed startup plus useful execution plus
cleanup time, provider minimum billing, Windows/license fees, disk, snapshot,
public IP and egress, with failed Spot attempts included. Amortize image builds
over expected launches. Requery availability and prices before choosing a region;
an hourly price alone can misrank a ten-minute session.
The collectors model [AWS's per-second billing and 60-second minimum](https://aws.amazon.com/ec2/pricing/)
and [Vultr's hourly billing](https://docs.vultr.com/support/platform/billing/how-am-i-billed-for-my-servers).
Vultr continues billing stopped instances; destroy disposable workers to end
their instance charges.

Query AWS Spot and Vultr GPU VM base prices with the read-only collector:

```sh
python3 harness/e2e/pricing/quotes.py \
  --regions us-west-2,us-east-1,eu-north-1,eu-west-1 \
  --instance-types g6f.large,g6f.xlarge,g4dn.xlarge,g5.xlarge \
  --minutes 20 --output /tmp/gpu-quotes.json
```

Use a fresh output path. AWS requires read credentials for
`ec2:DescribeInstanceTypes` and `ec2:DescribeSpotPriceHistory`; Vultr's catalog is
public. The command preserves partial results and exits unsuccessfully if a
collector fails. It records excluded costs, quote time and the queried scope;
it neither provisions machines nor establishes capacity or Windows image
availability. Linux and Windows AWS prices remain separate. Fractional AWS GPUs
use logical GPU metadata even when physical count is zero. Vultr Windows rows
explicitly exclude the unquoted Windows license charge.

The [sample quotes](validation/gpu-price-results.json) retain the smallest AWS
and Vultr candidates from the named-region query. They are base prices, not a
global cheapest-provider claim. Check quotas as well as prices before building:
an enabled region can still have zero GPU launch quota.

## Image acceptance and integration evidence

The [Linux T4 results](validation/linux-gpu-results.json) cover a fresh AWS
CAPA worker using the prepared Ubuntu driver image, k0s 1.36.2, containerd 2.3.2
and bundled Calico 3.32. GPU Operator 26.3.3 with Toolkit 1.19.1 allocated the
GPU through CDI/NRI while leaving the baked driver unchanged. The scheduled
probe verified 4,096 offscreen Vulkan pixels and encoded 30 NVENC frames; the
network matrix passed 102 checks. Claim removal verified CAPA-owned instance
termination and cleanup of the Machine, Node, credentials and peer fields.
The workload image needs `libegl1` alongside
the Vulkan loader, consistent with the
[NVIDIA toolkit report](https://github.com/NVIDIA/nvidia-container-toolkit/issues/1952).
This qualifies the recorded smoke tests, not Unity streaming, fractional GPUs,
Windows GPU workers or the full dual-stack isolation matrix.

The [composed Linux GPU image](validation/linux-k0s-gpu-image-capture-results.json)
preserves the verified k0s executable layer while adding the shared Ubuntu
driver recipe. Its post-reboot host checks passed T4 enumeration and 30-frame
NVENC encoding. The [fresh composed-image CAPI trial](validation/linux-k0s-gpu-baked-capi-results.json)
verified the same baked k0s layer, a driver module predating instance launch,
one allocatable GPU, 4,096 Vulkan pixels and 30 NVENC frames. All 102 network
checks and CAPI removal passed. The original survivor streams retained six
failures in 45,041 echoes, so the combined diagnostic result remains unqualified.
The workload image was manually imported after boot; this is not startup-cache
or replacement qualification. Driver hashes stayed unchanged across the workload,
but the first hash measurement did not precede every initial Operator action.

For each OS/distro/CNI/provider/GPU/runtime combination:

1. Boot a fresh single-NIC VM from the captured image using the provider's native
   bootstrap consumer. Confirm versions and no driver/toolkit downloads or
   unplanned reboots. Hash installed drivers and runtime configuration before
   and after Operator reconciliation.
2. Record create, guest-ready, tunnel-ready, Node Ready, GPU allocatable,
   workload-ready, first-frame and first-encoded-frame timestamps. Include cold
   image pulls and separate warm-cache results.
3. Schedule the actual GPU workload; check rendered content, encoder output,
   resource allocation and unavailable/unallocated device behavior. A CUDA
   smoke test cannot substitute for a graphics test.
4. Exercise pod and Service traffic in IPv4 and IPv6, plus allowed and denied
   NetworkPolicy cases on the same and different nodes. Preserve source-identity
   observations so SNAT cannot hide a policy bypass.
5. Remove and re-add the worker through its claim. Verify instance termination,
   removed Node/peer/bootstrap state, a fresh identity on replacement and an
   unaffected survivor. Repeat the GPU workload after replacement.
6. Save sanitized observations and coverage, then derive unit fixtures from the
   behavior observed. Record unsupported combinations as unsupported, never as
   passed because a probe was skipped.

GPU Operator examples here apply to Linux. Windows allocation, forked CNI/proxy
behavior and dual-stack isolation require their own VM evidence. Keep those
results distinct from upstream-supported combinations and existing production
configuration.
