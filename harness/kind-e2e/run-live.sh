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
    kubectl get dockermachine public-worker -o yaml
    kubectl -n capd-system logs deployment/capd-controller-manager --tail=30
    kubectl -n capi-system logs deployment/capi-controller-manager --tail=30
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

handshake_established() {
  local hs
  hs=$(node_netns "${CLUSTER}-control-plane" wg show "$IFACE" latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')
  [ -n "$hs" ] && [ "$hs" -gt 0 ] 2>/dev/null
}

echo "--- build: controller binary, dialer image, dialer release binary ---"
( cd "$REPO_DIR/controller" && go build -o "$LOG_DIR/endpoint-controller" ./cmd/endpoint-controller )
docker build -q --target dialer -t cldt-dialer:e2e -f "$REPO_DIR/controller/Dockerfile" "$REPO_DIR" >/dev/null
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
kind load docker-image cldt-dialer:e2e --name "$CLUSTER" >/dev/null
CP_NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
CP_IP=$(docker inspect "${CLUSTER}-control-plane" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
[ -n "$CP_IP" ] || fail "could not resolve the control plane's kind network address"
echo "  control plane: $CP_NODE @ $CP_IP"

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

echo "--- the externally-managed Cluster/DockerCluster pair + kubeconfig Secret ---"
# --wait-providers returns before the webhook endpoints actually
# serve; give them time by retrying the apply itself.
cat > "$LOG_DIR/cluster-pair.yaml" <<EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $CLUSTER
  namespace: default
  annotations: {cluster.x-k8s.io/managed-by: external}
spec:
  controlPlaneEndpoint: {host: "$CP_IP", port: 6443}
  infrastructureRef: {apiGroup: infrastructure.cluster.x-k8s.io, kind: DockerCluster, name: $CLUSTER}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: DockerCluster
metadata:
  name: $CLUSTER
  namespace: default
  annotations: {cluster.x-k8s.io/managed-by: external}
spec:
  controlPlaneEndpoint: {host: "$CP_IP", port: 6443}
EOF
until_ok 120 kubectl apply -f "$LOG_DIR/cluster-pair.yaml" \
  || fail "could not create the Cluster/DockerCluster pair (webhooks never came up)"
# Documented deviations for a BYO/externally-managed cluster, the same
# class as the AWSCluster status.ready patch: mark the infra cluster
# provisioned, and mark the Cluster's initialization fields -- they are
# one-way ("initialized" is sticky by CAPI's own contract), and without
# controlPlaneInitialized no infra provider will create worker machines
# for a cluster that has no CAPI-managed control-plane Machine to
# derive it from.
kubectl patch dockercluster "$CLUSTER" --subresource=status --type=merge \
  -p '{"status":{"initialization":{"provisioned":true},"ready":true}}' >/dev/null 2>&1 \
  || kubectl patch dockercluster "$CLUSTER" --subresource=status --type=merge \
    -p '{"status":{"ready":true}}' >/dev/null
kubectl patch cluster "$CLUSTER" --subresource=status --type=merge \
  -p '{"status":{"initialization":{"controlPlaneInitialized":true,"infrastructureProvisioned":true}}}' >/dev/null
# CAPI's clustercache needs API access at an address reachable from
# inside the cluster's pods -- and it caches kubeconfig Secrets
# FILTERED by the cluster-name label: an unlabeled Secret is invisible
# to it ("not found" despite existing).
sed "s#server: https://127.0.0.1:[0-9]*#server: https://$CP_IP:6443#" "$KUBECONFIG" > "$LOG_DIR/incluster-kubeconfig"
kubectl create secret generic "$CLUSTER-kubeconfig" -n default \
  --type=cluster.x-k8s.io/secret \
  --from-file=value="$LOG_DIR/incluster-kubeconfig" >/dev/null
kubectl label secret "$CLUSTER-kubeconfig" -n default "cluster.x-k8s.io/cluster-name=$CLUSTER" >/dev/null

echo "--- operator inputs: claim CRD, reference RBAC (namespace + the dialer pods' ServiceAccount), provider config ---"
kubectl apply -f "$REPO_DIR/manifests/wg-dialer/crd.yaml" >/dev/null
kubectl apply -f "$REPO_DIR/manifests/wg-dialer/rbac.yaml" >/dev/null
kubectl -n wg-dialer create secret generic docker-provider-config \
  --from-literal=node-image="$NODE_IMAGE" \
  --from-literal=extra-mounts="$LOG_DIR/binaries:/opt/dialer-dist" \
  --from-literal=preload-images="cldt-dialer:e2e" >/dev/null

echo "--- run the controller (kubeadm join specialization, docker fulfillment) ---"
"$LOG_DIR/endpoint-controller" \
  --dialer-image=cldt-dialer:e2e \
  --dialer-pod-cidrs=10.244.0.0/16 \
  --dialer-service-cidrs=10.96.0.0/12 \
  --join-provider=kubeadm \
  --join-template-path="$REPO_DIR/join-patterns/kubeadm-worker.cloud-config.tmpl" \
  --join-api-address="https://$CP_IP:6443" \
  --join-api-vip="$CP_IP" \
  --join-node-vip4-prefix=10.199.0. \
  --join-node-vip6-prefix=fd99:: \
  --join-node-vip-start=200 \
  --join-dialer-binary-url-amd64="$BIN_URL" \
  --join-dialer-binary-sha256-amd64="$BIN_SHA" \
  --tunnel-endpoints=node-role.kubernetes.io/control-plane= \
  >"$LOG_DIR/controller.log" 2>&1 &
CONTROLLER_PID=$!

echo "--- mesh precondition: the single (control-plane, explicitly selected) node allocates + publishes ---"
until_ok 60 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-tunnel-address-$CP_NODE}' | grep -q ." \
  || fail "tunnel address never allocated for the explicitly-selected control-plane node"
until_ok 120 sh -c "kubectl -n wg-dialer get pods -l app=wg-dialer --no-headers | grep -q Running" \
  || fail "on-prem dialer pod never Running (image load / scheduling problem)"
until_ok 60 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-public-key-$CP_NODE}' | grep -q ." \
  || fail "the dialer pod never self-published its public key"
echo "  dialer pod Running, key published"

echo "--- ONE claim ---"
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata: {name: public-worker, namespace: default}
spec:
  requests: {cpu: "1", memory: 1Gi}
  arch: amd64
EOF

echo "--- CAPD launches the node container and the bootstrap joins it ---"
until_ok 90 kubectl get dockermachine public-worker || fail "DockerMachine never created"
until_ok 60 kubectl get secret public-worker-bootstrap || fail "bootstrap Secret never rendered"
until_ok 240 sh -c "docker ps --format '{{.Names}}' | grep -q 'public-worker'" \
  || fail "CAPD never launched the node container"
NODE_CONTAINER=$(docker ps --format '{{.Names}}' | grep public-worker | head -1)
echo "  node container: $NODE_CONTAINER"

until_ok 300 sh -c "kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker --no-headers | grep -q ." \
  || { docker logs "$NODE_CONTAINER" 2>&1 | tail -20 >&2; docker exec "$NODE_CONTAINER" journalctl -u wg-dialer --no-pager 2>/dev/null | tail -10 >&2 || true; fail "the node never joined with the cloud-worker role label"; }
CLOUD_NODE=$(kubectl get node -l cloud-provisioning.appmana.com/role=cloud-worker -o jsonpath='{.items[0].metadata.name}')
kubectl get node "$CLOUD_NODE" -o jsonpath='{.spec.taints}' | grep -q "internet-facing" \
  || fail "joined node is missing the internet-facing taint"
echo "  node $CLOUD_NODE joined with role label + taint"

echo "--- endpoint mirrored from CAPD's InternalIP; cloud DaemonSet lands; tunnel up ---"
until_ok 120 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-endpoint-public-worker}' | base64 -d | grep -q ':51820'" \
  || fail "peer endpoint never mirrored (CAPD reports InternalIP only -- the fallback must handle it)"
until_ok 180 sh -c "kubectl -n wg-dialer get pods -l app=wg-dialer-cloud -o wide --no-headers | grep '$CLOUD_NODE' | grep -q Running" \
  || fail "cloud DaemonSet pod never Running on the joined node"
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

echo "--- cascade: delete the claim, the node container must terminate ---"
kubectl delete provisionednodeclaim public-worker --wait=true >/dev/null
until_ok 180 sh -c "! docker ps --format '{{.Names}}' | grep -q public-worker" \
  || fail "node container survived claim deletion"
until_ok 60 sh -c "! kubectl get machine public-worker" || fail "Machine survived claim deletion"

echo
echo "ALL ASSERTIONS PASSED: one claim became a real second kind node over a real tunnel, and one delete removed it (logs: $LOG_DIR)"
