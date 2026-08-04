#!/usr/bin/env bash
# Reachability against tunnel placement.
#
# Runs the end to end harness once per configuration, on a fresh cluster
# each time, and records which pairs of nodes could reach each other.
# The question it answers is what tunnel placement costs: a node that
# terminates no tunnel has no route to a remote node's address, so the
# route its CNI installed for that node's pods has an unresolvable next
# hop. That is true of Flannel and Cilium as much as Calico, since none
# of them can route to an address the underlay does not carry.
#
# The health check reports rather than fails here, because a pair being
# unreachable is the measurement, not an error.
set -uo pipefail
cd "$(dirname "$0")"
OUT="${OUT:-/tmp/cldt-matrix}"
mkdir -p "$OUT"

# Each row: name, endpoint selector, number of remote nodes.
CONFIGS=(
  "control-plane|node-role.kubernetes.io/control-plane=|1"
  "one-worker|kubernetes.io/hostname=cldt-live-worker|1"
  "all-nodes|all|1"
  "all-nodes-2-remotes|all|2"
  "one-worker-2-remotes|kubernetes.io/hostname=cldt-live-worker|2"
)

for row in "${CONFIGS[@]}"; do
  IFS='|' read -r name selector remotes <<< "$row"
  echo
  echo "================ $name: endpoints=$selector remotes=$remotes ================"

  # One at a time: every run uses the same cluster name, so an overlap
  # would have one run's teardown delete the other's cluster.
  while pgrep -f "^bash run-live.sh" >/dev/null; do sleep 10; done
  docker ps -aq --filter "name=cldt-live" | xargs -r docker rm -f >/dev/null 2>&1 || true
  kind delete cluster --name cldt-live >/dev/null 2>&1 || true

  TUNNEL_ENDPOINTS="$selector" \
  REMOTE_COUNT="$remotes" \
  HEALTH_CHECK_ARGS="--report-only" \
  CNI="${CNI:-calico}" \
  LOG_DIR="$OUT/$name" \
    bash run-live.sh > "$OUT/$name.log" 2>&1
  status=$?

  {
    echo "### $name (endpoints=$selector, remotes=$remotes) exit=$status"
    grep -E "^  (PASS|FAIL)  " "$OUT/$name.log" 2>/dev/null || echo "  (no checks ran)"
    grep -E "^checks:" "$OUT/$name.log" 2>/dev/null || true
    grep -E "^FAIL: " "$OUT/$name.log" 2>/dev/null | tail -1 || true
    echo
  } >> "$OUT/summary.txt"
  echo "recorded $name (exit $status)"
done

echo
echo "================ summary ================"
cat "$OUT/summary.txt"
