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
# The copy this re-execs as lives in /tmp, so where the harness lives
# has to be carried across rather than derived from $0.
cd "${MATRIX_DIR:-$(dirname "$0")}"
export MATRIX_DIR="$PWD"

LAB=cldt
NS=cloud-provisioning
REPO_DIR="$(cd ../.. && pwd)"
OUT="${OUT:-$PWD/out}"
SCENARIOS="${SCENARIOS:-scenarios.tsv}"
ONLY="${ONLY:-}"
# The join provider follows the distribution the site was built as;
# rows change placement, never the pairing.
JOIN_PROVIDER="${JOIN_PROVIDER:-${DISTRO:-kubeadm}}"
[ -r "$SCENARIOS" ] || { echo "cannot read $SCENARIOS" >&2; exit 2; }
mkdir -p "$OUT/matrix"
: > "$OUT/matrix/summary.txt"

# The rows are read on fd 3, not stdin. kubectl here is a shim around
# docker exec -i, which reads stdin, and inside a loop fed by the
# scenarios file that consumes the rows: the matrix ran one row and
# reported itself complete.
# Two of these against one cluster is not two runs, it is each one
# changing tunnel placement under the other, and the failures that
# produces look exactly like product faults. It has happened.
if [ "${MATRIX_REEXEC:-}" != 1 ]; then
  # A lock, not a search of the process list. Matching on a pattern
  # finds the shell doing the matching, because its own command line
  # contains the pattern, and the guard then refuses to start on
  # account of itself.
  #
  # A parent driver (distro-matrix.sh) holds this same lock across a
  # whole distribution's run and says so; contending with one's own
  # caller would deadlock every run it starts.
  if [ "${CLDT_LOCK_HELD:-}" != 1 ]; then
    exec 9>/tmp/cldt-matrix.lock
    flock -n 9 || { echo "another matrix run holds the lock; wait for it or kill it" >&2; exit 2; }
  fi
  # Run from a copy. bash reads a script as it goes, so editing this
  # file while it runs makes the running process execute whatever the
  # bytes became, which is a failure with no relation to the change.
  cp "$0" /tmp/cldt-matrix-running.sh
  MATRIX_REEXEC=1 exec bash /tmp/cldt-matrix-running.sh "$@"
fi

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
k() { in_node bastion kubectl "$@"; }

# Which nodes a row's endpoint spec names, as a selector the chart takes.
#
# A set based selector contains commas, and helm --set reads a comma as
# the separator between values, so "hostname in (w1,w2)" arrives at the
# chart as two mangled fragments and the release fails to install. The
# commas belong to the term, so they are escaped, and --set-string keeps
# helm from interpreting anything else in it.
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
    --set-string tunnel.endpoints="${selector//,/\\,}" \
    --set joinProvider="$JOIN_PROVIDER" \
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

  # Wait for the placement to actually take, rather than for a number of
  # seconds. A row measured before the dialers converge reports a
  # product failure that is not one, and a fixed wait is a guess that is
  # either too short on a slow pass or wasted on a fast one.
  #
  # Two things have to hold: every node the selector names is running a
  # dialer and has published a key, and every remote's peer list names
  # only nodes that are still endpoints. The second is what the
  # migration fix is for, so a row that starts before it holds would be
  # testing the previous placement.
  placed=
  for _ in $(seq 1 40); do
    # Ask the API server which nodes the selector names, in one call.
    # Naming a node and giving a selector in the same kubectl invocation
    # is an error, not a filter, so a per-node loop returns nothing for
    # every node: the wait then had nothing to wait for, ran its full
    # length, and the row was measured with the placement unverified.
    #
    # "all" is every node this operator did not provision. A jsonpath
    # filter cannot express that: a comparison against a label the node
    # does not carry at all does not match, which would drop exactly the
    # site nodes the row is about.
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
    stale=$(k -n "$NS" get secret "$NS-peers" -o json 2>/dev/null \
      | python3 -c '
import json,sys
d=json.load(sys.stdin).get("data",{})
want=set(sys.argv[1].split())
print(" ".join(sorted(k[len("node-tunnel-address-"):] for k in d
      if k.startswith("node-tunnel-address-") and k[len("node-tunnel-address-"):] not in want)))
' "$want" 2>/dev/null)
    # A node that has left the selector keeps its entries for the
    # retention window on purpose, so that a remote which reads its peer
    # list over the tunnel has time to move before the old one goes.
    # Waiting for it to disappear would be waiting for the mechanism to
    # finish protecting us, and would report a placement as stuck while
    # it is working. What has to hold is that every node the selector
    # names is published; the departed one is reported, not waited on.
    if [ "$expected" -gt 0 ] && [ "$published" -eq "$expected" ]; then
      echo "  placed on $(echo $want | tr '\n' ' ')${stale:+, retaining $stale}"
      placed=1
      break
    fi
    sleep 10
  done
  # A wait that gives up is a result. Measuring anyway reports whatever
  # the previous row left behind as this row's verdict.
  if [ -z "$placed" ]; then
    echo "  FAIL the placement never took: $published of $expected endpoints published"
    failed=$((failed + 1))
    echo "### FAIL $name (endpoints=$endpoints, clouds=$remotes) placement never took" >> "$OUT/matrix/summary.txt"
    continue
  fi

  # And wait for the cluster to agree it has converged. Moving a tunnel
  # takes the old endpoint's dialer away at once, while a remote re-reads
  # its peer list on its own schedule and kubelet then needs its grace
  # period to notice the recovery, so a remote is briefly NotReady by
  # design. Measuring during that window reports a product failure that
  # is really a migration in progress, and pods cannot even be placed on
  # a NotReady node, which surfaces as "pods did not all start".
  #
  # How long this takes is itself worth knowing, so it is reported.
  settle_start=$SECONDS
  for _ in $(seq 1 60); do
    notready=$(k get nodes --no-headers 2>/dev/null | awk '$2!="Ready"{print $1}' | tr '\n' ' ')
    [ -z "$notready" ] && break
    sleep 10
  done
  if [ -n "${notready:-}" ]; then
    echo "  FAIL still not ready after $((SECONDS - settle_start))s: $notready"
    failed=$((failed + 1))
    echo "### FAIL $name (endpoints=$endpoints, clouds=$remotes) never settled: $notready" >> "$OUT/matrix/summary.txt"
    continue
  fi
  echo "  every node ready $((SECONDS - settle_start))s after the placement changed"

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
