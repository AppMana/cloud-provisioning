#!/usr/bin/env bash
# Every row in scenarios.tsv, on the lab that is already standing.
#
# A row changes which site nodes hold tunnels, which is a chart value,
# and how many clouds are on the far side, which is a claim. Neither
# needs the topology rebuilt, and rebuilding it per row would cost far
# more than it proves: what the rows differ in is placement, and the
# topology is the same topology.
#
# A row passes only if the check ran and nothing failed. A report nobody
# asserts on is how a broken configuration stays green.
set -uo pipefail
cd "$(dirname "$0")"

LAB=cldt
NS=cloud-provisioning
REPO_DIR="$(cd ../.. && pwd)"
OUT="${OUT:-$PWD/out}"
SCENARIOS="${SCENARIOS:-scenarios.tsv}"
ONLY="${ONLY:-}"
[ -r "$SCENARIOS" ] || { echo "cannot read $SCENARIOS" >&2; exit 2; }
mkdir -p "$OUT/matrix"
: > "$OUT/matrix/summary.txt"

# The rows are read on fd 3, not stdin. kubectl here is a shim around
# docker exec -i, which reads stdin, and inside a loop fed by the
# scenarios file that consumes the rows: the matrix ran one row and
# reported itself complete.
c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
k() { in_node bastion kubectl "$@"; }

# Which nodes a row's endpoint spec names, as a selector the chart takes.
selector_for() {
  case "$1" in
    all) echo "all" ;;
    cp)  echo "node-role.kubernetes.io/control-plane=" ;;
    *)   case "$1" in
           *,*) echo "kubernetes.io/hostname in (${1})" ;;
           *)   echo "kubernetes.io/hostname=$1" ;;
         esac ;;
  esac
}

BIN_SHA=$(sha256sum "$OUT/binaries/wg-dialer-linux-amd64" 2>/dev/null | awk '{print $1}')
[ -n "$BIN_SHA" ] || { echo "no dialer binary; run install.sh first" >&2; exit 2; }

rows=0; failed=0
while IFS=$'\t' read -r name endpoints remotes <&3; do
  case "$name" in \>*|''|name) continue ;; esac
  [ -n "$ONLY" ] && case " $ONLY " in *" $name "*) ;; *) continue ;; esac

  selector=$(selector_for "$endpoints")
  rows=$((rows + 1))
  echo
  echo "================ $name: endpoints=$endpoints ($selector) clouds=$remotes ================"

  in_node bastion helm upgrade --install cloud-provisioning /tmp/chart \
    --namespace "$NS" --create-namespace --wait --timeout 6m \
    --set image.repository=cldt-controller --set image.tag=e2e --set image.pullPolicy=Never \
    --set dialerImage.repository=cldt-dialer --set dialerImage.tag=e2e \
    --set tunnel.endpoints="$selector" \
    --set joinProvider=kubeadm \
    --set dialerBinary.amd64.url="file:///opt/dialer-dist/wg-dialer-linux-amd64" \
    --set dialerBinary.amd64.sha256="$BIN_SHA" >/dev/null 2>&1 \
    || { echo "  FAIL could not place the tunnels"; failed=$((failed+1)); continue; }

  # The second cloud joins once and stays. Rows are ordered so this only
  # ever grows.
  if [ "$remotes" -ge 2 ] && ! k get node remote2 >/dev/null 2>&1; then
    bash claim.sh remote2 remote2 192.0.2.10 >"$OUT/matrix/$name-claim.log" 2>&1 \
      && bash bootstrap.sh remote2 remote2 192.0.2.10 >"$OUT/matrix/$name-bootstrap.log" 2>&1 \
      || { echo "  FAIL cloud B never joined"; failed=$((failed+1)); continue; }
  fi

  # The dialers reconverge on their own schedule after the placement
  # changes; the check's own convergence wait covers the rest.
  sleep 45

  HEALTH_CHECK_ARGS=--report-only bash run.sh > "$OUT/matrix/$name.log" 2>&1
  counts=$(grep -E "^checks:" "$OUT/matrix/$name.log" | tail -1)
  n_failed=$(sed -n 's/.*failed: \([0-9]*\).*/\1/p' <<< "$counts")
  n_ran=$(sed -n 's/^checks: \([0-9]*\).*/\1/p' <<< "$counts")
  if [ -n "$n_ran" ] && [ "$n_ran" -gt 0 ] && [ "${n_failed:-1}" -eq 0 ]; then
    verdict=PASS
  else
    verdict=FAIL; failed=$((failed + 1))
  fi
  echo "  $verdict ${counts:-no checks ran}"
  {
    echo "### $verdict $name (endpoints=$endpoints, clouds=$remotes)"
    echo "    ${counts:-no checks ran}"
    grep -E "^  FAIL  " "$OUT/matrix/$name.log" 2>/dev/null | sed 's/^/    /'
    echo
  } >> "$OUT/matrix/summary.txt"
done 3< "$SCENARIOS"

echo
echo "================ summary ================"
cat "$OUT/matrix/summary.txt"
echo "rows: $rows  failed: $failed"
# Zero rows is not success.
[ "$rows" -gt 0 ] && [ "$failed" -eq 0 ]
