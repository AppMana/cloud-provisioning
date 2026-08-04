#!/usr/bin/env bash
# Live end-to-end: one ProvisionedNodeClaim against a single-node kind
# cluster becomes a real second node, entirely through the product's
# own abstractions, with no shim CRDs and no bespoke cluster rig:
#
#   claim -> docker MachineProvisioner renders a DockerMachine -> CAPD
#   launches the "cloud" node container -> kubeadm ClusterJoinProvider's
#   minted token + the rendered kubeadm-worker cloud-config join it to
#   the kind control plane -> the node registers with the cloud-worker
#   label/taint -> both dialer DaemonSets land -> a real WireGuard
#   tunnel (cldt*) comes up between the two containers -> deleting the
#   claim terminates the node container by ownerRef cascade.
#
# kind's control plane IS kubeadm and CAPD exists precisely to fulfill
# machines as local containers: kind is the cluster, the providers do
# the rest.
set -euo pipefail
cd "$(dirname "$0")"
REPO_DIR="$(cd ../.. && pwd)"
CLUSTER=cldt-live
# The release namespace, and the names the chart derives from it.
NS=cloud-provisioning
PEER_SECRET="$NS-peers"
LOCAL_DS="$NS-dialer"
REMOTE_DS="$NS-dialer-remote"
CONTROLLER="$NS-endpoint-controller"
NODE_IMAGE=kindest/node:v1.34.0
# Pinned: the harness asserts against Calico's own resources, so the
# version it installs has to be the version those assertions describe.
CALICO_MANIFEST="${CALICO_MANIFEST:-https://raw.githubusercontent.com/projectcalico/calico/v3.29.1/manifests/calico.yaml}"
# Which network to run, and therefore what the accept list should
# contain. A native network needs each peer's own pod blocks; an
# encapsulated one addresses its packets to the node and needs none.
CNI="${CNI:-calico}"
case "$CNI" in
  calico)       WANT_ENCAP=native ;;
  calico-vxlan) WANT_ENCAP=encapsulated ;;
  cilium)       WANT_ENCAP=native ;;
  *) echo "unknown CNI $CNI (calico, calico-vxlan, cilium)" >&2; exit 2 ;;
esac
LOG_DIR="${LOG_DIR:-$(mktemp -d /tmp/cldt-live-e2e.XXXXXX)}"

CONTROLLER_PID=""
cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1: leaving the cluster and controller running for inspection (kubeconfig: $KUBECONFIG)"
    return
  fi
  [ -n "$CONTROLLER_PID" ] && kill "$CONTROLLER_PID" 2>/dev/null || true
  # CAPD node containers are owned by the claim cascade; the final
  # assertion already deleted them on a green run. This catches red
  # runs so nothing leaks.
  docker ps -aq --filter "label=io.x-k8s.kind.cluster=${CLUSTER}" --filter "name=public-worker" | xargs -r docker rm -f >/dev/null 2>&1 || true
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  # Capture cluster state before the EXIT trap tears everything down.
  {
    kubectl -n "$NS" get pods -o wide
    kubectl -n "$NS" describe pods
    kubectl -n "$NS" logs daemonset/$LOCAL_DS --tail=30
    kubectl -n "$NS" logs daemonset/$REMOTE_DS --tail=30
    kubectl get machines,dockermachines -A -o wide
    kubectl get cluster "$CLUSTER" -o yaml
    kubectl get machine public-worker -o yaml
    kubectl get dockermachine public-worker -o yaml
    kubectl get provisionednodeclaim -A -o yaml
    kubectl get events -A --sort-by=.lastTimestamp | tail -25
    kubectl -n capd-system logs deployment/capd-controller-manager --tail=30
    kubectl -n capi-system logs deployment/capi-controller-manager --tail=30
    # The tunnel state on both sides. Without this a ping failure is
    # undiagnosable after teardown. Uses host tools in each netns (the
    # node image has no wg).
    for c in "${CLUSTER}-control-plane" "${NODE_CONTAINER:-}"; do
      [ -n "$c" ] || continue
      echo "===== netns state: $c ====="
      node_netns "$c" wg show all 2>&1
      node_netns "$c" ip -br addr 2>&1
      node_netns "$c" ip route show 2>&1
      node_netns "$c" ip -6 route show 2>&1
    done
    echo "===== peer secret ====="
    kubectl -n "$NS" get secret $PEER_SECRET -o json |
      python3 -c 'import base64,json,sys; d=json.load(sys.stdin).get("data",{}); [print(k,"=",base64.b64decode(v).decode(errors="replace")) for k,v in sorted(d.items()) if "private" not in k]'
  } >"$LOG_DIR/postmortem.log" 2>&1 || true
  tail -60 "$LOG_DIR/postmortem.log" >&2 || true
  [ -f "$LOG_DIR/controller.log" ] && tail -40 "$LOG_DIR/controller.log" >&2
  exit 1
}

until_ok() {
  local timeout=$1; shift
  local waited=0
  until "$@" >/dev/null 2>&1; do
    sleep 3; waited=$((waited + 3))
    [ "$waited" -lt "$timeout" ] || return 1
  done
}

# Run a command inside a node container's network namespace using the
# host's tools. The node image ships neither wg nor ping, so
# `docker exec ... wg show` does not just fail: piped into awk it makes
# the assertion vacuous, because the pipeline's status is awk's and awk
# succeeds on empty input. Every tunnel assertion below therefore
# enters the namespace instead, where the host's own wg/ping/ip apply
# to the container's interfaces.
node_netns() {
  local container="$1"; shift
  local pid
  pid=$(docker inspect -f '{{.State.Pid}}' "$container") || return 1
  sudo nsenter -t "$pid" -n "$@"
}

# WAN reachability, asserted at each phase on each node. Bringing up a
# tunnel does not cost a node its ordinary path off-box. Checking it
# once at the end would not distinguish "never broke" from "broke and
# recovered", so it is checked before and after every step that touches
# networking. Uses the host's ping inside the node's netns (the node
# image has none) and a DNS-independent target, so a resolver blip
# can't masquerade as a routing failure.
WAN_TARGET="${WAN_TARGET:-1.1.1.1}"
assert_wan() {
  local phase="$1"; shift
  local container
  for container in "$@"; do
    docker inspect "$container" >/dev/null 2>&1 || continue
    if ! node_netns "$container" ping -c1 -W3 "$WAN_TARGET" >/dev/null 2>&1; then
      node_netns "$container" ip route show >&2 || true
      fail "WAN UNREACHABLE from $container at phase: $phase (default route above)"
    fi
  done
  echo "  wan ok [$phase]: $*"
}

handshake_established() {
  local hs
  hs=$(node_netns "${CLUSTER}-worker" wg show "$IFACE" latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')
  [ -n "$hs" ] && [ "$hs" -gt 0 ] 2>/dev/null
}

echo "--- build: controller image, dialer image, dialer release binary ---"
docker build -q --target dialer -t cldt-dialer:e2e -f "$REPO_DIR/controller/Dockerfile" "$REPO_DIR" >/dev/null
docker build -q --target endpoint-controller -t cldt-controller:e2e -f "$REPO_DIR/controller/Dockerfile" "$REPO_DIR" >/dev/null
mkdir -p "$LOG_DIR/binaries"
( cd "$REPO_DIR/controller" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$LOG_DIR/binaries/wg-dialer-linux-amd64" ./cmd/dialer )
BIN_SHA=$(sha256sum "$LOG_DIR/binaries/wg-dialer-linux-amd64" | awk '{print $1}')

echo "--- kind cluster: a control plane and two workers, docker socket mounted so CAPD can manage sibling containers ---"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
# No --wait: without a CNI the node cannot become Ready, and Calico
# is installed below.
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config - >/dev/null <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  # No default CNI: Calico is installed below instead. Calico is what
  # this project has to work with, since it keeps its own IP pools
  # rather than letting the controller manager allocate per-node
  # blocks, and it peers over the tunnel.
  disableDefaultCNI: true
  podSubnet: "10.244.0.0/16"
  serviceSubnet: "10.96.0.0/12"
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
  - role: worker
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
  - role: worker
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
EOF
export KUBECONFIG="$LOG_DIR/kubeconfig"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
kind load docker-image cldt-dialer:e2e cldt-controller:e2e --name "$CLUSTER" >/dev/null

echo "--- install the network ($CNI) ---"
if [ "$CNI" = "cilium" ]; then
  helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
  helm repo update >/dev/null 2>&1
  helm install cilium cilium/cilium --namespace kube-system \
    --set routingMode=native \
    --set ipv4NativeRoutingCIDR=10.244.0.0/16 \
    --set autoDirectNodeRoutes=true \
    --set ipam.mode=kubernetes \
    --set k8sServiceHost="$CP_IP" --set k8sServicePort=6443 \
    --wait --timeout 6m >"$LOG_DIR/cni-install.log" 2>&1 \
    || fail "installing Cilium failed (see $LOG_DIR/cni-install.log)"
else
  kubectl apply -f "$CALICO_MANIFEST" >"$LOG_DIR/cni-install.log" 2>&1 \
    || fail "installing Calico failed (see $LOG_DIR/cni-install.log)"
  # The stock manifest ships ipipMode Always, so Calico encapsulates
  # out of the box. Set the mode this run is for, which is what the
  # model then has to read back.
  until_ok 240 sh -c "kubectl get ippools.crd.projectcalico.org default-ipv4-ippool" \
    || fail "Calico never created its default IP pool"
  if [ "$CNI" = "calico-vxlan" ]; then
    POOL_PATCH='{"spec":{"vxlanMode":"Always","ipipMode":"Never"}}'
  else
    POOL_PATCH='{"spec":{"vxlanMode":"Never","ipipMode":"Never"}}'
  fi
  kubectl patch ippools.crd.projectcalico.org default-ipv4-ippool --type=merge \
    -p "$POOL_PATCH" >/dev/null || fail "could not set the pool's encapsulation"
  # calico-node programs its routes from the pool it saw at startup, so
  # a pool changed afterwards leaves routes for the old encapsulation
  # behind. Restart it and let it program the mode this run is for.
  kubectl -n kube-system rollout restart daemonset/calico-node >/dev/null
  kubectl -n kube-system rollout status daemonset/calico-node --timeout=5m >/dev/null \
    || fail "calico-node did not come back after the pool change"
fi
# Calico's own IP pool is what the controller reads its pod ranges
# from, so it has to exist before the controller starts.
until_ok 240 sh -c "kubectl wait --for=condition=Ready node --all --timeout=10s" \
  || fail "the nodes never became Ready under $CNI"
CP_NODE=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')
# The tunnel endpoints on this side: both workers. Control planes are
# left out by the selector's default posture, which is asserted below.
ENDPOINT_A=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}')
ENDPOINT_B=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[1].metadata.name}')
[ -n "$ENDPOINT_B" ] || fail "expected two worker nodes to terminate tunnels"
CP_IP=$(docker inspect "${CLUSTER}-control-plane" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
[ -n "$CP_IP" ] || fail "could not resolve the control plane's kind network address"
echo "  control plane: $CP_NODE @ $CP_IP"
assert_wan "baseline, before anything touches networking" "${CLUSTER}-control-plane"

# Binary delivery without a download server. The docker provider mounts
# $LOG_DIR/binaries read-only into the node container (extra-mounts in
# its provider config) and the userdata curls a file:// URL: fully
# deterministic, nothing to start, nothing to leak, nothing to go
# stale; the pinned-sha verification is unchanged.
BIN_URL="file:///opt/dialer-dist/wg-dialer-linux-amd64"

echo "--- install CAPI core + CAPD (clusterctl) ---"
clusterctl init --infrastructure docker --wait-providers >"$LOG_DIR/clusterctl-init.log" 2>&1 \
  || fail "clusterctl init failed (see $LOG_DIR/clusterctl-init.log)"
SERVED=$(kubectl get crd dockermachines.infrastructure.cluster.x-k8s.io -o jsonpath='{range .spec.versions[?(@.served)]}{.name}{" "}{end}')
echo "  DockerMachine served versions: $SERVED"
echo "$SERVED" | grep -qw v1beta2 || fail "installed CAPD does not serve infrastructure v1beta2: adjust pkg/join/docker's GVK to: $SERVED"

# CAPD (not this project) needs the target cluster's kubeconfig, at an
# address reachable from inside the cluster's pods, in a Secret its
# clustercache can see. It filters by the cluster-name label, so an
# unlabelled Secret is invisible to it ("not found" despite existing).
sed "s#server: https://127.0.0.1:[0-9]*#server: https://$CP_IP:6443#" "$KUBECONFIG" > "$LOG_DIR/incluster-kubeconfig"
kubectl create secret generic "$CLUSTER-kubeconfig" -n "$NS" --type=cluster.x-k8s.io/secret \
  --from-file=value="$LOG_DIR/incluster-kubeconfig" --dry-run=client -o yaml > "$LOG_DIR/kubeconfig-secret.yaml"

echo "--- INSTALL THE CHART: and nothing else ---"
# Everything the product owns comes from `helm install`: the claim CRD,
# RBAC, the controller, the provider config, and the externally-managed
# Cluster pair. Nothing here creates those by hand.
#
# Assembling them with kubectl instead would let the tests pass against
# an install path that does not exist, for example a chart missing the
# Cluster pair and the provider config. Anything the chart fails to
# render fails here.
kubectl create namespace "$NS" >/dev/null
kubectl apply -n "$NS" -f "$LOG_DIR/kubeconfig-secret.yaml" >/dev/null
kubectl label secret "$CLUSTER-kubeconfig" -n "$NS" "cluster.x-k8s.io/cluster-name=$CLUSTER" >/dev/null
cat > "$LOG_DIR/values.yaml" <<EOF
image: {repository: cldt-controller, tag: e2e, pullPolicy: Never}
dialerImage: {repository: cldt-dialer, tag: e2e}
joinProvider: kubeadm
cluster:
  apiAddress: "https://$CP_IP:6443"
  apiVIP: "$CP_IP"
  podCIDRs: 10.244.0.0/16
  serviceCIDRs: 10.96.0.0/12
  nodeVIP4Prefix: 10.199.0.
  nodeVIP6Prefix: "fd99::"
tunnel: {endpoints: "kubernetes.io/os=linux"}
dialerBinary: {amd64: {url: "$BIN_URL", sha256: "$BIN_SHA"}}
targetCluster: {enabled: true, name: "$CLUSTER", infrastructureKind: DockerCluster}
provider:
  docker:
    nodeImage: "$NODE_IMAGE"
    extraMounts: "$LOG_DIR/binaries:/opt/dialer-dist"
    preloadImages: cldt-dialer:e2e
EOF
helm install cloud-provisioning "$REPO_DIR/charts/cloud-provisioning" \
  --namespace "$NS" -f "$LOG_DIR/values.yaml" \
  --wait --timeout 5m >"$LOG_DIR/helm-install.log" 2>&1 \
  || fail "helm install failed (see $LOG_DIR/helm-install.log)"

# Everything this harness touches from here lives in the release
# namespace, the same as a real install.
kubectl config set-context --current --namespace="$NS" >/dev/null

echo "--- mesh precondition: both workers allocate and publish, the control plane does not ---"
until_ok 60 sh -c "kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.node-tunnel-address-$ENDPOINT_A}' | grep -q ." \
  || fail "no tunnel address was allocated for worker $ENDPOINT_A"
until_ok 120 sh -c "kubectl -n "$NS" get pods -l app=$LOCAL_DS --no-headers | grep -q Running" \
  || fail "on-prem dialer pod never Running (image load / scheduling problem)"
until_ok 120 sh -c "kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.node-public-key-$ENDPOINT_A}' | grep -q ." \
  || fail "$ENDPOINT_A never self-published its public key"
until_ok 120 sh -c "kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.node-public-key-$ENDPOINT_B}' | grep -q ." \
  || fail "$ENDPOINT_B never self-published its public key"
kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath="{.data.node-tunnel-address-$CP_NODE}" | grep -q . \
  && fail "the control plane was allocated a tunnel address; it must be excluded unless named"
echo "  dialer pod Running, key published"
assert_wan "on-prem dialer running (interface created, key published)" "${CLUSTER}-control-plane"

echo "--- the Cluster API objects, applied the way a user applies them ---"
# These describe the cluster and its cloud account rather than the
# provisioner, so they are not part of the chart. Same pair as
# examples/aws.yaml, with the docker provider's cluster kind.
kubectl apply -f - <<EOF >/dev/null
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $CLUSTER
  namespace: $NS
  annotations: {cluster.x-k8s.io/managed-by: external}
spec:
  controlPlaneEndpoint: {host: "$CP_IP", port: 6443}
  infrastructureRef: {apiGroup: infrastructure.cluster.x-k8s.io, kind: DockerCluster, name: $CLUSTER}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: DockerCluster
metadata:
  name: $CLUSTER
  namespace: $NS
  annotations: {cluster.x-k8s.io/managed-by: external}
spec:
  controlPlaneEndpoint: {host: "$CP_IP", port: 6443}
EOF

echo "--- what the controller inferred from the cluster ---"
# Nothing was configured, so every one of these came from the cluster
# itself. Asserted against what the cluster actually holds rather than
# against the values the harness would have passed.
INFERRED=$(kubectl -n "$NS" logs deployment/$CONTROLLER 2>/dev/null | grep -m1 "^cluster: ")
[ -n "$INFERRED" ] || fail "the controller logged no discovered configuration"
echo "  $INFERRED"
case "$INFERRED" in
  *"network=calico/$WANT_ENCAP"*) ;;
  *) fail "the network was not read as calico/$WANT_ENCAP: $INFERRED" ;;
esac
# The API address has to be a real control-plane endpoint.
case "$INFERRED" in
  *"api=https://$CP_IP:6443"*) ;;
  *) fail "discovered API address is not the control plane's $CP_IP: $INFERRED" ;;
esac
echo "  network and API address both read from the cluster"

echo "--- a machine template, and ONE claim naming it ---"
# A DockerMachineTemplate is an ordinary Cluster API machine template,
# the same shape as an AWSMachineTemplate. The machine created from it
# is its kind without the suffix, which is the provider contract this
# relies on rather than anything provider-specific.
kubectl apply -f - <<EOF >/dev/null
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: DockerMachineTemplate
metadata: {name: public-worker, namespace: cloud-provisioning}
spec:
  template:
    spec:
      customImage: $NODE_IMAGE
      extraMounts:
        - hostPath: $LOG_DIR/binaries
          containerPath: /opt/dialer-dist
          readOnly: true
      preLoadImages:
        - cldt-dialer:e2e
EOF
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata: {name: public-worker, namespace: cloud-provisioning}
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: DockerMachineTemplate
    name: public-worker
EOF

echo "--- the chart's Cluster pair is reported provisioned BY THE CONTROLLER ---"
# Externally-managed means Cluster API's own controllers stand down, so
# nothing upstream ever sets this status and Machines block on it
# forever. It used to be a `kubectl patch` here, which is exactly the
# kind of step a real install would never know to run.
until_ok 90 sh -c "kubectl -n "$NS" get cluster $CLUSTER -o jsonpath='{.status.initialization.controlPlaneInitialized}' | grep -q true" \
  || fail "the controller never reported the externally-managed Cluster provisioned"

echo "--- CAPD launches the node container and the bootstrap joins it ---"
until_ok 90 kubectl get dockermachine public-worker || fail "DockerMachine never created"
until_ok 60 kubectl get secret public-worker-bootstrap || fail "bootstrap Secret never rendered"
until_ok 240 sh -c "docker ps --format '{{.Names}}' | grep -q 'public-worker'" \
  || fail "CAPD never launched the node container"
NODE_CONTAINER=$(docker ps --format '{{.Names}}' | grep public-worker | head -1)
echo "  node container: $NODE_CONTAINER"
assert_wan "remote node container up, bootstrap running" "$NODE_CONTAINER" "${CLUSTER}-control-plane"

until_ok 300 sh -c "kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker --no-headers | grep -q ." \
  || { docker logs "$NODE_CONTAINER" 2>&1 | tail -20 >&2; docker exec "$NODE_CONTAINER" journalctl -u wg-dialer --no-pager 2>/dev/null | tail -10 >&2 || true; fail "the node never joined with the cloud-worker role label"; }
CLOUD_NODE=$(kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker -o jsonpath='{.items[0].metadata.name}')
kubectl get node "$CLOUD_NODE" -o jsonpath='{.spec.taints}' | grep -q "internet-facing" \
  || fail "joined node is missing the internet-facing taint"
echo "  node $CLOUD_NODE joined with role label + taint"
assert_wan "remote joined the cluster (its bootstrap tunnel is up)" "$NODE_CONTAINER" "${CLUSTER}-control-plane"

echo "--- endpoint mirrored from CAPD's InternalIP; cloud DaemonSet lands; tunnel up ---"
until_ok 120 sh -c "kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.peer-endpoint-public-worker}' | base64 -d | grep -q ':51820'" \
  || fail "peer endpoint never mirrored (CAPD reports InternalIP only: the fallback must handle it)"
until_ok 180 sh -c "kubectl -n "$NS" get pods -l app=$REMOTE_DS -o wide --no-headers | grep '$CLOUD_NODE' | grep -q Running" \
  || fail "cloud DaemonSet pod never Running on the joined node"
assert_wan "adoption DaemonSet running on the remote" "$NODE_CONTAINER" "${CLUSTER}-control-plane"
IFACE=$(kubectl -n "$NS" get daemonset "$LOCAL_DS" -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep -o 'cldt[0-9a-f]*' | head -1)
until_ok 180 handshake_established || fail "no WireGuard handshake on $IFACE"
CLOUD_TUN=$(kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.peer-route-hosts-public-worker}' | base64 -d | cut -d, -f1)
until_ok 60 node_netns "${CLUSTER}-worker" ping -c2 -W3 "$CLOUD_TUN" \
  || fail "tunnel ping $CLOUD_TUN failed"
# Both directions: the remote's own dialer must equally have a live
# tunnel back, not just accept ours.
until_ok 60 node_netns "$NODE_CONTAINER" ping -c2 -W3 "$(kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath="{.data.node-tunnel-address-$ENDPOINT_A}" | base64 -d | cut -d/ -f1)" \
  || fail "reverse tunnel ping from the remote failed"
# The contract is host-prefix-only, not tunnel-subnet-only: peer node
# VIPs are routed via the tunnel, which is node-to-node reachability,
# while pod and service CIDRs are not. So every route via the interface
# is a single host, which `ip` prints bare (a /32), except the
# connected subnet the address itself creates (proto kernel). Anything
# else, of any width, is a route hijack.
BAD_ROUTES=$(node_netns "${CLUSTER}-worker" ip route show dev "$IFACE" \
  | grep -v "proto kernel" | awk '$1 ~ "/" && $1 !~ "/(32|128)$" {print}')
[ -z "$BAD_ROUTES" ] || fail "non-host route via $IFACE on the control plane: $BAD_ROUTES"
for v6 in $(node_netns "${CLUSTER}-worker" ip -6 route show dev "$IFACE" | grep -v "proto kernel" | awk '$1 ~ "/" && $1 !~ "/128$" {print $1}'); do
  fail "non-host IPv6 route via $IFACE: $v6"
done
echo "  bidirectional tunnel traffic on $IFACE; only host routes installed"
assert_wan "tunnel fully established, both directions carrying traffic" "$NODE_CONTAINER" "${CLUSTER}-control-plane"

echo "--- Calico is told which address to peer on ---"
# The remote node's own address belongs to its provider and is not
# reachable from here; its tunnel address is. Calico is told so through
# its per-node setting, which replaced giving the node a second address
# on a dummy interface.
TUNNEL_ADDR=$(kubectl -n "$NS" get machine public-worker \
  -o jsonpath='{.metadata.annotations.cloud-provisioning\.appmana\.com/wireguard-addr4}' | cut -d/ -f1)
[ -n "$TUNNEL_ADDR" ] || fail "the machine carries no tunnel address"
until_ok 120 sh -c "kubectl get node $CLOUD_NODE -o jsonpath='{.metadata.annotations.projectcalico\.org/IPv4Address}' | grep -q ." \
  || fail "Calico was never told the node's address"
CALICO_ADDR=$(kubectl get node "$CLOUD_NODE" -o jsonpath='{.metadata.annotations.projectcalico\.org/IPv4Address}')
# Calico normalises the prefix length to what the address carries on
# the interface, so the address is what matters here.
[ "${CALICO_ADDR%%/*}" = "$TUNNEL_ADDR" ] \
  || fail "Calico peers on $CALICO_ADDR, want the tunnel address $TUNNEL_ADDR"
# Nothing may have created a second address for the node to be found on.
node_netns "$NODE_CONTAINER" ip link show vip0 >/dev/null 2>&1 \
  && fail "a dummy vip0 interface exists; the node should carry no invented address"
echo "  Calico peers on $CALICO_ADDR, and no address was invented for it"

echo "--- the machine kind came from the template kind ---"
kubectl -n "$NS" get dockermachine public-worker >/dev/null 2>&1 \
  || fail "a DockerMachineTemplate must produce a DockerMachine"
IMG=$(kubectl -n "$NS" get dockermachine public-worker -o jsonpath='{.spec.customImage}')
[ "$IMG" = "$NODE_IMAGE" ] || fail "the machine did not take customImage from its template (got '$IMG')"
echo "  DockerMachineTemplate produced a DockerMachine carrying the template's spec"

echo "--- a second remote node: the mesh is many to many ---"
# Two tunnel endpoints on this side and two remote nodes. Every endpoint
# peers with every remote, and the remotes peer with each other: they
# are in different networks and share no path except the one they build.
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata: {name: public-worker-2, namespace: cloud-provisioning}
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: DockerMachineTemplate
    name: public-worker
EOF
until_ok 420 sh -c "kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker --no-headers | grep -c Ready | grep -q 2" \
  || fail "the second remote node never joined"
CLOUD_NODE_2=$(kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker -o jsonpath='{.items[1].metadata.name}')
echo "  remote nodes: $CLOUD_NODE and $CLOUD_NODE_2"
assert_wan "both remote nodes joined" "${CLUSTER}-control-plane"

echo "--- reachability across every pair of nodes ---"
# One pod per node, then every source against every destination, by pod
# address and by service address. Two local and two remote, so the pairs
# cross the tunnel in both directions and between the two remotes.
"$REPO_DIR/harness/health-check.sh" \
  "$ENDPOINT_A" "$ENDPOINT_B" "$CLOUD_NODE" "$CLOUD_NODE_2" 2>&1 | tee "$LOG_DIR/health-check.log"
grep -q "failed: 0" "$LOG_DIR/health-check.log" \
  || fail "reachability checks failed (see $LOG_DIR/health-check.log)"
echo "  every pair reachable, by pod and by service"

echo "--- what each peer is permitted, and what it is not ---"
# The accept list is a trie with one owner per prefix, so a prefix on
# two peers belongs to whichever was configured last. Every peer must
# carry only its own, and under encapsulation only host addresses.
for container in "${CLUSTER}-worker" "${CLUSTER}-worker2"; do
  ALLOWED=$(node_netns "$container" wg show "$IFACE" allowed-ips 2>/dev/null | awk '{$1=""; print}')
  echo "  $container permits:$ALLOWED"
  [ -n "$ALLOWED" ] || fail "$container permits nothing on $IFACE"
  for entry in $ALLOWED; do
    case "$entry" in
      0.0.0.0/0|::/0) fail "$container permits $entry, which takes every packet" ;;
    esac
    # No service range may appear: a ClusterIP is translated on the
    # sending node, so one crossing the tunnel would mean the reasoning
    # is wrong.
    case "$entry" in
      10.96.*) fail "$container permits service range $entry" ;;
    esac
    if [ "$WANT_ENCAP" = "encapsulated" ]; then
      case "$entry" in
        */32|*/128) ;;
        *) fail "$container permits $entry, but $CNI encapsulates and needs host addresses only" ;;
      esac
    fi
  done
  # Duplicates across peers are the failure this exists to catch.
  DUPES=$(node_netns "$container" wg show "$IFACE" allowed-ips 2>/dev/null | awk '{for(i=2;i<=NF;i++) print $i}' | sort | uniq -d)
  [ -z "$DUPES" ] || fail "$container permits the same prefix on more than one peer: $DUPES"
done
# Absence of the wrong entries is not presence of the right ones. Under
# a native network every remote's own block has to be permitted, or the
# pods on it are unreachable from here.
if [ "$WANT_ENCAP" = "native" ]; then
  for machine in public-worker public-worker-2; do
    node=$(kubectl -n "$NS" get machine "$machine" -o jsonpath='{.status.nodeRef.name}')
    [ -n "$node" ] || fail "$machine has no node"
    until_ok 180 sh -c "kubectl -n '$NS' get secret '$PEER_SECRET' -o jsonpath='{.data.peer-allowed-ips-$machine}' | base64 -d | grep -q /26" \
      || fail "$machine's peer entry never carried its node's blocks"
    BLOCK=$(kubectl -n "$NS" get secret "$PEER_SECRET" -o jsonpath="{.data.node-pod-cidrs-$node}" | base64 -d)
    PERMITTED=$(kubectl -n "$NS" get secret "$PEER_SECRET" -o jsonpath="{.data.peer-allowed-ips-$machine}" | base64 -d)
    case "$PERMITTED" in
      *"$BLOCK"*) ;;
      *) fail "$machine permits $PERMITTED, missing its node's block $BLOCK" ;;
    esac
  done
  echo "  each remote's own blocks are permitted, so its pods are reachable"
fi
echo "  every peer carries only its own prefixes, none of them cluster wide"

kubectl delete provisionednodeclaim public-worker-2 --wait=true >/dev/null 2>&1 || true
assert_wan "after the second claim was withdrawn" "${CLUSTER}-control-plane"

echo "--- cascade: delete the claim, the node container must terminate ---"
kubectl delete provisionednodeclaim public-worker --wait=true >/dev/null
# Budgeted generously on purpose: CAPI deletion drains the node, waits
# for volume detach, then deletes the Node object, and the Machine's
# own bounded timeouts (set by the claim reconciler) have to be able to
# elapse before this gives up; otherwise a slow-but-correct teardown
# reads as a failure.
until_ok 420 sh -c "! docker ps --format '{{.Names}}' | grep -q public-worker" \
  || fail "node container survived claim deletion"
until_ok 60 sh -c "! kubectl get machine public-worker" || fail "Machine survived claim deletion"
# The teardown must leave the surviving node exactly as it found it:
# a removed peer must not strand routes behind.
assert_wan "after the claim cascade removed the remote" "${CLUSTER}-control-plane"

echo
echo "  one claim became a real second kind node over a real tunnel, and one delete removed it"

echo "--- uninstall: the release must take everything with it ---"
# The controller creates the DaemonSets, the peer Secret and the
# per-machine adoption Secrets at RUNTIME, so Helm never templated
# them and an uninstall used to leave them behind: a tunnel interface
# on every endpoint node with nothing reconciling it. They carry an
# ownerReference to the controller's own Deployment now, so removing
# the release collects them.
helm uninstall cloud-provisioning --namespace "$NS" --wait >/dev/null 2>&1 \
  || fail "helm uninstall failed"
for res in daemonset/$LOCAL_DS daemonset/$REMOTE_DS secret/$PEER_SECRET; do
  until_ok 90 sh -c "! kubectl -n "$NS" get $res" \
    || fail "$res survived the uninstall (nothing owns it: an orphaned tunnel is the failure this project exists to prevent)"
done
# The CRD survives: Helm does not remove crds/, and it should not.
# Removing it would delete every claim still in the cluster, and with
# the controller already gone nothing would run their finalizers, so
# the provisioned instances would be orphaned and still billed while
# the objects hung in Terminating.
kubectl get crd provisionednodeclaims.cloud-provisioning.appmana.com >/dev/null 2>&1 \
  || fail "the claim CRD was removed by the uninstall: that orphans running instances"
# The tunnel interface itself must be gone from the node: on a node
# that reaches the cluster over its LAN, removing the DaemonSet means
# the tunnel is meant to be gone. The cloud node is the opposite case
# and keeps its interface, since it is that node's only path back, so
# it outlives whatever manages it.
until_ok 60 sh -c "! sudo nsenter -t \$(docker inspect -f '{{.State.Pid}}' ${CLUSTER}-control-plane) -n ip link show $IFACE" \
  || fail "$IFACE survived the uninstall on the control-plane node"
assert_wan "after uninstall" "${CLUSTER}-control-plane"
echo "  release removed: no DaemonSets, no peer Secret, no tunnel interface (CRD kept, by design)"

echo "--- reinstall: a second install must work with no manual steps ---"
helm install cloud-provisioning "$REPO_DIR/charts/cloud-provisioning" \
  --namespace "$NS" -f "$LOG_DIR/values.yaml" --wait --timeout 5m \
  >"$LOG_DIR/helm-reinstall.log" 2>&1 || fail "reinstall failed (see $LOG_DIR/helm-reinstall.log)"
until_ok 180 sh -c "kubectl -n "$NS" get secret $PEER_SECRET -o jsonpath='{.data.node-public-key-$ENDPOINT_A}' | grep -q ." \
  || fail "the reinstalled release never came back up"
assert_wan "after reinstall" "${CLUSTER}-control-plane"
echo "  reinstall clean"

echo "ALL ASSERTIONS PASSED (logs: $LOG_DIR)"

