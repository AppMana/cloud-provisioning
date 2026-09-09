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
# Two ways down, chosen per row (see outages.tsv). link: the NIC dies
# and the victim keeps running against it, a pulled cable, silence
# rather than a goodbye. reboot: the machine dies, SIGKILL, and comes
# back with only what a platform provides, so everything it was must
# be rebuilt by what it runs at boot. Anything that only works because
# the departing node said goodbye, or that only exists because someone
# once configured it by hand, is what this file exists to catch.
set -uo pipefail
cd "${OUTAGE_DIR:-$(dirname "$0")}"
export OUTAGE_DIR="$PWD"

LAB=cldt
NS=cloud-provisioning
OUT="${OUT:-$PWD/out}"
OUTAGES="${OUTAGES:-outages.tsv}"
ONLY="${ONLY:-}"
# The join provider follows the distribution the site was built as;
# rows change placement and take nodes down, never the pairing.
JOIN_PROVIDER="${JOIN_PROVIDER:-${DISTRO:-kubeadm}}"
[ -r "$OUTAGES" ] || { echo "cannot read $OUTAGES" >&2; exit 2; }
mkdir -p "$OUT/outage"
: > "$OUT/outage/summary.txt"

# Same lock as the matrix: two drivers changing tunnel placement under
# one cluster produce failures that look exactly like product faults.
if [ "${OUTAGE_REEXEC:-}" != 1 ]; then
  # A parent driver (distro-matrix.sh) holds this same lock across a
  # whole distribution's run and says so; contending with one's own
  # caller would deadlock every run it starts.
  if [ "${CLDT_LOCK_HELD:-}" != 1 ]; then
    exec 9>/tmp/cldt-matrix.lock
    flock -n 9 || { echo "another run holds the lock; wait for it or kill it" >&2; exit 2; }
  fi
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
    cp)  echo "node-role.kubernetes.io/control-plane" ;;
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

# Each node's single NIC: the segment bridge it hangs off, the
# containerlab peer name on that bridge, its address, and its gateway.
# The same facts up.sh plumbs at deploy, needed again because a
# rebooted container gets a fresh network namespace and its veth dies
# with the old one.
node_net() {
  case "$1" in
    cp)      echo "cldt-lan lan-cp 10.10.0.10/24 10.10.0.1" ;;
    cp2)     echo "cldt-lan lan-cp2 10.10.0.13/24 10.10.0.1" ;;
    cp3)     echo "cldt-lan lan-cp3 10.10.0.14/24 10.10.0.1" ;;
    w1)      echo "cldt-lan lan-w1 10.10.0.11/24 10.10.0.1" ;;
    w2)      echo "cldt-lan lan-w2 10.10.0.12/24 10.10.0.1" ;;
    remote1) echo "cldt-cloud-a a-remote1 203.0.113.10/24 203.0.113.1" ;;
    remote2) echo "cldt-cloud-b b-remote2 192.0.2.10/24 192.0.2.1" ;;
    *) return 1 ;;
  esac
}

# The way down. A link outage takes the NIC and leaves everything
# running against it; a reboot takes the machine, SIGKILL, because a
# power loss does not say goodbye and anything that only works after a
# goodbye is what these rows exist to catch.
take_down() {
  local victim="$1" mode="$2"
  case "$mode" in
    link)   in_node "$victim" ip link set eth1 down ;;
    reboot) docker kill "$(c "$victim")" >/dev/null ;;
  esac
}

# The way back from an outage, in full, on every path out of a row.
#
# link: the NIC returns, then the main-table routes the kernel dropped
# at link-down and will not restore on its own, the default among
# them. The site dials out to the remotes, so a victim with its link
# up but no default route cannot re-open a single tunnel, and every
# row after it inherits a cluster that never healed. This ran only on
# the happy path once, and the rows after a failed one measured that
# mistake instead of the product.
#
# reboot: the machine returns from scratch. Only what the platform
# would provide comes back by hand: the NIC (a fresh veth to the same
# segment, since the old one died with the old namespace), its
# address, and its gateway. Everything else -- the tunnel, the routes,
# the forwarding state, the node's membership -- must be rebuilt by
# what the node itself runs at boot, because that is the claim a
# reboot row makes.
restore_victim() {
  local name="$1" victim="$2" mode="${3:-link}"
  local bridge peer addr gw
  read -r bridge peer addr gw <<EOF
$(node_net "$victim")
EOF
  case "$mode" in
    link)
      in_node "$victim" ip link set eth1 up || return 1
      while read -r route; do
        case "$route" in *" proto kernel "*|"") continue ;; esac
        in_node "$victim" ip route replace $route 2>/dev/null
      done < "$OUT/outage/$name-routes"
      ;;
    reboot)
      # docker start on a freshly killed kind node fails transiently;
      # a machine that takes two tries to power on is still a machine
      # that powers on. Each step names itself on failure, because a
      # restore that fails silently costs a row and then hides which
      # of its four steps to fix.
      started=
      for _ in 1 2 3; do
        if [ "$(docker inspect -f '{{.State.Running}}' "$(c "$victim")" 2>/dev/null)" = "true" ] \
          || docker start "$(c "$victim")" >/dev/null 2>&1; then
          started=1
          break
        fi
        sleep 5
      done
      [ -n "$started" ] || { echo "  restore: docker start $(c "$victim") failed three times" >&2; return 1; }
      for _ in $(seq 1 24); do
        in_node "$victim" true 2>/dev/null && break
        sleep 5
      done
      if ! in_node "$victim" ip link show eth1 >/dev/null 2>&1; then
        # The old pair's host side can outlive the killed container,
        # and the re-plumb then fails renaming onto the name it still
        # holds ("failed to rename link: file exists", measured on
        # remote1). With the container's side gone, the leftover is
        # dead by definition, so it is removed by name first.
        sudo ip link del "$peer" 2>/dev/null || true
        sudo containerlab tools veth create -a "$(c "$victim"):eth1" -b "bridge:$bridge:$peer" >"$OUT/outage/$name-veth.log" 2>&1 \
          || { echo "  restore: veth re-plumb failed, see $OUT/outage/$name-veth.log" >&2; return 1; }
      fi
      in_node "$victim" ip addr replace "$addr" dev eth1 \
        || { echo "  restore: could not address eth1" >&2; return 1; }
      in_node "$victim" ip link set eth1 up
      # The pair's host side can be left down: a re-plumb that races
      # the old pair's deletion gets a generated name instead of the
      # requested one, and nothing raises it. Carrier is part of the
      # NIC a platform provides, so find the peer by ifindex, the one
      # identity a veth cannot lose, and raise it. Measured: cp came
      # back addressed, routed, UP, and NO-CARRIER, and its etcd
      # called elections into a wire that was not plugged in.
      peer_idx=$(in_node "$victim" cat /sys/class/net/eth1/iflink 2>/dev/null | tr -d '\r')
      if [ -n "$peer_idx" ]; then
        peer_if=$(ip -o link | awk -F': ' -v i="$peer_idx" '$1==i {print $2}' | cut -d@ -f1)
        [ -n "$peer_if" ] && sudo ip link set "$peer_if" up 2>/dev/null
      fi
      if ! in_node "$victim" ip link show eth1 2>/dev/null | grep -q "LOWER_UP"; then
        echo "  restore: eth1 has no carrier, the host-side peer is not up" >&2
        return 1
      fi
      in_node "$victim" ip route replace default via "$gw" dev eth1 \
        || { echo "  restore: could not restore the default route" >&2; return 1; }
      ;;
  esac
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

control_planes() {
  k get nodes -l node-role.kubernetes.io/control-plane \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$'
}

rows=0; failed=0
while IFS=$'\t' read -r name endpoints victim mode <&3; do
  mode="${mode:-link}"
  case "$name" in \>*|''|name) continue ;; esac
  [ -n "$ONLY" ] && case " $ONLY " in *" $name "*) ;; *) continue ;; esac
  # A control plane may die only where quorum survives it: stacked
  # etcd with fewer than three members loses the cluster with the
  # node, and a cluster with no API server is not a scenario, it is
  # the end of observation.
  if control_planes | grep -qx "$victim"; then
    cps=$(control_planes | wc -l)
    if [ "$cps" -lt 3 ]; then
      echo "refusing to take a control plane down with only $cps of them: quorum dies with it" >&2
      failed=$((failed + 1)); rows=$((rows + 1)); continue
    fi
  fi

  selector=$(selector_for "$endpoints")
  rows=$((rows + 1))
  echo
  echo "================ $name: endpoints=$endpoints, victim=$victim, mode=$mode ================"

  in_node bastion helm upgrade --install cloud-provisioning /tmp/chart \
    --namespace "$NS" --create-namespace --wait --timeout 6m \
    --set image.repository=cldt-controller --set image.tag=e2e --set image.pullPolicy=Never \
    --set dialerImage.repository=cldt-dialer --set dialerImage.tag=e2e \
    --set-string tunnel.endpoints="${selector//,/\\,}" \
    --set joinProvider="$JOIN_PROVIDER" \
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

    echo "  taking $victim down ($mode)"
    take_down "$victim" "$mode" || { why="could not take $victim down"; break; }
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
    restore_victim "$name" "$victim" "$mode" || { why="could not bring $victim back"; break; }
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
  # the machine, the link, and the routes, or the next row starts on a
  # cluster this one broke.
  restore_victim "$name" "$victim" "$mode" 2>/dev/null
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
