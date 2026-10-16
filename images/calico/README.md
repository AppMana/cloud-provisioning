# Windows Calico node image

`build-windows.py` appends the patched `calico-node.exe` to a digest-pinned upstream
Windows Calico node image. It runs on Linux with Python 3.11+ and `crane`; it does
not need a Windows Docker daemon or install packages on workers. The output is a
Docker image archive plus a build receipt containing the base digest, binary
checksum, image digest and archive checksum.

Build the executable using the [matching Calico source patch](../../providers/calico/README.md),
then supply its independently verified SHA-256. The builder rejects mutable base
tags, non-Windows images, non-amd64 executables and checksum mismatches. It checks
that the archive preserves all upstream base layers and startup configuration,
and that the appended Windows layer contains the expected executable. Original
licensing files and scripts remain in their base layers.
The appended layer includes its parent directory explicitly: Windows extracts
each layer into a separate staging tree, so a directory in a base layer does not
suffice for creating the replacement file during extraction.

```sh
python3 images/calico/build-windows.py \
  --base docker.io/calico/node-windows@sha256:9e9cd608c9ee009b3d814b15e95d29860770eecf5c914364339b09639d805086 \
  --binary /path/to/calico-node.exe \
  --sha256 VERIFIED_BINARY_SHA256 \
  --output /path/to/new-image-directory
```

This example pins the Calico 3.32.0 Windows Server 2022 image. Its platform
metadata is preserved. Compatibility with another Windows host version requires
native HostProcess testing; the builder does not rewrite platform metadata to
claim additional OS support. Worker VM licensing and GPU drivers remain separate
image layers.

Publish `image.tar` to the target cloud's private OCI registry. For the isolated
AWS harness, use [the repository-scoped ECR publisher](../../docs/aws.md#private-windows-publisher-image):

```sh
python3 harness/e2e/aws/publisher.py \
  --work-dir /path/to/private-aws-run \
  --archive /path/to/new-image-directory/image.tar \
  --component calico-node-windows \
  --api-server https://ISOLATED_API:6443
```

The helper records `calicoWindowsImage`, `calicoWindowsRepository`,
`calicoWindowsPullSecret` and `calicoWindowsPullSessionExpiresAt` in the private run
inventory. It creates the pull Secret in `kube-system`. These fields and the
repository are separate from the Windows tunnel publisher's state.

## Configure k0s

Merge the node image override into every controller's configuration. k0s requires
an image repository and a version containing a tag; include the immutable digest
in that version as well. The test publisher uses the digest's hexadecimal value
as its tag. Replace the uppercase values below with the recorded repository,
tag, digest and Secret name:

```yaml
spec:
  images:
    calico:
      windows:
        node:
          image: REGISTRY/REPOSITORY
          version: "TAG@sha256:DIGEST"
  network:
    provider: calico
    calico:
      mtu: 1370
      patches:
        - target:
            kind: DaemonSet
            name: calico-node-windows
            namespace: kube-system
          patch:
            type: StrategicMergePatch
            content: |
              spec:
                template:
                  spec:
                    imagePullSecrets:
                      - name: PULL_SECRET
```

Preserve other configuration and patches. Remove the temporary staged-binary
command patch if present; the derived image uses the distribution's startup
command. Set MTU for the actual path, following the
[k0s MTU procedure](../../docs/windows-gateway.md#configure-the-k0s-mtu). Validate
static controller configurations and restart one controller at a time, requiring
API recovery before proceeding. Wait for the Windows DaemonSet's new generation
to finish rolling out. CNI install images remain distribution defaults.

Verify the image digest in runtime container status and the executable hash inside
each Felix HostProcess container, then test fresh ordinary pods, MTU, traffic,
policy and restart recovery. A successful archive build or image push is not a
fresh CAPI worker qualification. Refresh expiring test pull credentials before
new pulls; production deployments need their cloud's credential rotation or
registry integration. Preload the same digest during VM image preparation when
startup latency matters, then verify it survives image generalization and boot.

## Native validation

Require loss-free concurrent UDP traffic and retain every failed attempt. A
passing UDP-only repeat does not qualify concurrent traffic or establish the
cause of earlier losses. Record exact binary and image hashes with each result.

Verify actual workload MTU after adapter restarts and restore any diagnostic
offload changes before measuring the candidate. The periodic scheduler rechecks
one active endpoint per five-second tick; eventual drift repair does not prove
uninterrupted packet delivery. The [trace correlator](../../harness/e2e/observe/README.md)
includes regression fixtures from both OS versions. Native traffic and lifecycle
qualification remain required before promotion.
