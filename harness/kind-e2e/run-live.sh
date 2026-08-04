#!/usr/bin/env bash
# LIVE end-to-end: one ProvisionedNodeClaim against a ONE-node kind
# cluster becomes a real second node, entirely through the product's
# own abstractions -- no shim CRDs, no bespoke cluster rig:
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
NODE_IMAGE=kindest/node:v1.34.0
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
    kubectl -n wg-dialer get pods -o wide
    kubectl -n wg-dialer describe pods
    kubectl -n wg-dialer logs daemonset/wg-dialer --tail=30
    kubectl -n wg-dialer logs daemonset/wg-dialer-cloud --tail=30
    kubectl get machines,dockermachines -A -o wide
    kubectl get cluster "$CLUSTER" -o yaml
    kubectl get machine public-worker -o yaml
    kubectl get dockermachine public-worker -o yaml
    kubectl get provisionednodeclaim -A -o yaml
    kubectl get events -A --sort-by=.lastTimestamp | tail -25
    kubectl -n capd-system logs deployment/capd-controller-manager --tail=30
    kubectl -n capi-system logs deployment/capi-controller-manager --tail=30
    # The actual tunnel state on both sides. Without this a ping
    # failure is undiagnosable after teardown -- which cost a full
    # re-run once already. Uses host tools in each netns (the node
    # image has no wg).
    for c in "${CLUSTER}-control-plane" "${NODE_CONTAINER:-}"; do
      [ -n "$c" ] || continue
      echo "===== netns state: $c ====="
      node_netns "$c" wg show all 2>&1
      node_netns "$c" ip -br addr 2>&1
      node_netns "$c" ip route show 2>&1
      node_netns "$c" ip -6 route show 2>&1
    done
    echo "===== peer secret ====="
    kubectl -n wg-dialer get secret wg-dialer-peer -o json |
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

# Run a command inside a node container's NETWORK namespace using the
# HOST's tools. The node image ships neither wg nor ping (confirmed:
# both MISSING in kindest/node), so `docker exec ... wg show` doesn't
# just fail -- piped into awk it makes the assertion VACUOUS, because
# the pipeline's status is awk's and awk succeeds on empty input. Every
# tunnel assertion below therefore enters the namespace instead, where
# the host's own wg/ping/ip apply to the container's interfaces.
node_netns() {
  local container="$1"; shift
  local pid
  pid=$(docker inspect -f '{{.State.Pid}}' "$container") || return 1
  sudo nsenter -t "$pid" -n "$@"
}

# WAN reachability, asserted at EVERY phase on EVERY node. This is the
# invariant the whole design exists to protect: bringing up a tunnel
# must never cost a node its ordinary path off-box. Checking it once at
# the end would not distinguish "never broke" from "broke and
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
  hs=$(node_netns "${CLUSTER}-control-plane" wg show "$IFACE" latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')
  [ -n "$hs" ] && [ "$hs" -gt 0 ] 2>/dev/null
}

echo "--- build: controller image, dialer image, dialer release binary ---"
docker build -q --target dialer -t cldt-dialer:e2e -f "$REPO_DIR/controller/Dockerfile" "$REPO_DIR" >/dev/null
docker build -q --target endpoint-controller -t cldt-controller:e2e -f "$REPO_DIR/controller/Dockerfile" "$REPO_DIR" >/dev/null
mkdir -p "$LOG_DIR/binaries"
( cd "$REPO_DIR/controller" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$LOG_DIR/binaries/wg-dialer-linux-amd64" ./cmd/dialer )
BIN_SHA=$(sha256sum "$LOG_DIR/binaries/wg-dialer-linux-amd64" | awk '{print $1}')

echo "--- kind cluster: ONE node (docker socket mounted -- CAPD manages sibling containers through it, the documented CAPI docker-provider setup) ---"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s --config - >/dev/null <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
EOF
export KUBECONFIG="$LOG_DIR/kubeconfig"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
kind load docker-image cldt-dialer:e2e cldt-controller:e2e --name "$CLUSTER" >/dev/null
CP_NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
CP_IP=$(docker inspect "${CLUSTER}-control-plane" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
[ -n "$CP_IP" ] || fail "could not resolve the control plane's kind network address"
echo "  control plane: $CP_NODE @ $CP_IP"
assert_wan "baseline, before anything touches networking" "${CLUSTER}-control-plane"

# Binary delivery: NO download server. The docker provider mounts
# $LOG_DIR/binaries read-only into the node container (extra-mounts in
# its provider config) and the userdata curls a file:// URL -- fully
# deterministic, nothing to start, nothing to leak, nothing to go
# stale; the pinned-sha verification is unchanged.
BIN_URL="file:///opt/dialer-dist/wg-dialer-linux-amd64"

echo "--- install CAPI core + CAPD (clusterctl) ---"
clusterctl init --infrastructure docker --wait-providers >"$LOG_DIR/clusterctl-init.log" 2>&1 \
  || fail "clusterctl init failed (see $LOG_DIR/clusterctl-init.log)"
SERVED=$(kubectl get crd dockermachines.infrastructure.cluster.x-k8s.io -o jsonpath='{range .spec.versions[?(@.served)]}{.name}{" "}{end}')
echo "  DockerMachine served versions: $SERVED"
echo "$SERVED" | grep -qw v1beta2 || fail "installed CAPD does not serve infrastructure v1beta2 -- adjust pkg/join/docker's GVK to: $SERVED"

# CAPD (not this project) needs the target cluster's kubeconfig, at an
# address reachable from inside the cluster's pods, in a Secret its
# clustercache can see -- it filters by the cluster-name label, so an
# unlabelled Secret is invisible to it ("not found" despite existing).
sed "s#server: https://127.0.0.1:[0-9]*#server: https://$CP_IP:6443#" "$KUBECONFIG" > "$LOG_DIR/incluster-kubeconfig"
kubectl create secret generic "$CLUSTER-kubeconfig" -n wg-dialer --type=cluster.x-k8s.io/secret \
  --from-file=value="$LOG_DIR/incluster-kubeconfig" --dry-run=client -o yaml > "$LOG_DIR/kubeconfig-secret.yaml"

echo "--- INSTALL THE CHART -- and nothing else ---"
# Everything the product owns comes from `helm install`: the claim CRD,
# RBAC, the controller, the provider config, and the externally-managed
# Cluster pair. Nothing here creates those by hand.
#
# This is the point of this harness. When the setup assembled them with
# kubectl, the tests passed green against an install path that did not
# exist -- the chart was missing the Cluster pair and the provider
# config, and no test could see it. Anything the chart fails to render
# must fail here.
kubectl create namespace wg-dialer >/dev/null
kubectl apply -n wg-dialer -f "$LOG_DIR/kubeconfig-secret.yaml" >/dev/null
kubectl label secret "$CLUSTER-kubeconfig" -n wg-dialer "cluster.x-k8s.io/cluster-name=$CLUSTER" >/dev/null
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
tunnel: {endpoints: "node-role.kubernetes.io/control-plane="}
dialerBinary: {amd64: {url: "$BIN_URL", sha256: "$BIN_SHA"}}
targetCluster: {enabled: true, name: "$CLUSTER", infrastructureKind: DockerCluster}
provider:
  docker:
    nodeImage: "$NODE_IMAGE"
    extraMounts: "$LOG_DIR/binaries:/opt/dialer-dist"
    preloadImages: cldt-dialer:e2e
EOF
helm install cloud-provisioning "$REPO_DIR/charts/cloud-provisioning" \
  --namespace wg-dialer -f "$LOG_DIR/values.yaml" \
  --wait --timeout 5m >"$LOG_DIR/helm-install.log" 2>&1 \
  || fail "helm install failed (see $LOG_DIR/helm-install.log)"

# Everything this harness touches from here lives in the release
# namespace, the same as a real install.
kubectl config set-context --current --namespace=wg-dialer >/dev/null

echo "--- mesh precondition: the single (control-plane, explicitly selected) node allocates + publishes ---"
until_ok 60 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-tunnel-address-$CP_NODE}' | grep -q ." \
  || fail "tunnel address never allocated for the explicitly-selected control-plane node"
until_ok 120 sh -c "kubectl -n wg-dialer get pods -l app=wg-dialer --no-headers | grep -q Running" \
  || fail "on-prem dialer pod never Running (image load / scheduling problem)"
until_ok 60 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-public-key-$CP_NODE}' | grep -q ." \
  || fail "the dialer pod never self-published its public key"
echo "  dialer pod Running, key published"
assert_wan "on-prem dialer running (interface created, key published)" "${CLUSTER}-control-plane"

echo "--- ONE claim ---"
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata: {name: public-worker, namespace: wg-dialer}
spec:
  requests: {cpu: "1", memory: 1Gi}
  arch: amd64
EOF

echo "--- the chart's Cluster pair is reported provisioned BY THE CONTROLLER ---"
# Externally-managed means Cluster API's own controllers stand down, so
# nothing upstream ever sets this status and Machines block on it
# forever. It used to be a `kubectl patch` here, which is exactly the
# kind of step a real install would never know to run.
until_ok 90 sh -c "kubectl -n wg-dialer get cluster $CLUSTER -o jsonpath='{.status.initialization.controlPlaneInitialized}' | grep -q true" \
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
until_ok 120 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-endpoint-public-worker}' | base64 -d | grep -q ':51820'" \
  || fail "peer endpoint never mirrored (CAPD reports InternalIP only -- the fallback must handle it)"
until_ok 180 sh -c "kubectl -n wg-dialer get pods -l app=wg-dialer-cloud -o wide --no-headers | grep '$CLOUD_NODE' | grep -q Running" \
  || fail "cloud DaemonSet pod never Running on the joined node"
assert_wan "adoption DaemonSet running on the remote" "$NODE_CONTAINER" "${CLUSTER}-control-plane"
IFACE=$(kubectl -n wg-dialer get daemonset wg-dialer -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep -o 'cldt[0-9a-f]*' | head -1)
until_ok 180 handshake_established || fail "no WireGuard handshake on $IFACE"
CLOUD_TUN=$(kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-route-hosts-public-worker}' | base64 -d | cut -d, -f1)
until_ok 60 node_netns "${CLUSTER}-control-plane" ping -c2 -W3 "$CLOUD_TUN" \
  || fail "tunnel ping $CLOUD_TUN failed"
# Both directions: the remote's own dialer must equally have a live
# tunnel back, not just accept ours.
until_ok 60 node_netns "$NODE_CONTAINER" ping -c2 -W3 "$(kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath="{.data.node-tunnel-address-$CP_NODE}" | base64 -d | cut -d/ -f1)" \
  || fail "reverse tunnel ping from the remote failed"
# The contract is HOST-PREFIX-ONLY, not tunnel-subnet-only: peer node
# VIPs are legitimately routed via the tunnel (that IS node-to-node
# reachability), pod/service CIDRs never are. So every route via the
# interface must be a single host -- `ip` prints those bare (a /32) --
# except the connected subnet the address itself creates (proto
# kernel). Anything else, of any width, is the hijack class.
BAD_ROUTES=$(node_netns "${CLUSTER}-control-plane" ip route show dev "$IFACE" \
  | grep -v "proto kernel" | awk '$1 ~ "/" && $1 !~ "/(32|128)$" {print}')
[ -z "$BAD_ROUTES" ] || fail "non-host route via $IFACE on the control plane: $BAD_ROUTES"
for v6 in $(node_netns "${CLUSTER}-control-plane" ip -6 route show dev "$IFACE" | grep -v "proto kernel" | awk '$1 ~ "/" && $1 !~ "/128$" {print $1}'); do
  fail "non-host IPv6 route via $IFACE: $v6"
done
echo "  bidirectional tunnel traffic on $IFACE; only host routes installed"
assert_wan "tunnel fully established, both directions carrying traffic" "$NODE_CONTAINER" "${CLUSTER}-control-plane"

echo "--- cascade: delete the claim, the node container must terminate ---"
kubectl delete provisionednodeclaim public-worker --wait=true >/dev/null
# Budgeted generously on purpose: CAPI deletion drains the node, waits
# for volume detach, then deletes the Node object, and the Machine's
# own bounded timeouts (set by the claim reconciler) have to be able to
# elapse before this gives up -- otherwise a slow-but-correct teardown
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
helm uninstall cloud-provisioning --namespace wg-dialer --wait >/dev/null 2>&1 \
  || fail "helm uninstall failed"
for res in daemonset/wg-dialer daemonset/wg-dialer-cloud secret/wg-dialer-peer; do
  until_ok 90 sh -c "! kubectl -n wg-dialer get $res" \
    || fail "$res survived the uninstall (nothing owns it -- an orphaned tunnel is the failure this project exists to prevent)"
done
# The CRD deliberately SURVIVES: Helm never removes crds/, and it
# should not. Removing it would delete every claim still in the
# cluster, and with the controller already gone nothing would run their
# finalizers -- the provisioned instances would be orphaned and still
# billed while the objects hung in Terminating forever.
kubectl get crd provisionednodeclaims.cloud-provisioning.appmana.com >/dev/null 2>&1 \
  || fail "the claim CRD was removed by the uninstall -- that orphans running instances"
# The tunnel interface itself must be gone from the node: on a node
# that reaches the cluster over its LAN, removing the DaemonSet means
# the tunnel is meant to be gone. (The CLOUD node is the opposite case
# and deliberately keeps its interface -- it is that node's only path
# back, so it outlives whatever manages it.)
until_ok 60 sh -c "! sudo nsenter -t \$(docker inspect -f '{{.State.Pid}}' ${CLUSTER}-control-plane) -n ip link show $IFACE" \
  || fail "$IFACE survived the uninstall on the control-plane node"
assert_wan "after uninstall" "${CLUSTER}-control-plane"
echo "  release removed: no DaemonSets, no peer Secret, no CRD, no tunnel interface"

echo "--- reinstall: a second install must work with no manual steps ---"
helm install cloud-provisioning "$REPO_DIR/charts/cloud-provisioning" \
  --namespace wg-dialer -f "$LOG_DIR/values.yaml" --wait --timeout 5m \
  >"$LOG_DIR/helm-reinstall.log" 2>&1 || fail "reinstall failed (see $LOG_DIR/helm-reinstall.log)"
until_ok 120 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-public-key-$CP_NODE}' | grep -q ." \
  || fail "the reinstalled release never came back up"
assert_wan "after reinstall" "${CLUSTER}-control-plane"
echo "  reinstall clean"

echo "ALL ASSERTIONS PASSED (logs: $LOG_DIR)"

