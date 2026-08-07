#!/usr/bin/env bash
# Build the cluster on the site's five nodes: three control planes and
# two workers.
#
# Three control planes, not two: stacked etcd needs a majority, and a
# majority of two is two, so a second control plane only adds a way to
# halt. Three is the smallest number that tolerates a death, which is
# what the outage rows take away.
#
# The cluster knows itself by one address, the API VIP, held by
# kube-vip on whichever control plane currently leads. Every cert
# carries it (controlPlaneEndpoint), every kubeconfig names it, and a
# remote reaches it over the tunnel like any other site address. When
# the holder dies the VIP moves; nothing that dials it has to know.
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
VIP=$LAN.100
POD_CIDR=10.244.0.0/16
SVC_CIDR=10.96.0.0/12
KUBE_VIP_IMAGE="${KUBE_VIP_IMAGE:-ghcr.io/kube-vip/kube-vip:v0.8.9}"
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

cp_addr() {
  case "$1" in
    cp)  echo "$LAN.10" ;;
    cp2) echo "$LAN.13" ;;
    cp3) echo "$LAN.14" ;;
  esac
}

echo "--- first control plane on cp at $LAN.10, endpoint $VIP ---"
# The VIP is held by hand on cp until kube-vip can hold it properly:
# kube-vip leader-elects through the API server, so it cannot answer
# for the endpoint before the endpoint exists. The manual address is
# removed once kube-vip is installed on all three.
in_node cp ip addr replace "$VIP/24" dev eth1

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
controlPlaneEndpoint: $VIP:6443
apiServer:
  certSANs: [$VIP, $LAN.10, $LAN.13, $LAN.14, 127.0.0.1, cp, cp2, cp3]
---
apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
conntrack:
  # Left alone. kube-proxy would otherwise raise nf_conntrack_max, and
  # /proc/sys is not writable from a container, so it exits and nothing
  # translates a service address: the network's own pods then cannot
  # reach the API service and never start. The kernel here belongs to
  # the host and its value is the host's to choose.
  maxPerCore: 0
  min: 0
EOF

if ! in_node cp test -f /etc/kubernetes/admin.conf; then
  # Preflight inspects the kernel it is running on, and in a container
  # that is this host's, so it checks things the node neither owns nor
  # can change. kind passes the same flag for the same reason.
  in_node cp kubeadm init --config /tmp/init.yaml --skip-token-print \
    --upload-certs --ignore-preflight-errors=all \
    >"$OUT/init.log" 2>&1 || { tail -30 "$OUT/init.log" >&2; fail "kubeadm init (see $OUT/init.log)"; }
fi
# The kubeconfig points at the VIP and is used from the site, so
# nothing has to be rewritten and no certificate has to cover an
# address that changes whenever the containers restart.
in_node cp cat /etc/kubernetes/admin.conf > "$OUT/kubeconfig"
in_node bastion mkdir -p /root/.kube
write_to bastion /root/.kube/config < "$OUT/kubeconfig"
k get --raw /healthz >/dev/null 2>&1 || fail "the API server is not answering"
echo "  up, kubeconfig at $OUT/kubeconfig"

echo "--- the other control planes ---"
# A fresh certificate key each run: upload-certs re-encrypts the CA
# bundle into the cluster for two minutes, which is all the join needs.
JOIN=$(in_node cp kubeadm token create --print-join-command 2>/dev/null | tr -d '\r')
[ -n "$JOIN" ] || fail "no join command"
CERT_KEY=$(in_node cp kubeadm init phase upload-certs --upload-certs 2>/dev/null | tail -1 | tr -d '\r')
[ -n "$CERT_KEY" ] || fail "no certificate key"
for n in cp2 cp3; do
  ip=$(cp_addr "$n")
  if ! in_node "$n" test -f /etc/kubernetes/kubelet.conf; then
    write_to "$n" /etc/default/kubelet <<<"KUBELET_EXTRA_ARGS=--node-ip=$ip"
    in_node "$n" sh -c "$JOIN --control-plane --certificate-key $CERT_KEY \
      --apiserver-advertise-address $ip --ignore-preflight-errors=all" \
      >"$OUT/join-$n.log" 2>&1 \
      || { tail -20 "$OUT/join-$n.log" >&2; fail "$n did not join as a control plane"; }
  fi
done

echo "--- workers ---"
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
# Readiness is not asserted here and cannot be: a node with no network
# installed is legitimately NotReady, and this runs before the network.
# What can be asserted now is that every node registered, and by which
# address. install.sh waits for Ready once there is a network to be
# ready for, and fails there.
for n in cp cp2 cp3 w1 w2; do
  k get node "$n" >/dev/null 2>&1 || fail "$n never registered"
done

echo "--- kube-vip takes the endpoint ---"
# Leader-elected ARP on the LAN: the VIP lives on exactly one control
# plane and moves when its holder stops renewing. Installed after the
# joins so /etc/kubernetes/admin.conf exists on all three, and only
# then is cp's manual hold released.
if ! docker image inspect "$KUBE_VIP_IMAGE" >/dev/null 2>&1; then
  docker pull -q "$KUBE_VIP_IMAGE" >/dev/null || fail "could not pull $KUBE_VIP_IMAGE"
fi
for n in cp cp2 cp3; do
  docker save "$KUBE_VIP_IMAGE" | docker exec -i "$(c "$n")" ctr -n k8s.io images import - >/dev/null 2>&1 \
    || fail "could not import kube-vip into $n"
  write_to "$n" /etc/kubernetes/manifests/kube-vip.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kube-vip
  namespace: kube-system
spec:
  hostNetwork: true
  # kube-vip dials the name "kubernetes"; the alias points it at this
  # node's own API server, the one instance that is always reachable
  # from a control plane whatever the VIP is doing. Omitting this
  # sends the dial to whatever DNS answers, which is nothing here.
  hostAliases:
    - ip: 127.0.0.1
      hostnames: [kubernetes]
  containers:
    - name: kube-vip
      image: $KUBE_VIP_IMAGE
      imagePullPolicy: Never
      args: ["manager"]
      env:
        - {name: vip_arp, value: "true"}
        - {name: address, value: "$VIP"}
        - {name: port, value: "6443"}
        - {name: vip_interface, value: eth1}
        - {name: vip_cidr, value: "32"}
        - {name: cp_enable, value: "true"}
        - {name: cp_namespace, value: kube-system}
        - {name: vip_leaderelection, value: "true"}
        - {name: vip_leaseduration, value: "5"}
        - {name: vip_renewdeadline, value: "3"}
        - {name: vip_retryperiod, value: "1"}
      securityContext:
        capabilities:
          add: [NET_ADMIN, NET_RAW]
      volumeMounts:
        - {mountPath: /etc/kubernetes/admin.conf, name: kubeconfig}
  volumes:
    - name: kubeconfig
      hostPath:
        path: /etc/kubernetes/admin.conf
EOF
done
# Wait for a kube-vip to hold a lease before releasing the manual VIP,
# or the endpoint goes dark with nobody yet responsible for it.
held=
for _ in $(seq 1 30); do
  if k -n kube-system get lease plndr-cp-lock -o jsonpath='{.spec.holderIdentity}' 2>/dev/null | grep -q .; then
    held=1; break
  fi
  sleep 5
done
[ -n "$held" ] || fail "kube-vip never took its lease, so releasing the VIP would orphan the endpoint"
in_node cp ip addr del "$VIP/24" dev eth1 2>/dev/null || true
# The kernel's neighbours may still map the VIP to cp's MAC until the
# leader's gratuitous ARP lands; the check below rides through it.
ok=
for _ in $(seq 1 24); do
  if k get --raw /healthz >/dev/null 2>&1; then ok=1; break; fi
  sleep 5
done
[ -n "$ok" ] || fail "the API endpoint did not survive the handoff to kube-vip"
holder=$(k -n kube-system get lease plndr-cp-lock -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)
echo "  $VIP answered after the handoff, leader: ${holder:-unknown}"

echo "--- every node registered by its segment address ---"
k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' |
  while read -r name ip; do
    case "$ip" in
      $LAN.*) echo "  $name $ip" ;;
      *) fail "$name registered $ip, which is not a site address: the cluster formed on the wrong network" ;;
    esac
  done