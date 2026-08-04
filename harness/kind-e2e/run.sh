#!/usr/bin/env bash
# Declarative fan-out e2e on a real (kind) API server: apply ONE
# ProvisionedNodeClaim and assert everything the controller derives
# from it -- the AWSMachine + Machine pair with ownerRefs, the rendered
# bootstrap Secret (binary URL + pinned sha, unique cldt* interface,
# baked peers.json, machine-name file), the peer Secret entries, the
# adoption Secret, both dialer DaemonSets' scheduling shapes, and the
# full cascade on delete.
#
# What kind CANNOT do here: real CAPA (no instance), real BGP, real
# tunnels -- that needs a real cluster. The dialer's
# kernel behavior is covered by harness/netns-routing. The one
# simulated element: the dialer pod's key publication (the dialer image
# is in a private registry; its Secret-mode publication logic is
# unit-tested), done here with one kubectl patch of a locally-generated
# public key.
#
# CAPI/CAPA CRDs are shim definitions (x-kubernetes-preserve-unknown-fields):
# the assertions are about WHAT THIS CONTROLLER CREATES, not about
# CAPI's own reconciliation of it.
set -euo pipefail
cd "$(dirname "$0")"
REPO_DIR="$(cd ../.. && pwd)"
CLUSTER=cldt-e2e
LOG_DIR="${LOG_DIR:-$(mktemp -d /tmp/cldt-kind-e2e.XXXXXX)}"
RELEASES_PORT=18732

CONTROLLER_PID=""
RELEASES_PID=""
cleanup() {
  [ -n "$CONTROLLER_PID" ] && kill "$CONTROLLER_PID" 2>/dev/null || true
  [ -n "$RELEASES_PID" ] && kill "$RELEASES_PID" 2>/dev/null || true
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; [ -f "$LOG_DIR/controller.log" ] && tail -30 "$LOG_DIR/controller.log" >&2; exit 1; }

# Retry a command until success or timeout (seconds).
until_ok() {
  local timeout=$1; shift
  local waited=0
  until "$@" >/dev/null 2>&1; do
    sleep 2; waited=$((waited + 2))
    [ "$waited" -lt "$timeout" ] || return 1
  done
}

echo "--- build the controller ---"
( cd "$REPO_DIR/controller" && go build -o "$LOG_DIR/endpoint-controller" ./cmd/endpoint-controller )

echo "--- kind cluster (1 control-plane + 1 worker; default posture must pick ONLY the worker) ---"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
EOF
export KUBECONFIG="$LOG_DIR/kubeconfig"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
WORKER=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}')
CONTROL_PLANE=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')

echo "--- shim CAPI/CAPA CRDs ---"
for kind_ns in "machines:cluster.x-k8s.io:Machine:Namespaced" \
               "clusters:cluster.x-k8s.io:Cluster:Namespaced" \
               "awsmachines:infrastructure.cluster.x-k8s.io:AWSMachine:Namespaced" \
               "awsclusters:infrastructure.cluster.x-k8s.io:AWSCluster:Namespaced" \
               "awsclusterstaticidentities:infrastructure.cluster.x-k8s.io:AWSClusterStaticIdentity:Cluster"; do
  IFS=: read -r plural group kindname scope <<< "$kind_ns"
  kubectl apply -f - <<EOF >/dev/null
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ${plural}.${group}
spec:
  group: ${group}
  names: {kind: ${kindname}, listKind: ${kindname}List, plural: ${plural}, singular: $(echo "$kindname" | tr '[:upper:]' '[:lower:]')}
  scope: ${scope}
  versions:
    - name: v1beta2
      served: true
      storage: true
      subresources: {status: {}}
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
EOF
done
kubectl apply -f "$REPO_DIR/manifests/wg-dialer/crd.yaml" >/dev/null

echo "--- namespace + AWS provider config (the only user-supplied inputs) ---"
kubectl create namespace wg-dialer >/dev/null
kubectl -n wg-dialer create secret generic aws-provider-config \
  --from-literal=ami-arm64=ami-0e2e2e2e2e2e2e2e2 \
  --from-literal=subnet-id=subnet-0123456789 >/dev/null

echo "--- fake k0s releases API (kind's kubelet has no +k0s suffix a real lookup could resolve) ---"
KUBELET_VERSION=$(kubectl get node "$WORKER" -o jsonpath='{.status.nodeInfo.kubeletVersion}')
KUBELET_VERSION="$KUBELET_VERSION" python3 ./fake-k0s-releases.py "$RELEASES_PORT" &
RELEASES_PID=$!
# The k0s SPECIALIZATION's own knob, in its own provider-config Secret
# (the same pattern as aws-provider-config) -- never an operator flag.
kubectl -n wg-dialer create secret generic k0s-provider-config \
  --from-literal=releases-api="http://127.0.0.1:$RELEASES_PORT" >/dev/null

echo "--- run the controller (out of cluster, against kind) ---"
DIALER_IMAGE="ghcr.io/appmana/cloud-provisioning-dialer:test@sha256:0000000000000000000000000000000000000000000000000000000000000000"
BIN_URL="https://github.com/appmana/cloud-provisioning/releases/download/dialer-test/wg-dialer-linux-arm64"
BIN_SHA="1111111111111111111111111111111111111111111111111111111111111111"
"$LOG_DIR/endpoint-controller" \
  --dialer-image="$DIALER_IMAGE" \
  --dialer-pod-cidrs=10.3.0.0/16 \
  --dialer-service-cidrs=10.152.184.0/24 \
  --join-template-path="$REPO_DIR/join-patterns/k0s-worker.cloud-config.tmpl" \
  --join-api-address=https://10.2.0.19:6443 \
  --join-api-vip=10.2.0.19 \
  --join-node-vip4-prefix=10.2.0. \
  --join-node-vip6-prefix=fd5a:8000:1::c8: \
  --join-node-vip-start=200 \
  --join-dialer-binary-url-arm64="$BIN_URL" \
  --join-dialer-binary-sha256-arm64="$BIN_SHA" \
  >"$LOG_DIR/controller.log" 2>&1 &
CONTROLLER_PID=$!

echo "--- gate: mesh side comes up on its own (peer Secret created, worker allocated, both DaemonSets) ---"
until_ok 60 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.node-tunnel-address-$WORKER}' | grep -q ." \
  || fail "worker never allocated a tunnel address (controller must create the peer Secret itself)"
kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath="{.data.node-tunnel-address-$CONTROL_PLANE}" | grep -q . \
  && fail "the CONTROL-PLANE node was allocated a tunnel address -- default posture must exclude controllers"
until_ok 30 kubectl -n wg-dialer get daemonset wg-dialer || fail "on-prem DaemonSet never created"
until_ok 30 kubectl -n wg-dialer get daemonset wg-dialer-cloud || fail "cloud DaemonSet never created"
IFACE=$(kubectl -n wg-dialer get daemonset wg-dialer -o json | python3 -c "
import json,sys
args = json.load(sys.stdin)['spec']['template']['spec']['containers'][0]['args']
print([a.split('=',1)[1] for a in args if a.startswith('--iface=')][0])")
case "$IFACE" in cldt????????) ;; *) fail "on-prem DS iface '$IFACE' is not a cldt<8hex> name" ;; esac
kubectl -n wg-dialer get daemonset wg-dialer -o json | python3 -c "
import json,sys
spec = json.load(sys.stdin)['spec']['template']['spec']
terms = spec['affinity']['nodeAffinity']['requiredDuringSchedulingIgnoredDuringExecution']['nodeSelectorTerms'][0]['matchExpressions']
assert any(t['key']=='node-role.kubernetes.io/control-plane' and t['operator']=='DoesNotExist' for t in terms), terms
assert any(t['key']=='kubernetes.io/os' and t['values']==['linux'] for t in terms), terms
assert not spec.get('tolerations'), 'on-prem DS must NOT tolerate the cloud-worker taint'
args = spec['containers'][0]['args']
assert not any('0.0.0.0/0' in a or '::/0' in a for a in args), args
" || fail "on-prem DaemonSet scheduling/args are wrong"
kubectl -n wg-dialer get daemonset wg-dialer-cloud -o json | python3 -c "
import json,sys
spec = json.load(sys.stdin)['spec']['template']['spec']
assert spec['nodeSelector'].get('cloud-provisioning.appmana.com/role') == 'cloud-worker', spec.get('nodeSelector')
assert any(t.get('key')=='cloud-provisioning.appmana.com/internet-facing' for t in spec.get('tolerations',[])), spec.get('tolerations')
args = spec['containers'][0]['args']
assert any(a=='--machine-name-file=/etc/wg-dialer/machine-name' for a in args), args
" || fail "cloud DaemonSet scheduling/args are wrong"
echo "  peer Secret self-created, worker-only allocation, both DaemonSets shaped correctly ($IFACE)"

echo "--- simulate the worker dialer pod's one job here: publish its public key ---"
WORKER_PUB=$(wg genkey | wg pubkey)
kubectl -n wg-dialer patch secret wg-dialer-peer --type=merge \
  -p "{\"data\":{\"node-public-key-$WORKER\":\"$(printf '%s' "$WORKER_PUB" | base64 -w0)\"}}" >/dev/null

echo "--- the CAPI Cluster the claim resolves against ---"
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata: {name: kind-test, namespace: default}
spec:
  infrastructureRef: {apiGroup: infrastructure.cluster.x-k8s.io, kind: AWSCluster, name: kind-test}
EOF

echo "--- apply ONE ProvisionedNodeClaim ---"
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata: {name: public-worker, namespace: default}
spec:
  requests: {cpu: "2", memory: 4Gi}
  arch: arm64
EOF

echo "--- assert the fan-out ---"
until_ok 60 kubectl get awsmachine public-worker || fail "AWSMachine never created from the claim"
until_ok 60 kubectl get machine public-worker || fail "Machine never created from the claim"
kubectl get awsmachine public-worker -o json | python3 -c "
import json,sys
m = json.load(sys.stdin)
assert m['spec']['instanceType'] == 't4g.medium', m['spec']
assert m['spec']['ami']['id'] == 'ami-0e2e2e2e2e2e2e2e2', m['spec']
owners = m['metadata']['ownerReferences']
assert len(owners) == 1 and owners[0]['kind'] == 'ProvisionedNodeClaim', owners
" || fail "AWSMachine shape wrong"
kubectl get machine public-worker -o json | python3 -c "
import json,sys
m = json.load(sys.stdin)
assert m['spec']['bootstrap']['dataSecretName'] == 'public-worker-bootstrap', m['spec']
assert m['spec']['infrastructureRef']['kind'] == 'AWSMachine', m['spec']
assert m['metadata']['labels']['cloud-provisioning.appmana.com/role'] == 'cloud-worker'
owners = m['metadata']['ownerReferences']
assert len(owners) == 1 and owners[0]['kind'] == 'ProvisionedNodeClaim', owners
" || fail "Machine shape wrong"

until_ok 90 kubectl get secret public-worker-bootstrap || fail "bootstrap Secret never rendered (join reconciler)"
kubectl get secret public-worker-bootstrap -o json | python3 -c "
import base64, json, sys
s = json.load(sys.stdin)
owners = s['metadata']['ownerReferences']
assert len(owners) == 1 and owners[0]['kind'] == 'Machine', owners
value = base64.b64decode(s['data']['value']).decode()
for want in [
    '$BIN_URL', '$BIN_SHA', 'sha256sum -c',
    '--iface=$IFACE',
    '$WORKER_PUB',
    '/etc/wg-dialer/machine-name',
    'public-worker',
    '10.2.0.200',            # first node VIP allocation
    'K0S_VERSION=$KUBELET_VERSION.0',  # resolved through the releases API
]:
    assert want in value, f'bootstrap userdata missing {want!r}'
# The ONLY private key in userdata is the cloud node's own identity
# (rendered fresh for this machine). No on-prem node's key may appear:
# the peer Secret never contained one to leak.
assert value.count('privateKey') == 1, 'expected exactly the cloud node identity key'
" || fail "bootstrap Secret content wrong"

echo "--- peer entries + adoption Secret ---"
until_ok 30 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-public-key-public-worker}' | grep -q ." \
  || fail "peer-public-key-<machine> never written"
PENDING=$(kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-endpoint-public-worker}' | base64 -d)
[ "$PENDING" == "pending" ] || fail "peer-endpoint should be 'pending' before an ExternalIP exists, got '$PENDING'"
kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-route-hosts-public-worker}' | base64 -d | grep -q "10.2.0.200" \
  || fail "peer-route-hosts missing the allocated node VIP"
until_ok 60 kubectl -n wg-dialer get secret public-worker-tunnel-peers \
  || fail "adoption Secret never rendered"
kubectl -n wg-dialer get secret public-worker-tunnel-peers -o jsonpath='{.data.peers\.json}' | base64 -d | python3 -c "
import json,sys
doc = json.load(sys.stdin)
peers = doc['peers']
assert any(p['publicKey'] == '$WORKER_PUB' for p in peers), peers
assert all('privateKey' not in p for p in peers)
assert any('10.2.0.19/32' in p.get('allowedIPs', []) for p in peers), 'API VIP must ride the transit local peer'
" || fail "adoption Secret content wrong"

echo "--- claim status ---"
until_ok 30 sh -c "kubectl get provisionednodeclaim public-worker -o jsonpath='{.status.instanceType}' | grep -q t4g.medium" \
  || fail "claim status never mirrored the resolved instance type"

echo "--- endpoint mirroring: fake CAPA setting an ExternalIP ---"
kubectl patch machine public-worker --subresource=status --type=merge \
  -p '{"status":{"addresses":[{"type":"ExternalIP","address":"203.0.113.99"}],"phase":"Provisioned"}}' >/dev/null
until_ok 30 sh -c "kubectl -n wg-dialer get secret wg-dialer-peer -o jsonpath='{.data.peer-endpoint-public-worker}' | base64 -d | grep -q 203.0.113.99:51820" \
  || fail "peer endpoint never mirrored from Machine.status.addresses"
echo "  endpoint mirrored: pending -> 203.0.113.99:51820"

echo "--- cascade: delete the claim, EVERYTHING derived goes away ---"
kubectl delete provisionednodeclaim public-worker --wait=true >/dev/null
until_ok 60 sh -c "! kubectl get machine public-worker" || fail "Machine survived claim deletion"
until_ok 60 sh -c "! kubectl get awsmachine public-worker" || fail "AWSMachine survived claim deletion"
until_ok 60 sh -c "! kubectl get secret public-worker-bootstrap" || fail "bootstrap Secret survived (must be GC'd so a re-created claim gets a FRESH join token)"

echo
echo "ALL ASSERTIONS PASSED (logs: $LOG_DIR)"
