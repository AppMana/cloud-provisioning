#!/usr/bin/env bash
# Reachability across a set of nodes: one pod on each, then every source
# against every destination, by pod address and by service address, plus
# DNS and the path off the cluster.
#
# Adapted from the dual-stack health check in the Calico fork
# (hack/appmana/ipv6-health-check.sh), reduced to Linux and to what this
# project needs to prove: that a node joined over a tunnel is an
# ordinary member of the pod network in both directions.
#
# The service checks matter here for a specific reason. A ClusterIP is
# translated to a backend address on the sending node, so no service
# range is permitted anywhere in the tunnel's accept list. If that
# reasoning is wrong, these are the checks that fail.
#
# Usage: health-check.sh [--namespace NS] [--service-port PORT] NODE...
set -uo pipefail

# Unique per run. A fixed name couples this run to the previous run's
# cleanup having finished, and namespace deletion is allowed to hang
# in real clusters: with a node dead ungracefully, an aggregated API
# served from it (k0s ships metrics-server) makes the namespace
# controller's discovery fail, and every deletion waits until the
# aggregated API reschedules. Measured: cp3 down, the previous check's
# namespace Terminating for the whole 150s wait, and the survivors'
# check reported "no checks ran" on a healthy network. A fresh name
# needs nobody's deletion to finish; the old ones drain on their own
# once discovery recovers.
NAMESPACE="cloud-provisioning-health-$(date +%s)"
SERVICE_PORT=8080
# The large object every pod serves, for the transfer check below. A
# megabyte is far past any MTU on this path, so a size that comes back
# short or a transfer that stalls is the packet-size failure and
# nothing else.
BIG_KIB="${HEALTH_CHECK_BIG_KIB:-1024}"
BIG_BYTES=$((BIG_KIB * 1024))
IMAGE="${HEALTH_CHECK_IMAGE:-busybox:1.37}"
NODES=()
# How to run a command inside a probe pod.
#
# kubectl exec reaches a pod by way of its node's kubelet, which the API
# server connects to directly. A node joined over a tunnel has no return
# path for that: the control plane carries no tunnel, which is what
# keeps a tunnel from ever costing a control plane its default route. So
# on a node that is off the local network, kubectl exec cannot work, and
# using it would report the tunnel as broken when it is the exec path
# that is absent.
#
# With node access, the container runtime on the node runs the command
# instead. The traffic under test is unchanged; only the way the probe
# is started differs.
EXEC_VIA="${HEALTH_CHECK_EXEC:-kubectl}"   # kubectl | node
# A node's container is not always named after the node. A lab names
# them for the lab, so the two differ and the exec has to be told.
NODE_PREFIX="${HEALTH_CHECK_NODE_PREFIX:-}"
# Which containerd crictl asks. Empty means crictl's default, which is
# where kubeadm's pods live; a distribution that brings its own
# runtime keeps its pods behind its own socket, and a crictl aimed at
# the wrong one sees no containers, which reads as every path being
# broken at once.
CRI_ENDPOINT="${HEALTH_CHECK_CRI_ENDPOINT:-}"
# Report the matrix and exit 0 rather than failing on the first
# unreachable pair. Which pairs reach each other is the measurement
# when comparing tunnel placements, not a pass or fail.
REPORT_ONLY="${HEALTH_CHECK_REPORT_ONLY:-0}"

while [[ $# -gt 0 ]]; do
  case $1 in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --service-port) SERVICE_PORT="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --exec) EXEC_VIA="$2"; shift 2 ;;
    --report-only) REPORT_ONLY=1; shift ;;
    *) NODES+=("$1"); shift ;;
  esac
done

if [[ ${#NODES[@]} -eq 0 ]]; then
  echo "usage: $0 [--namespace NS] [--service-port PORT] NODE..." >&2
  exit 2
fi

cleanup() {
  kubectl delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# A namespace left terminating from a previous run rejects everything
# created in it, and the rejection surfaces as "pods did not all start",
# which reads as the network being broken. Wait for it to finish going,
# and force out anything holding it open: a probe pod on a node that was
# briefly unreachable can hold a namespace for hours.
if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  for _ in $(seq 1 30); do
    phase=$(kubectl get namespace "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo gone)
    [[ "$phase" == "Terminating" ]] || break
    kubectl -n "$NAMESPACE" delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
    sleep 5
  done
fi
kubectl create namespace "$NAMESPACE" >/dev/null 2>&1 || true
kubectl get namespace "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null | grep -qv Terminating \
  || { echo "FAIL: $NAMESPACE is still terminating, so nothing can be created in it" >&2; exit 1; }

declare -A POD_IP
declare -A SERVICE_IP

for node in "${NODES[@]}"; do
  podname="hc-${node}"
  # Tolerate everything: a provisioned node carries a taint that keeps
  # ordinary workloads off it, and this check is not ordinary.
  kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $podname
  labels: {app: $podname}
spec:
  nodeSelector: {kubernetes.io/hostname: $node}
  tolerations:
    - operator: Exists
  containers:
    - name: serve
      image: $IMAGE
      command: ["sh","-c","mkdir -p /tmp/www; printf ok > /tmp/www/index.html; dd if=/dev/zero of=/tmp/www/big bs=1024 count=$BIG_KIB 2>/dev/null; httpd -f -p $SERVICE_PORT -h /tmp/www"]
      ports: [{containerPort: $SERVICE_PORT}]
EOF
  kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: svc-$podname
spec:
  selector: {app: $podname}
  ports:
    - {name: http, port: $SERVICE_PORT, targetPort: $SERVICE_PORT}
EOF
done

echo "waiting for a pod on each of ${#NODES[@]} nodes"
deadline=$((SECONDS + 300))
while true; do
  ready=true
  for node in "${NODES[@]}"; do
    phase=$(kubectl get pod "hc-${node}" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
    [[ "$phase" == "Running" ]] || ready=false
  done
  $ready && break
  if (( SECONDS > deadline )); then
    echo "FAIL: pods did not all start" >&2
    kubectl get pods -n "$NAMESPACE" -o wide >&2
    exit 1
  fi
  sleep 5
done

for node in "${NODES[@]}"; do
  POD_IP[$node]=$(kubectl get pod "hc-${node}" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')
  SERVICE_IP[$node]=$(kubectl get service "svc-hc-${node}" -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}')
  echo "  $node: pod ${POD_IP[$node]}  service ${SERVICE_IP[$node]}"
  if [[ -z "${POD_IP[$node]}" || -z "${SERVICE_IP[$node]}" ]]; then
    echo "FAIL: $node has no pod or service address" >&2
    exit 1
  fi
done

# Run a command in the probe pod on a node, by whichever route works.
pod_exec() {
  local node="$1"; shift
  if [[ "$EXEC_VIA" == "node" ]]; then
    local cid
    local ctr="${NODE_PREFIX}${node}"
    # Anchored, and scoped to this check's own namespace. --name is a
    # substring regex, and kube-apiserver contains "serve", so on a
    # control plane the unanchored form can select the API server, whose
    # image has neither wget nor ping and reads as a broken network.
    cid=$(docker exec "$ctr" crictl ${CRI_ENDPOINT:+--runtime-endpoint "$CRI_ENDPOINT"} ps --name '^serve$' \
      --label "io.kubernetes.pod.namespace=$NAMESPACE" -q 2>/dev/null | head -1)
    [[ -n "$cid" ]] || return 1
    docker exec "$ctr" crictl ${CRI_ENDPOINT:+--runtime-endpoint "$CRI_ENDPOINT"} exec "$cid" "$@" 2>/dev/null
  else
    kubectl exec "hc-${node}" -n "$NAMESPACE" -- "$@" 2>/dev/null
  fi
}

http_from() {
  local src="$1" url="$2"
  pod_exec "$src" wget -q -T 5 -O - "$url" | grep -q ok
}

# A transfer big enough to need full-size segments, verified by size.
#
# Every check above this one fits in a single small packet, so all of
# them pass on a path whose largest packet cannot cross: the request
# goes, the reply goes, and the failure waits for the first real
# workload. What makes that failure invisible rather than loud is that
# nothing can report it. An endpoint forwards for nodes two hops away,
# so the "packet too big" it would have to send goes back to a pod it
# does not host, on a node it does not control (see the dialer's
# underlayMTU). That is why the dialer clamps the advertised segment
# size on the handshake instead of relying on discovery, and this is
# the check that holds the clamp to its claim: an encapsulating
# network stacks its own header inside the tunnel's, so the margin
# this measures is the one that actually gets consumed.
big_from() {
  local src="$1" url="$2" got
  got=$(pod_exec "$src" wget -q -T 20 -O - "$url" | wc -c)
  [[ "$got" == "$BIG_BYTES" ]]
}

# A node that has just joined has to be given its pod block, have that
# block reach the peers, and have the network distribute a route for it.
# That is convergence, not flakiness, so it is waited for once here
# rather than folded into every check's timeout.
http_from_early() {
  http_from "$1" "$2"
}
if [[ ${#NODES[@]} -gt 1 ]]; then
  first="${NODES[0]}"
  echo
  echo "waiting for reachability to converge between nodes"
  converge_start=$SECONDS
  converge_deadline=$((SECONDS + ${HEALTH_CHECK_CONVERGE_SECONDS:-420}))
  # The gate has to probe what the checks then measure, or it opens on
  # paths the checks do not take and the checks eat the convergence out
  # of their own much shorter retries. Three paths per node: to it, from
  # it, and from it by service address. The return path is not implied
  # by the forward one: the accept list that admits a source on one
  # tunnel says nothing about the reverse direction, and a service path
  # additionally waits on the node's own kube-proxy programming.
  wait_path() {
    local label="$1"; shift
    while ! "$@" >/dev/null 2>&1; do
      if (( SECONDS >= converge_deadline )); then
        echo "  gave up waiting for $label" >&2
        return 1
      fi
      sleep 10
    done
  }
  # Every ordered pair, not a sample through the first node. The pairs
  # the sample missed are exactly the slowest ones: a transit pair
  # (neither end holds a tunnel; the path goes through a relay) after
  # a placement shrink converges on the rehoming evidence's schedule,
  # minutes, not seconds, and a gate that never probed it left the
  # checks' own short retries to eat that convergence. Measured:
  # endpoints moved from all to w1,w2, and the two transit pairs
  # (cp2 to remote2, cp3 to remote1) were the 2 of 112 still
  # converging when the checks ran. Once converged, this is n-squared
  # fast probes; while converging, it is the wait the measurement
  # needs to mean anything.
  for src in "${NODES[@]}"; do
    for dst in "${NODES[@]}"; do
      [[ "$src" == "$dst" ]] && continue
      wait_path "$src to reach $dst" \
        http_from_early "$src" "http://${POD_IP[$dst]}:$SERVICE_PORT/"
    done
    # One service-address path per source: the service hop beyond the
    # pod path is the source node's own kube-proxy programming, which
    # does not vary by destination.
    if [[ "$src" != "$first" ]]; then
      wait_path "$src to reach $first by service address" \
        http_from_early "$src" "http://${SERVICE_IP[$first]}:$SERVICE_PORT/"
    fi
  done
  # Cluster DNS too, per node, because the checks measure it and a
  # check that is not gated eats convergence out of its own single
  # try. This is the one path class with cluster-side convergence in
  # it: a DNS replica on a node that died ungracefully stays in the
  # service's endpoints until the node lifecycle controller marks that
  # node's pods NotReady, and until then the service address is a coin
  # flip. That is Kubernetes' documented window, not this mesh's, and
  # the checks below assert recovery, not luck. Measured: cp died
  # ungracefully, pods and services converged, and three nodes' DNS
  # lost the flip inside the window.
  for n in "${NODES[@]}"; do
    wait_path "$n to resolve cluster DNS" \
      pod_exec "$n" nslookup kubernetes.default.svc.cluster.local
  done
  echo "  converged after $((SECONDS - converge_start))s"
fi

PASS=0
FAIL=0
# Reachability across a tunnel converges: a node is given its pod block
# when the first pod lands on it, and the block reaches the peers on the
# next pass. So each check is retried to a deadline rather than taken as
# final on its first attempt.
RETRY_SECONDS="${HEALTH_CHECK_RETRY_SECONDS:-60}"
check() {
  local label="$1"; shift
  local deadline=$((SECONDS + RETRY_SECONDS))
  while true; do
    if "$@" >/dev/null 2>&1; then
      echo "  PASS  $label"; PASS=$((PASS+1)); return
    fi
    if (( SECONDS >= deadline )); then
      echo "  FAIL  $label"; FAIL=$((FAIL+1)); return
    fi
    sleep 5
  done
}


echo
echo "pod to pod, every pair"
for src in "${NODES[@]}"; do
  for dst in "${NODES[@]}"; do
    check "$src to $dst pod (${POD_IP[$dst]})" http_from "$src" "http://${POD_IP[$dst]}:$SERVICE_PORT/"
  done
done

echo
echo "pod to service, every pair, with no service range permitted on the tunnel"
for src in "${NODES[@]}"; do
  for dst in "${NODES[@]}"; do
    check "$src to $dst service (${SERVICE_IP[$dst]})" http_from "$src" "http://${SERVICE_IP[$dst]}:$SERVICE_PORT/"
  done
done

echo
echo "a ${BIG_KIB}KiB transfer, every pair: the segment size the tunnel clamps to"
for src in "${NODES[@]}"; do
  for dst in "${NODES[@]}"; do
    check "$src to $dst ${BIG_KIB}KiB (${POD_IP[$dst]})" big_from "$src" "http://${POD_IP[$dst]}:$SERVICE_PORT/big"
  done
done

echo
echo "cluster DNS, from every node"
for src in "${NODES[@]}"; do
  check "$src resolves kubernetes.default" \
    pod_exec "$src" nslookup kubernetes.default.svc.cluster.local
done

echo
echo "the path off the cluster, from every node"
for src in "${NODES[@]}"; do
  check "$src reaches 1.1.1.1" \
    pod_exec "$src" ping -c1 -W3 1.1.1.1
done

echo
echo "checks: $((PASS+FAIL))  passed: $PASS  failed: $FAIL"
[[ "$REPORT_ONLY" == "1" ]] && exit 0
[[ $FAIL -eq 0 ]] || exit 1
