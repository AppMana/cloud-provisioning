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

NAMESPACE="cloud-provisioning-health"
SERVICE_PORT=8080
IMAGE="${HEALTH_CHECK_IMAGE:-busybox:1.37}"
NODES=()

while [[ $# -gt 0 ]]; do
  case $1 in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --service-port) SERVICE_PORT="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
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

kubectl create namespace "$NAMESPACE" >/dev/null 2>&1 || true

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
      command: ["sh","-c","mkdir -p /tmp/www; printf ok > /tmp/www/index.html; httpd -f -p $SERVICE_PORT -h /tmp/www"]
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

# A node that has just joined has to be given its pod block, have that
# block reach the peers, and have the network distribute a route for it.
# That is convergence, not flakiness, so it is waited for once here
# rather than folded into every check's timeout.
http_from_early() {
  kubectl exec "hc-$1" -n "$NAMESPACE" -- wget -q -T 5 -O - "$2" 2>/dev/null | grep -q ok
}
if [[ ${#NODES[@]} -gt 1 ]]; then
  first="${NODES[0]}"
  echo
  echo "waiting for reachability to converge between nodes"
  converge_deadline=$((SECONDS + ${HEALTH_CHECK_CONVERGE_SECONDS:-420}))
  for dst in "${NODES[@]}"; do
    [[ "$dst" == "$first" ]] && continue
    while ! http_from_early "$first" "http://${POD_IP[$dst]}:$SERVICE_PORT/"; do
      if (( SECONDS >= converge_deadline )); then
        echo "  gave up waiting for $first to reach $dst" >&2
        break
      fi
      sleep 10
    done
  done
  echo "  converged after $((SECONDS))s"
fi

PASS=0
FAIL=0
# Reachability across a tunnel converges: a node is given its pod block
# when the first pod lands on it, and the block reaches the peers on the
# next pass. So each check is retried to a deadline rather than taken as
# final on its first attempt.
RETRY_SECONDS="${HEALTH_CHECK_RETRY_SECONDS:-90}"
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

http_from() {
  local src="$1" url="$2"
  kubectl exec "hc-${src}" -n "$NAMESPACE" -- wget -q -T 5 -O - "$url" 2>/dev/null | grep -q ok
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
echo "cluster DNS, from every node"
for src in "${NODES[@]}"; do
  check "$src resolves kubernetes.default" \
    kubectl exec "hc-${src}" -n "$NAMESPACE" -- nslookup kubernetes.default.svc.cluster.local
done

echo
echo "the path off the cluster, from every node"
for src in "${NODES[@]}"; do
  check "$src reaches 1.1.1.1" \
    kubectl exec "hc-${src}" -n "$NAMESPACE" -- ping -c1 -W3 1.1.1.1
done

echo
echo "checks: $((PASS+FAIL))  passed: $PASS  failed: $FAIL"
[[ $FAIL -eq 0 ]] || exit 1
