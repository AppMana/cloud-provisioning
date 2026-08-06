#!/usr/bin/env bash
# Every pair of nodes, on the lab.
#
# The check runs from this host because it needs docker to reach into
# each node, and talks to the cluster through the bastion because this
# host cannot reach the API server and should not be able to. The shim
# is how those two facts live together.
set -uo pipefail
cd "$(dirname "$0")"

LAB=cldt
SHIM="$PWD/out/shim"
mkdir -p "$SHIM"
cat > "$SHIM/kubectl" <<'EOF'
#!/usr/bin/env bash
exec docker exec -i clab-cldt-bastion kubectl "$@"
EOF
chmod +x "$SHIM/kubectl"
export PATH="$SHIM:$PATH"

NODES="${NODES:-$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v '^$')}"
[ -n "$NODES" ] || { echo "FAIL: no nodes" >&2; exit 1; }

echo "--- reachability across every pair ---"
HEALTH_CHECK_EXEC=node HEALTH_CHECK_NODE_PREFIX="clab-$LAB-" \
  ../health-check.sh --exec node ${HEALTH_CHECK_ARGS:-} $NODES
