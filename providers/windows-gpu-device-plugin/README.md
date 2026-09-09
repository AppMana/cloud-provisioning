# Windows GPU runtime mounts

`runtime-mounts.patch` adds the opt-in environment variable
`WDDM_DEVICE_PLUGIN_SKIP_RUNTIME_MOUNTS=true` to the WDDM plugin. The default
continues mounting driver runtime files. The option omits only those file
mounts; allocation still validates requested device IDs and returns their PCI
location paths. It does not provide tenant isolation or change sharing limits.

Apply to [Azure's fork of the TensorWorks plugin](https://github.com/Azure/Kubernetes-Windows-GPU-Device-Plugin)
at `b21f59eb5dedf8ae9dc621dfecef61e54785b9e2`:

```sh
git apply /path/to/runtime-mounts.patch
cd plugins
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -o device-plugin-wddm.exe ./cmd/device-plugin-wddm
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -cover \
  -o plugin-tests.exe ./internal/plugin
```

Run `plugin-tests.exe -test.v -test.coverprofile=plugin.coverage` on a Windows
VM. These tests check default mounts, opt-in configuration, preservation of
the requested PCI device and rejection of unknown device IDs. They do not
substitute for a scheduled rendering and encoding test.

The native Server 2025 / containerd 2.3.2 / GRID 582.53 test on an AWS G6f L4
failed HCS container creation when combining PCI assignment with the explicit
`nvcuda.dll` mount. PCI assignment alone passed Direct3D rendering and NVENC;
mounts without device assignment also permitted container creation. A debugger
DLL mount alone worked. This isolates a specific interaction, not a general
failure of PCI device assignment. Do not enable the option for other driver,
host, or workload-image combinations without native validation.

After fetching full upstream history, `git blame` attributes the runtime mount
generation to `7ee9ff34` (2023-03-13). The native observation justifies making
that behavior optional for the affected combination; it does not justify
deleting it for Server 2022 or claiming Windows supersedes it universally.

Build a candidate container image with
[`images/windows-gpu-device-plugin/build-windows.py`](../../images/windows-gpu-device-plugin/build-windows.py).
Supply `--base` with the pinned upstream image digest, `--binary` with the
compiled executable, `--sha256` with its hash, and a new `--output` directory.
The shared Windows image builder preserves the base layers, discovery DLL and
startup configuration, validates the executable and resulting Windows layer,
and records the image digest and archive hash. Calico uses the same layer builder.

For the isolated AWS harness, `aws/publisher.py --component
windows-gpu-device-plugin` publishes that archive to a run-owned private ECR
repository with a repository-scoped expiring pull credential. The common
cleanup path includes this repository and verifies its ownership before deletion.
Fresh-worker rollout, replacement and GPU isolation checks remain release
requirements. Test artifacts are not released plugin images.
