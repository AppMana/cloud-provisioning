#!/usr/bin/env bash
# Every tunnel placement in scenarios.tsv, each on its own cluster.
#
# The rows are the specification; this file only knows how to run one.
# A row names roles rather than nodes, so the selector it turns into
# depends on the cluster the row is running on, and two rows never
# share a cluster: they have collided before, one run's teardown
# deleting another run's cluster mid flight.
#
# Each row asserts total reachability. The health check reports rather
# than exits on the first failure, so the whole matrix is visible, and
# this reads its count and fails the row if anything did not pass. A
# report nobody asserts on is how a broken configuration stayed green
# before.
set -uo pipefail
cd "$(dirname "$0")"

OUT="${OUT:-/tmp/cldt-matrix}"
SCENARIOS="${SCENARIOS:-scenarios.tsv}"
[ -r "$SCENARIOS" ] || { echo "cannot read $SCENARIOS" >&2; exit 2; }
ONLY="${ONLY:-}"          # run just these rows, space separated
mkdir -p "$OUT"
: > "$OUT/summary.txt"

# selector_for CLUSTER ROLES -> a label selector, or the word "all"
selector_for() {
  local cluster="$1" roles="$2"
  case "$roles" in
    all) echo "all"; return ;;
    control-plane) echo "node-role.kubernetes.io/control-plane="; return ;;
  esac
  # One or more workers, named by the role's position in the cluster.
  local out="" role
  for role in ${roles//,/ }; do
    out+="${out:+,}${cluster}-${role}"
  done
  # kubernetes.io/hostname takes one value, so several nodes need a set.
  if [[ "$out" == *,* ]]; then
    echo "kubernetes.io/hostname in (${out//,/,})"
  else
    echo "kubernetes.io/hostname=$out"
  fi
}

rows=0
failed_rows=0
while IFS=$'\t' read -r name endpoints remotes cni; do
  # Comments start with > so the file stays a table anything can read.
  [[ "$name" == \>* || -z "$name" || "$name" == "name" ]] && continue
  [[ -n "$ONLY" && " $ONLY " != *" $name "* ]] && continue

  cluster="cldt-$name"
  selector=$(selector_for "$cluster" "$endpoints")
  rows=$((rows + 1))

  echo
  echo "================ $name: endpoints=$endpoints remotes=$remotes cni=$cni ================"
  echo "  cluster $cluster, selector $selector"

  # Nothing shares a cluster name, so this only ever removes leftovers
  # from a previous run of this same row.
  docker ps -aq --filter "name=$cluster" | xargs -r docker rm -f >/dev/null 2>&1 || true
  kind delete cluster --name "$cluster" >/dev/null 2>&1 || true

  CLUSTER="$cluster" \
  TUNNEL_ENDPOINTS="$selector" \
  REMOTE_COUNT="$remotes" \
  CNI="$cni" \
  HEALTH_CHECK_ARGS="--report-only" \
  LOG_DIR="$OUT/$name" \
    bash run-live.sh > "$OUT/$name.log" 2>&1
  status=$?

  counts=$(grep -E "^checks:" "$OUT/$name.log" | tail -1)
  n_failed=$(sed -n 's/.*failed: \([0-9]*\).*/\1/p' <<< "$counts")
  verdict="FAIL"
  if [[ "$status" -eq 0 && -n "$n_failed" && "$n_failed" -eq 0 ]]; then
    verdict="PASS"
  else
    failed_rows=$((failed_rows + 1))
  fi

  {
    echo "### $verdict $name (endpoints=$endpoints, remotes=$remotes, cni=$cni) exit=$status"
    echo "    ${counts:-no checks ran}"
    grep -E "^  FAIL  " "$OUT/$name.log" 2>/dev/null | sed 's/^/    /'
    grep -E "^FAIL: " "$OUT/$name.log" 2>/dev/null | tail -1 | sed 's/^/    /'
    echo
  } >> "$OUT/summary.txt"
  echo "  $verdict ${counts:-no checks ran}"
done < "$SCENARIOS"

echo
echo "================ summary ================"
cat "$OUT/summary.txt"
echo "rows: $rows  failed: $failed_rows"
# Zero rows is not success. A mistyped ONLY, an unreadable table or a
# filter that matches nothing would otherwise report a green matrix
# having provisioned nothing at all.
[[ "$rows" -gt 0 && "$failed_rows" -eq 0 ]]
