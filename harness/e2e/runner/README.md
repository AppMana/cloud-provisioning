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
reserves 40 GiB and 12 CPUs; leave enough resources for retained clusters.

```sh
repo=$(pwd -P)
state="$repo/harness/e2e/.state/isolated-kuberouter"
runner=cldt-isolated-kuberouter
umask 077
mkdir "$state"
mkdir "$state/context" "$state/work" "$state/docker-data" "$state/go-cache"
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

The [native capacity check](../../../docs/validation/nodeimage-capacity-results.json)
verifies the grown platform copy's disk, root partition and filesystem in a
single-NIC guest. Check growth logs as well as capacity: the wrapper may disable
cloud-init after initial provisioning, so its final `disabled` status alone
does not prove that first-boot setup succeeded.

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
cache is not part of a clean checkout and must not be required for a campaign.
Staging leaves the source chart and lock unchanged. See the
[clean-checkout regression](../../../docs/validation/chart-clean-checkout-results.json)
for validation and native retry status.

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
