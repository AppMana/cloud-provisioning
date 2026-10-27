# Building Linux images

From the repository root, compile the static payloads with the host Go toolchain:

```sh
bash controller/build-binaries.sh controller/_work/binaries all amd64 arm64
```

Assemble either `endpoint-controller` or `dialer` for the required platforms:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-context binaries=controller/_work/binaries \
  --target endpoint-controller -f controller/Dockerfile \
  -t registry.example/controller:VERSION --push .
```

For a local image, select one platform and use `--load` instead of `--push`.
Each image copies its architecture's payload onto the corresponding base image.
Assembly executes no target binaries and needs no QEMU or native ARM builder.
The output also includes `wg-dialer-linux-ARCH`, identical to the dialer image
payload, for first-boot provisioning. Native runtime qualification remains a
separate deployment requirement.
