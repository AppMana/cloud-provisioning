# Isolated VM harness runner

The lab uses fixed bridge names, subnets, container names and a process lock.
A different work directory or lab name does not isolate host routes. To keep
an existing lab while bringing up another distribution, use a separate Docker
daemon in a private network and PID namespace.

This runner hosts the network appliances and QEMU wrappers. Kubernetes nodes
remain KVM VMs with one Ethernet NIC each; guest commands use virtio-serial.
The privileged runner shares the host kernel and is intended for trusted test
code, not as a security boundary. It does not mount the host Docker socket or
publish a Docker TCP endpoint.

The runner image includes AWS CLI for real CAPA rows. Keep only derived,
short-lived CAPA and harness sessions under the private `/work` directory;
load setup credentials through `source-me.sh` on the setup host. Run AWS row
commands inside this runner so its bastion and VM inventory are selected.
Worker nodes do not inherit the runner's packages or credentials. See
[the AWS workflow](../../../docs/aws.md) for image, IAM and claim setup.

## Build and start

Run from the repository root on a Linux KVM host with Docker and containerlab
installed, plus a statically linked Helm binary. The harness copies Helm into
the Debian bastion, so Alpine's musl-linked package cannot be used. Choose a
fresh state directory and unused runner name. The example
limits the runner to 40 GiB and 12 CPUs; these limits do not reserve resources.
Keep only the fleet under test running. Shut down idle guests cleanly and retain
their disks and evidence before stopping their runner.

Size the runner for its own guests. Guest RAM is anonymous memory in the
runner's cgroup, and the page cache for their disks is charged to the same
limit. Leave at least 10 GiB above the sum of guest RAM, set `--memory-swap`
equal to `--memory`, and give at least one CPU per cluster VM. A seven-VM
2 GiB profile under 18 GiB and four CPUs spent a third of its CPU periods
throttled and hit its memory limit continuously. The guests' containerd then
missed crictl's two-second connect deadline and matrix rows failed without
measuring the network. The same site passed every row under 28 GiB and eight
CPUs. Check `cpu.stat` and `memory.events` in the runner's cgroup before
attributing a failure to the product.

Prefer a separate local SSD for state, selected with `LAB_STATE_ROOT`. On Btrfs,
create an empty directory for VM disks and set `chattr +C` before creating or
copying disks into it. This disables data checksums and compression for those
files; retain independent recovery copies. Never relocate a live VM disk or
Docker data directory. Keep captures and verdicts until their archive hashes
have been verified; do not use indiscriminate Docker or filesystem pruning.

```sh
repo=$(pwd -P)
state="${LAB_STATE_ROOT:-$repo/harness/e2e/.state}/isolated-kuberouter"
runner=cldt-isolated-kuberouter
umask 077
mkdir "$state"
mkdir "$state/context" "$state/work" "$state/docker-data" "$state/go-cache"
python3 harness/e2e/runner/host-check.py --state "$state" \
  --additional-memory-gib 40 || exit 1
cp harness/e2e/runner/Dockerfile "$state/context/Dockerfile"
install -m 0755 "$(command -v containerlab)" "$state/context/containerlab"
install -m 0755 "$(command -v helm)" "$state/context/helm"
containerlab_sha=$(sha256sum "$state/context/containerlab" | cut -d ' ' -f 1)
helm_sha=$(sha256sum "$state/context/helm" | cut -d ' ' -f 1)
docker build --build-arg "CONTAINERLAB_SHA256=$containerlab_sha" \
  --build-arg "HELM_SHA256=$helm_sha" \
  -t cldt/isolated-runner:local "$state/context"
platform=$(realpath harness/e2e/.state/platform/platform-node.qcow2)
docker run -d --privileged --name "$runner" \
  --label cloud-provisioning-isolated-runner=kuberouter \
  --memory 40g --cpus 12 \
  --mount "type=bind,source=$repo,target=/workspace,readonly" \
  --mount "type=bind,source=$state/work,target=/work" \
  --mount "type=bind,source=$state/docker-data,target=/var/lib/docker" \
  --mount "type=bind,source=$state/go-cache,target=/root/go" \
  --mount "type=bind,source=$platform,target=/work/platform-node.qcow2,readonly" \
  --mount type=bind,source=/lib/modules,target=/lib/modules,readonly \
  cldt/isolated-runner:local
docker exec "$runner" docker info
```

Wait for the last command to succeed before loading images. Record and compare
the two Docker daemon IDs with `docker info --format '{{.ID}}'`, using
`docker exec "$runner"` for the inner daemon. They must differ. Check that
`/dev/kvm` is available in the runner. Record the retained cluster's Node UIDs
and readiness and the host routes before deployment and compare them afterward.

## Run and inspect

For MicroK8s, preserve the bundled Calico profile and budget for the complete
seven-VM topology. The runner's cgroup limit does not reserve host RAM; check
available memory before starting another fleet. Keep the default 4 GiB per VM
and prepare root disks of at least 20 GiB, following the
[MicroK8s resource recommendation](https://canonical.com/microk8s/docs/getting-started).
Inspect the prepared image: the existing 16 GiB platform disk does not meet
that recommendation. `cmd/nodeimage` exports a standalone disk of at least
32 GiB by default. Use `-disk-gib` to select a minimum between 20 and 2048 GiB;
an already larger image is preserved. The export is grown after guest shutdown,
and cloud-init expands its partition and filesystem on first boot. This does
not change existing images or running guests. Build into a fresh work directory
and check both `qemu-img info` and the new guest's `lsblk`/`df` output before a
full run:

```sh
go -C harness/e2e run ./cmd/nodeimage -platform-only -disk-gib 32 \
  -work-dir .state/platform-32g
```

A successful single-VM installation checks prerequisites
only; it cannot replace the multi-node CAPI lifecycle matrix.

The host must already contain the VM wrapper and appliance images built or
selected by the normal harness setup. Enable pipeline failure detection so an
image-export failure cannot be hidden by the import command.

```sh
set -o pipefail
docker save cloud-provisioning/vm:single-nic kindest/node:v1.34.0 \
  | docker exec -i "$runner" docker load
docker exec "$runner" mkdir -p /work/coverage
docker exec -w /workspace/harness/e2e "$runner" \
  go build -cover -coverpkg=./... -o /work/lab ./cmd/lab
docker exec -w /workspace/harness/e2e -e GOCOVERDIR=/work/coverage "$runner" \
  /work/lab -rig vm -distro k0s -cni kube-router -product \
  -remotes remote1,remote2 -check \
  -placements control-plane,one-worker,two-workers,all-nodes \
  -repo-dir /workspace -work-dir /work -timeout 2h
```

Keep stdout and stderr in the private state directory. The harness also writes
`work/events.jsonl`. A successful deployment alone does not establish that
the network or lifecycle matrix passed. Runtime coverage is written when the
instrumented process exits; it covers the harness, not separately running
controllers or guest kernels.

The harness stages the product chart in its private work directory and runs
`helm dependency build` against the shipped `Chart.lock` before carrying it to
the bastion. Locked HTTP(S) repositories are registered in an isolated Helm
cache; OCI dependencies use Helm's registry support. The network-connected
runner must be able to fetch these dependencies. An ignored local `charts/`
cache is not part of a clean checkout and must not be required for a campaign. Staging leaves the source chart and lock unchanged.

Every command targeting this lab must traverse the outer runner:

```sh
docker exec "$runner" docker exec clab-cldt-bastion \
  kubectl --server=https://10.10.0.10:6443 get nodes
docker exec "$runner" docker exec clab-cldt-w1 \
  /cldt-guest exec 25s 0 ip -j link
docker exec -w /workspace/harness/e2e "$runner" \
  go tool covdata textfmt -i=/work/coverage -o=/work/native.cover
```

The inner names and IPs deliberately overlap the original lab. Omitting the
outer `docker exec "$runner"` targets the original host daemon instead.

## Memory during image import

Before a campaign on an existing runner, run the host check with no additional
memory budget. Preserve its JSON output with the private campaign evidence and
stop if it fails. A successful admission check is only a sample: record host
pressure and runner `memory.events` during the run as well. Do not classify a
failed continuity test as passing because the host was overloaded.

```sh
python3 harness/e2e/runner/host-check.py --state "$state" \
  > "$state/host-admission.json" || exit 1
```

Emergency host swap can protect unrelated processes from OOM, but is not extra
VM capacity. Keep guest RAM resident and leave host headroom; do not increase
runner limits to compensate for host memory pressure. Serialize full lab
campaigns, image imports, large builds and archive copies on shared hosts.

Budget for guest RAM, the nested Docker daemon, and image export/import buffers.
Import into one guest at a time and retain a single image export. Check available host memory before increasing a runner's limit.

After an OOM or image-import failure, inspect the runner's `memory.events`, QEMU
processes, and guest filesystem writes through the serial guest agent. A Ready
Node can still have a read-only filesystem. Preserve the disks and resource
identities during recovery, then verify new pod creation and the workload matrix.

## KEDA scaling

The [KEDA node-group scenario](keda/README.md) adds a queue and autoscaler to an
existing pooled VM lab. Its fixtures keep those services on a permanent site
worker and exercise the group's scale subresource through real queue metrics.

## Teardown

After the run exits, verify the runner name and ownership label, then destroy
only its inner lab:

```sh
docker inspect -f '{{index .Config.Labels "cloud-provisioning-isolated-runner"}}' "$runner"
docker exec -w /workspace/harness/e2e "$runner" \
  /work/lab -rig vm -work-dir /work -down
docker stop "$runner"
docker rm "$runner"
```

Retain the state directory for evidence. Do not remove an active daemon's
`docker-data` directory. Run another fresh profile in a new runner or explicitly
destroy the previous inner lab first.

## Match the wrapper to the pinned SDK

The SDK currently pins upstream commit `2c9fb9ef82fe3e72ba70307c9c9043ac5003f27f`
in both Go modules. Build the guest helper and
launcher from that same clean checkout:

```sh
bash harness/e2e/rig/vm/image/build.sh /path/to/labcontainers BASE_VM_IMAGE cloud-provisioning/vm:qualified-sdk
```

Import the resulting image into the isolated daemon, then tag it there as
`cloud-provisioning/vm:single-nic`. Keep retained labs' host tags unchanged.
The VM rig checks the image's source revision before modifying or destroying
any lab and resolves its image ID into a content-named local tag with pulls disabled.
Containerlab 0.79 interprets bare Docker image IDs as registry names.
Missing or stale provenance fails closed. The daemon's Go build metadata must
also match the SDK; a stale `LABCONTAINERS_LABD` or PATH binary is rejected.
Build it from the pinned module when needed:

```sh
CGO_ENABLED=0 GOWORK=off go -C harness/e2e build -o /absolute/path/labd github.com/appmana/labcontainers/cmd/labd
```

These checks follow the storage reliability harness's source/image pinning
pattern. Labels record build provenance; they are not cryptographic attestation.
The builder installs current source files rather than relabeling old wrappers.

## Assertion failures remain failures

Readiness waits may retry before measurement. Every completed matrix attempt
is evidence. `Matrix.OK()` requires every attempt to pass; `Converged()` reports
only whether the final observation recovered. A later green pass cannot change
a failed campaign to success. JSON retains prior attempts, and command exit
status follows the campaign result. Fault campaigns must separately assert
expected outages and recovery bounds; they cannot use blanket retry success.

UDP probe installation is serialized through completion so concurrent IPv4
and IPv6 checks cannot overwrite or remove each other's temporary executable.
