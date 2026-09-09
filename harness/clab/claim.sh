#!/usr/bin/env bash
# One claim becomes a node in a cloud, joined over a tunnel.
#
# The cluster objects and the template are applied the way an operator
# applies them, and the claim names the template. What is missing here
# and present in production is an infrastructure controller: nothing
# watches a ContainernetMachine, because containerlab already made the
# machine. So this fills in the two things such a controller would have
# reported, and nothing else: that the machine exists, and what address
# it has.
#
# Usage: claim.sh <name> <container> <cloud-address>
set -euo pipefail
cd "$(dirname "$0")"

NAME="${1:?claim name}"
CONTAINER="${2:?lab node}"
ADDRESS="${3:?the address the site reaches it at}"
NS="${NS:-cloud-provisioning}"
CLUSTER_NAME="${CLUSTER_NAME:-cldt}"
LAB=cldt

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
apply() { docker exec -i "$(c bastion)" kubectl apply -f - ; }
k() { in_node bastion kubectl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "--- the cluster objects, applied once per cluster ---"
# Marked managed-by external, which is what makes this operator, rather
# than Cluster API's controllers, responsible for reporting that the
# infrastructure is ready. No Cluster API controller runs here.
apply <<EOF >/dev/null
apiVersion: containernet.appmana.com/v1beta2
kind: ContainernetCluster
metadata:
  name: $CLUSTER_NAME
  namespace: $NS
spec: {}
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $CLUSTER_NAME
  namespace: $NS
  annotations:
    cluster.x-k8s.io/managed-by: external
spec:
  infrastructureRef:
    apiGroup: containernet.appmana.com
    kind: ContainernetCluster
    name: $CLUSTER_NAME
EOF

echo "--- a machine template, and one claim naming it ---"
apply <<EOF >/dev/null
apiVersion: containernet.appmana.com/v1beta2
kind: ContainernetMachineTemplate
metadata:
  name: $NAME
  namespace: $NS
spec:
  template:
    spec:
      # The machine is a container the topology already owns, so the
      # template says which one rather than how to build it.
      containerName: $(c "$CONTAINER")
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: $NAME
  namespace: $NS
spec:
  infrastructureRef:
    apiGroup: containernet.appmana.com
    kind: ContainernetMachineTemplate
    name: $NAME
  clusterName: $CLUSTER_NAME
EOF

echo "--- the claim produces a machine ---"
for _ in $(seq 1 60); do
  k -n "$NS" get containernetmachine "$NAME" >/dev/null 2>&1 && break
  sleep 5
done
k -n "$NS" get containernetmachine "$NAME" >/dev/null 2>&1 \
  || fail "the claim never produced a ContainernetMachine"
k -n "$NS" get machine "$NAME" >/dev/null 2>&1 || fail "no Machine for the claim"
echo "  ContainernetMachine and Machine exist"

echo "--- standing in for the infrastructure controller ---"
# Which container this machine is. The provider reads this to observe
# the machine, and the object's own name is the claim's, which a
# container name rarely matches.
k -n "$NS" annotate containernetmachine "$NAME" \
  "containernet.appmana.com/container-name=$(c "$CONTAINER")" --overwrite >/dev/null

# The address the site reaches it at, which is the one thing the mesh
# cannot derive: it is a property of the cloud the machine sits in. A
# provider that made the machine would report it; nothing here made it.
k -n "$NS" patch machine "$NAME" --subresource=status --type merge \
  -p "{\"status\":{\"addresses\":[{\"type\":\"ExternalIP\",\"address\":\"$ADDRESS\"},{\"type\":\"InternalIP\",\"address\":\"$ADDRESS\"}]}}" >/dev/null \
  || fail "reporting the machine's address"
echo "  $NAME is $(c "$CONTAINER") at $ADDRESS"

echo "--- the peer entry the site will dial ---"
for _ in $(seq 1 60); do
  ep=$(k -n "$NS" get secret "$NS-peers" -o jsonpath="{.data.peer-endpoint-$NAME}" 2>/dev/null | base64 -d 2>/dev/null || true)
  case "$ep" in ''|pending) sleep 5; continue ;; esac
  echo "  $ep"
  break
done
[ -n "${ep:-}" ] && [ "$ep" != pending ] || fail "the mesh never mirrored an endpoint for $NAME"
