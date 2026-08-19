#!/usr/bin/env bash
# Every row in distros.tsv: the same lab, rebuilt as each distribution
# in turn, with that distribution's suite run on it.
#
# The distribution is an outer loop, not a fourth column in
# scenarios.tsv, because a distro change rebuilds the site cluster and
# the placement rows are defined by never doing that. Each row here
# is: topology up from scratch, the site built as the row's
# distribution (cluster.d/<distro>.sh), the product installed with the
# matching join provider, the first cloud joined through the product,
# then the row's placement and outage subsets, and finally the
# mechanism assertions: who balances the API path, checked on the live
# remotes rather than trusted from the README's table.
#
# A row passes only if every stage passed. Zero rows is not success.
set -uo pipefail
cd "${DMATRIX_DIR:-$(dirname "$0")}"
export DMATRIX_DIR="$PWD"

OUT="${OUT:-$PWD/out}"
DISTROS="${DISTROS:-distros.tsv}"
ONLY_DISTRO="${ONLY_DISTRO:-}"
[ -r "$DISTROS" ] || { echo "cannot read $DISTROS" >&2; exit 2; }
mkdir -p "$OUT/distro"
: > "$OUT/distro/summary.txt"

if [ "${DMATRIX_REEXEC:-}" != 1 ]; then
  # The same lock as matrix.sh and outage.sh, held here for the whole
  # run: the children are told it is already held (CLDT_LOCK_HELD) so
  # they do not contend with their own caller, and nothing else can
  # start a run between two of this driver's stages.
  exec 9>/tmp/cldt-matrix.lock
  flock -n 9 || { echo "another run holds the lock; wait for it or kill it" >&2; exit 2; }
  # Run from a copy: bash reads a script as it goes, and editing this
  # file mid-run would have the running process execute the mixture.
  cp "$0" /tmp/cldt-distro-matrix-running.sh
  DMATRIX_REEXEC=1 exec bash /tmp/cldt-distro-matrix-running.sh "$@"
fi
export CLDT_LOCK_HELD=1

# Build and join stages get a deadline, because a stage that can hang
# holds the whole lab hostage: a join gate that never opens is a
# 12-hour wedge, not a wait (measured: cilium's classifier swallowing
# the tunnel's outers, the bootstrap gate looping on it overnight).
# The measurement stages run unbounded; their own drivers already
# bound every wait they contain.
stage() {
  local secs="$1" log="$2"; shift 2
  echo "  -- $*"
  local rc=0
  if [ "$secs" -gt 0 ]; then
    timeout "$secs" "$@" >"$log" 2>&1 || rc=$?
  else
    "$@" >"$log" 2>&1 || rc=$?
  fi
  if [ "$rc" -ne 0 ]; then
    [ "$rc" -eq 124 ] && echo "  TIMED OUT after ${secs}s" >>"$log"
    echo "  FAIL at: $* (see $log)" >&2
    tail -25 "$log" >&2
    return 1
  fi
}

rows=0; failed=0
while IFS=$'\t' read -r name cni placement outages <&3; do
  case "$name" in \>*|''|distro|name) continue ;; esac
  key="$name-$cni"
  # A bare distro name in the filter matches all of its network rows;
  # the full key names one.
  [ -n "$ONLY_DISTRO" ] && case " $ONLY_DISTRO " in *" $key "*|*" $name "*) ;; *) continue ;; esac

  rows=$((rows + 1))
  export DISTRO="$name" CNI="$cni"
  log="$OUT/distro/$key"
  echo
  echo "================ $key: placement=[$placement] outages=[$outages] ================"

  verdict=FAIL; why=
  while :; do
    stage 900  "$log-up.log"        bash up.sh                       || { why="the topology"; break; }
    stage 1800 "$log-cluster.log"   bash cluster.sh                  || { why="the site cluster"; break; }
    stage 1800 "$log-install.log"   bash install.sh                  || { why="the install"; break; }
    stage 600  "$log-claim.log"     bash claim.sh remote1 remote1 203.0.113.10 \
                                                                     || { why="the claim"; break; }
    stage 1800 "$log-bootstrap.log" bash bootstrap.sh remote1 remote1 203.0.113.10 \
                                                                     || { why="the first cloud's join"; break; }

    # "full" is every row in the file; the children treat an empty
    # ONLY the same way.
    p_only="$placement"; [ "$placement" = full ] && p_only=""
    o_only="$outages";   [ "$outages"   = full ] && o_only=""

    stage 0 "$log-placement.log" env ONLY="$p_only" bash matrix.sh || { why="a placement row"; break; }
    cp "$OUT/matrix/summary.txt" "$log-placement-summary.txt" 2>/dev/null
    stage 0 "$log-outage.log"    env ONLY="$o_only" bash outage.sh || { why="an outage row"; break; }
    cp "$OUT/outage/summary.txt" "$log-outage-summary.txt" 2>/dev/null
    stage 600 "$log-mechanism.log" bash mechanism.sh                || { why="the mechanism assertions"; break; }

    verdict=PASS
    break
  done
  # The children's summaries survive a failed stage too; a row that
  # died at the outage stage still says which outage row it died in.
  cp "$OUT/matrix/summary.txt" "$log-placement-summary.txt" 2>/dev/null || true
  cp "$OUT/outage/summary.txt" "$log-outage-summary.txt"   2>/dev/null || true

  [ "$verdict" = FAIL ] && { failed=$((failed + 1)); echo "  FAIL $why failed"; }
  {
    echo "### $verdict $key"
    [ -n "$why" ] && echo "    failed at $why"
    echo
  } >> "$OUT/distro/summary.txt"
done 3< "$DISTROS"

echo
echo "================ summary ================"
cat "$OUT/distro/summary.txt"
echo "distros: $rows  failed: $failed"
# Zero rows is not success.
[ "$rows" -gt 0 ] && [ "$failed" -eq 0 ]
