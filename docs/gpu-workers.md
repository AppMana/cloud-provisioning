# GPU workers and reusable images

Use remote GPU workers for burst rendering, interactive Unity sessions, video
encoding or compute when the on-premises cluster should retain scheduling and
application state. For workers lasting 10–30 minutes, bake expensive setup into
an image and measure time to the first usable frame or job result.

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
