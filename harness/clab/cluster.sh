#!/usr/bin/env bash
# Build the cluster on the site's three nodes.
#
# Every address the cluster knows itself by is a segment address. The
# management interface exists so this script can drive the lab and for
# nothing else, so kubelet is told which address to register and the API
# server is told which to advertise. If either were left to autodetect,
# the cluster would form on the management network and the isolation the
# topology asserts would be true and irrelevant.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
LAN=10.10.0
POD_CIDR=10.244.0.0/16
SVC_CIDR=10.96.0.0/12
OUT="${OUT:-$PWD/out}"
mkdir -p "$OUT"


c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
# Anything fed on stdin needs -i, or the command succeeds having read
# nothing and the file it was meant to write is empty.
write_to() { docker exec -i "$(c "$1")" sh -c "cat >$2"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Everything that talks to the API talks to it from the bastion, which
# is on the site network. Nothing here reaches into the site from
# outside it, which is what makes the same scripts work once the nodes
# are VMs whose single NIC is on a network the host cannot address.
k() { in_node bastion kubectl "$@"; }

echo "--- control plane on cp at $LAN.10 ---"
write_to cp /tmp/init.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: $LAN.10
  bindPort: 6443
nodeRegistration:
  kubeletExtraArgs:
    - {name: node-ip, value: $LAN.10}
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
networking:
  podSubnet: $POD_CIDR
  serviceSubnet: $SVC_CIDR
controlPlaneEndpoint: $LAN.10:6443
apiServer:
  certSANs: [$LAN.10, 127.0.0.1, cp]
EOF

if ! in_node cp test -f /etc/kubernetes/admin.conf; then
  # Preflight inspects the kernel it is running on, and in a container
  # that is this host's, so it checks things the node neither owns nor
  # can change. kind passes the same flag for the same reason.
  in_node cp kubeadm init --config /tmp/init.yaml --skip-token-print \
    --ignore-preflight-errors=all \
    >"$OUT/init.log" 2>&1 || { tail -30 "$OUT/init.log" >&2; fail "kubeadm init (see $OUT/init.log)"; }
fi
# The kubeconfig points at the site address and is used from the site,
# so nothing has to be rewritten and no certificate has to cover an
# address that changes whenever the containers restart.
in_node cp cat /etc/kubernetes/admin.conf > "$OUT/kubeconfig"
in_node bastion mkdir -p /root/.kube
write_to bastion /root/.kube/config < "$OUT/kubeconfig"
k get --raw /healthz >/dev/null 2>&1 || fail "the API server is not answering"
echo "  up, kubeconfig at $OUT/kubeconfig"

echo "--- workers ---"
JOIN=$(in_node cp kubeadm token create --print-join-command 2>/dev/null | tr -d '\r')
[ -n "$JOIN" ] || fail "no join command"
for w in w1 w2; do
  ip=$LAN.$( [ "$w" = w1 ] && echo 11 || echo 12 )
  if ! k get node "$w" >/dev/null 2>&1; then
    # node-ip belongs to kubelet, not to join, and it has to be set
    # before the node registers or it registers by whichever address it
    # picks, which here is the management one.
    write_to "$w" /etc/default/kubelet <<<"KUBELET_EXTRA_ARGS=--node-ip=$ip"
    in_node "$w" sh -c "$JOIN --ignore-preflight-errors=all" >"$OUT/join-$w.log" 2>&1 \
      || { tail -20 "$OUT/join-$w.log" >&2; fail "$w did not join"; }
  fi
done
for n in cp w1 w2; do
  k wait --for=condition=Ready node/"$n" --timeout=300s >/dev/null 2>&1 || true
done

echo "--- every node registered by its segment address ---"
k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' |
  while read -r name ip; do
    case "$ip" in
      $LAN.*) echo "  $name $ip" ;;
      *) fail "$name registered $ip, which is not a site address: the cluster formed on the wrong network" ;;
    esac
  done
