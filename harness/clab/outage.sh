#!/usr/bin/env bash
# Every row in outages.tsv, on the lab that is already standing.
#
# A row is three claims in sequence. The placement works: the full
# matrix is green before anything breaks, because a failure measured on
# a broken baseline names the wrong culprit. The survivors recover: with
# the victim's link down, every pair among the remaining nodes passes
# within the health check's convergence window. The victim returns: link
# back up, the whole cluster is green again, with no one needing to be
# reinstalled, restarted, or forgiven.
#
# The victim's link goes down; the victim does not. Its kubelet, dialer,
# and containerd keep running against a dead NIC, which is what a power
# event, a pulled cable, or a dead switch looks like from every other
# node: silence, not a goodbye. Anything that only works because the
# departing node said goodbye is what this file exists to catch.
set -uo pipefail
cd "${OUTAGE_DIR:-$(dirname "$0")}"
export OUTAGE_DIR="$PWD"

LAB=cldt
NS=cloud-provisioning
OUT="${OUT:-$PWD/out}"
OUTAGES="${OUTAGES:-outages.tsv}"
ONLY="${ONLY:-}"
[ -r "$OUTAGES" ] || { echo "cannot read $OUTAGES" >&2; exit 2; }
mkdir -p "$OUT/outage"
: > "$OUT/outage/summary.txt"

# Same lock as the matrix: two drivers changing tunnel placement under
# one cluster produce failures that look exactly like product faults.
if [ "${OUTAGE_REEXEC:-}" != 1 ]; then
  exec 9>/tmp/cldt-matrix.lock
  flock -n 9 || { echo "another run holds the lock; wait for it or kill it" >&2; exit 2; }
  # Run from a copy: bash reads a script as it goes, and editing this
  # file mid-run would have the running process execute the mixture.
  cp "$0" /tmp/cldt-outage-running.sh
  OUTAGE_REEXEC=1 exec bash /tmp/cldt-outage-running.sh "$@"
fi

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
k() { in_node bastion kubectl "$@"; }

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

# The placement wait from matrix.sh: every node the selector names has
# published a key and an address, or the row is measuring the previous
# placement.
wait_placed() {
  local selector="$1" want published expected key addr n
  for _ in $(seq 1 40); do
    case "$selector" in
      all) want=$(k get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin)["items"]:
    if n["metadata"].get("labels",{}).get("cloud-provisioning.appmana.com/role") != "cloud-worker":
        print(n["metadata"]["name"])
') ;;
      *)   want=$(k get nodes -l "$selector" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null) ;;
    esac
    want=$(echo "$want" | grep -v '^$')
    published=0; expected=0
    for n in $want; do
      expected=$((expected + 1))
      key=$(k -n "$NS" get secret "$NS-peers" -o jsonpath="{.data.node-public-key-$n}" 2>/dev/null)
      addr=$(k -n "$NS" get secret "$NS-peers" -o jsonpath="{.data.node-tunnel-address-$n}" 2>/dev/null)
      [ -n "$key" ] && [ -n "$addr" ] && published=$((published + 1))
    done
    if [ "$expected" -gt 0 ] && [ "$published" -eq "$expected" ]; then
      echo "  placed on $(echo $want | tr '\n' ' ')"
      return 0
    fi
    sleep 10
  done
  echo "  placement never took: $published of $expected endpoints published"
  return 1
}

wait_ready() {
  local notready
  for _ in $(seq 1 60); do
    notready=$(k get nodes --no-headers 2>/dev/null | awk '$2!="Ready"{print $1}' | tr '\n' ' ')
    [ -z "$notready" ] && return 0
    sleep 10
  done
  echo "  still not ready: $notready"
  return 1
}

# The way back from an outage, in full: link up, then the main-table
# routes the kernel dropped at link-down and will not restore on its
# own, the default among them. The site dials out to the remotes, so a
# victim with its link up but no default route cannot re-open a single
# tunnel, and every row after it inherits a cluster that never healed.
# This ran only on the happy path once, and the rows after a failed one
# measured that mistake instead of the product.
restore_victim() {
  local name="$1" victim="$2"
  in_node "$victim" ip link set eth1 up || return 1
  while read -r route; do
    case "$route" in *" proto kernel "*|"") continue ;; esac
    in_node "$victim" ip route replace $route 2>/dev/null
  done < "$OUT/outage/$name-routes"
  return 0
}

# One health-check pass over the named nodes; green means ran and
# nothing failed. The check's own convergence gate is the recovery
# window: it waits for the paths before measuring, so a pass that
# arrives is also a bound on how long recovery took, reported in its
# log as "converged after Ns".
check() {
  local log="$1"; shift
  NODES="$*" HEALTH_CHECK_ARGS=--report-only bash run.sh > "$log" 2>&1
  local counts n_ran n_failed
  counts=$(grep -E "^checks:" "$log" | tail -1)
  n_ran=$(sed -n 's/^checks: \([0-9]*\).*/\1/p' <<< "$counts")
  n_failed=$(sed -n 's/.*failed: \([0-9]*\).*/\1/p' <<< "$counts")
  echo "${counts:-no checks ran}"
  [ -n "$n_ran" ] && [ "$n_ran" -gt 0 ] && [ "${n_failed:-1}" -eq 0 ]
}

rows=0; failed=0
while IFS=$'\t' read -r name endpoints victim <&3; do
  case "$name" in \>*|''|name) continue ;; esac
  [ -n "$ONLY" ] && case " $ONLY " in *" $name "*) ;; *) continue ;; esac
  if [ "$victim" = "cp" ]; then
    echo "refusing to take the control plane down: one API server is the lab's eyes" >&2
    failed=$((failed + 1)); rows=$((rows + 1)); continue
  fi

  selector=$(selector_for "$endpoints")
  rows=$((rows + 1))
  echo
  echo "================ $name: endpoints=$endpoints, victim=$victim ================"

  in_node bastion helm upgrade --install cloud-provisioning /tmp/chart \
    --namespace "$NS" --create-namespace --wait --timeout 6m \
    --set image.repository=cldt-controller --set image.tag=e2e --set image.pullPolicy=Never \
    --set dialerImage.repository=cldt-dialer --set dialerImage.tag=e2e \
    --set-string tunnel.endpoints="${selector//,/\\,}" \
    --set joinProvider=kubeadm \
    --set dialerBinary.amd64.url="file:///opt/dialer-dist/wg-dialer-linux-amd64" \
    --set dialerBinary.amd64.sha256="$BIN_SHA" >/dev/null 2>&1 \
    || { echo "  FAIL could not place the tunnels"; failed=$((failed+1)); continue; }

  verdict=FAIL; why=
  while :; do
    wait_placed "$selector" || { why="placement never took"; break; }
    wait_ready || { why="never settled before the outage"; break; }

    echo "  baseline, all nodes:"
    if ! out=$(check "$OUT/outage/$name-baseline.log"); then
      why="baseline was already broken: $out"; break
    fi
    echo "    $out"

    all_nodes=$(k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v '^$')
    survivors=$(echo "$all_nodes" | grep -vx "$victim" | tr '\n' ' ')

    # The victim's main-table routes, for the way back: taking a link
    # down deletes them, bringing it up restores only the connected
    # ones. The dialer's own table and the CNI's routes are their
    # owners' jobs to restore, and that they restore them is part of
    # what the return leg asserts.
    in_node "$victim" ip -4 route show > "$OUT/outage/$name-routes" 2>/dev/null

    echo "  taking $victim's link down"
    in_node "$victim" ip link set eth1 down || { why="could not take the link down"; break; }
    down_at=$SECONDS

    # The cluster must notice, or nothing that follows measures an
    # outage. Kubelet's lease has to expire; the default is under a
    # minute, and two minutes of still-Ready means the lab is not
    # wired the way this file believes.
    noticed=
    for _ in $(seq 1 24); do
      status=$(k get node "$victim" --no-headers 2>/dev/null | awk '{print $2}')
      case "$status" in *NotReady*) noticed=1; break ;; esac
      sleep 5
    done
    [ -n "$noticed" ] || { why="$victim never went NotReady, so no outage was measured"; break; }
    echo "  $victim NotReady $((SECONDS - down_at))s after the link dropped"

    echo "  survivors: $survivors"
    if ! out=$(check "$OUT/outage/$name-survivors.log" $survivors); then
      why="the survivors did not recover: $out"; break
    fi
    echo "    $out"
    grep -E "converged after" "$OUT/outage/$name-survivors.log" | sed 's/^/    /'

    echo "  bringing $victim back"
    restore_victim "$name" "$victim" || { why="could not bring the link back"; break; }
    wait_ready || { why="$victim never came back"; break; }
    echo "  $victim Ready again"

    echo "  full matrix after the return:"
    if ! out=$(check "$OUT/outage/$name-return.log"); then
      why="the cluster did not return to green: $out"; break
    fi
    echo "    $out"
    verdict=PASS
    break
  done

  # Whatever happened, never leave the victim dark for the next row:
  # the link and the routes both, or the next row starts on a cluster
  # this one broke.
  restore_victim "$name" "$victim" 2>/dev/null
  if [ "$verdict" = FAIL ]; then
    failed=$((failed + 1))
    echo "  FAIL $why"
  fi
  {
    echo "### $verdict $name (endpoints=$endpoints, victim=$victim)"
    [ -n "$why" ] && echo "    $why"
    grep -E "^  FAIL  " "$OUT/outage/$name-survivors.log" "$OUT/outage/$name-return.log" 2>/dev/null | sed 's/^/    /'
    echo
  } >> "$OUT/outage/summary.txt"
done 3< "$OUTAGES"

echo
echo "================ summary ================"
cat "$OUT/outage/summary.txt"
echo "rows: $rows  failed: $failed"
# Zero rows is not success.
[ "$rows" -gt 0 ] && [ "$failed" -eq 0 ]
