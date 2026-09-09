# Windows GPU application probe

This test requests one `directx.microsoft.com/display` device from the WDDM
plugin. It runs in an ordinary process-isolated application container. The
HostProcess plugin only discovers and allocates devices.

`render.cpp` creates a hardware Direct3D 11 device, requires an NVIDIA adapter,
rasterizes a green triangle on a blue render target, and checks all 4,096 pixels
on each of 30 readbacks. It writes those rendered frames to a raw BGRA file.
`run.ps1` scales those checked frames to 256×256 for the tested encoder's minimum
dimensions, encodes them using `h264_nvenc`, decodes the resulting video, and
requires exactly 30 frames. Device enumeration or a successful software renderer
cannot satisfy this test. A passing result does not qualify an entire Unity
application, network streaming, or GPU isolation between tenants.

Build the executable on the development/build host, outside the GPU worker:

```sh
x86_64-w64-mingw32-g++ -std=c++17 -O2 -Wall -Wextra \
  -static-libgcc -static-libstdc++ render.cpp -o render.exe \
  -ld3d11 -ldxgi -ld3dcompiler
```

Apply `device-plugin.yaml` for Server 2022 in the isolated test cluster. For
Server 2025 with the tested GRID 582.53 combination, build and publish the
[candidate runtime-mount plugin](../../../providers/windows-gpu-device-plugin/README.md),
then render `device-plugin-2025.yaml.tmpl` with its digest-pinned
`WINDOWS_GPU_PLUGIN_IMAGE` and `WINDOWS_GPU_PLUGIN_PULL_SECRET`. The template
enables the opt-in runtime-mount behavior and selects only build 10.0.26100.
The upstream manifest selects build 10.0.20348; do not run two plugins advertising
the same resource on one node. These selectors separate host builds, and do not
qualify arbitrary GPU drivers on those builds.

After labeling the test nodes and waiting for the plugin pods, verify live
placement before creating a GPU application. Hostname-specific overrides can
silently select the wrong plugin when a replacement gets a different name:

```sh
python3 harness/e2e/observe/windows_plugin_placement.py \
  --api-server https://TEST_API:6443 --bastion TEST_BASTION \
  --output NEW_PLACEMENT_RESULT.json
```

The check requires the build selectors above, the Windows 2025 runtime-mount
setting, and one Ready plugin per labeled Windows GPU node. GPU execution and
driver compatibility still require the application probe.

Label only the GPU nodes
being tested with `cloud-provisioning.appmana.com/gpu-test=true`. Wait for a real
allocatable device before creating a workload. Stage the probe through a
ConfigMap and apply `job.yaml`, selecting a specific hostname for each matrix row:

```sh
kubectl --context="$TEST_CONTEXT" -n cldt-windows-gpu create configmap windows-gpu-probe \
  --from-file=render.exe --from-file=run.ps1
```

Mark the ConfigMap immutable before observing the test. Use a new ConfigMap
name for changed probe bytes and record `configMapName`, `configMapUID`,
`binarySHA256` and `scriptSHA256` in the observer's identity file. Update the
Job's volume to that name. The script executes a hash-verified copy of the
binary in the container's writable layer because Windows process creation
failed when executing it directly through the projected ConfigMap links.

The test performs no driver or toolkit installation. ConfigMap staging is for
the test executable; it is not a qualified production image-cache recipe. Record
the Node UID, host build, container image digest, plugin digest, driver version,
Pod UID, ordinary-container specification, completion status and JSON output.
Keep failed attempts and their logs when correcting test prerequisites.

The pinned FFmpeg example image has a Windows Server 2022 base. Microsoft's
[compatibility table](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/version-compatibility)
lists process isolation for that base on Server 2022 and Server 2025 hosts.
Both combinations still require native execution. The upstream plugin and example
image digests are pinned independently; they are not claimed to have been built
from the current Azure repository HEAD. These are validation artifacts, not
production image recommendations.

## Refreshing the workload base

Keep the application layer fixed when testing whether a newer Windows base
changes startup behavior. `images/rebase_windows.py` supports both Windows 2022
and 2025 through digest-pinned inputs; it does not duplicate the encoder build.
For GPU workloads use the full Windows Server base. Microsoft's
[base-image guidance](https://learn.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/container-base-images)
identifies it as GPU-capable and notes that the Windows image is unavailable for
Server 2025.

```sh
python3 images/rebase_windows.py \
  --original "$ORIGINAL_PROBE_IMAGE_DIGEST" \
  --old-base "$EXACT_OLD_BASE_IMAGE_DIGEST" \
  --new-base "$NEW_WINDOWS_SERVER_IMAGE_DIGEST" \
  --output NEW_BUILD_DIRECTORY
```

All image arguments must identify single-platform Windows amd64 manifests by
SHA256, not mutable tags or multi-platform indexes. The declared old base must
match the original image's compressed layers, uncompressed layer identities,
history prefix and Windows version. The builder preserves the complete
application layers and runtime configuration, replaces base layers and platform
metadata, and verifies compressed layer bytes before writing the OCI index.
`layout/` is the resulting OCI image and `build.json` records its identity.
A failed build keeps its output directory; inspect the terminal error and use a
new directory for another attempt.

An OCI build is not a native GPU result. After publishing the candidate and
preloading its exact digest into the worker image, use a fresh VM for the first
ordinary GPU workload. Pass the same expected digest explicitly to
`harness/e2e/observe/windows_gpu.py --image IMAGE@sha256:DIGEST`; the observer does
not infer trust from the Pod's image. Its default remains the historical probe
image so older evidence is not silently reinterpreted. The no-GPU process
control accepts the corresponding `image=` argument. Both observers retain
probe-command and identity checks.

Publish separate workload bases with `harness/e2e/aws/publisher.py`, selecting
`--component windows-gpu-probe`, `--windows-version 2022` or `2025`, and the
resulting `--layout`.
The build check reads the digest-verified OCI config as well as the index's
platform descriptor. Release-specific records preserve both images in the same
owned repository. Select the matching record explicitly when preparing the
worker cache; sharing an encoder layer does not make a build-26100 base usable
on a build-20348 host.
