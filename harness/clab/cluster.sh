#!/usr/bin/env bash
# Build the cluster on the site's five nodes: three control planes and
# two workers, with no VIP anywhere.
#
# Three control planes, not two: stacked etcd needs a majority, and a
# majority of two is two, so a second control plane only adds a way to
# halt. Three is the smallest number that tolerates a death.
#
# There is no address that moves between nodes. Every node holds all
# three control-plane addresses and reaches "the API server" through
# its own loopback: a static-pod TCP forwarder on 127.0.0.1:7445,
# written before kubeadm ever runs, because kubelet starts static pods
# from disk with no API access at all. controlPlaneEndpoint states that
# loopback, so every kubelet.conf kubeadm writes points at the node's
# own forwarder and no single member's death strands any node. This is
# the same shape the remotes get from the dialer's own balancer, and
# the same shape k0s calls nllb: the worker just has the three
# addresses.
#
# Every address the cluster knows itself by is a segment address. The
# management interface exists so this script can drive the lab and for
# nothing else, so kubelet is told which address to register and the
# API server which to advertise; joins bootstrap through a real
# member's address, because the loopback forwarder is only meaningful
# on a node that already has one.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
LAN=10.10.0
POD_CIDR=10.244.0.0/16
SVC_CIDR=10.96.0.0/12
PROXY_PORT=7445
NGINX_IMAGE="${NGINX_IMAGE:-nginx:1.27-alpine}"
OUT="${OUT:-$PWD/out}"
mkdir -p "$OUT"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
# Anything fed on stdin needs -i, or the command succeeds having read
# nothing and the file it was meant to write is empty.
write_to() { docker exec -i "$(c "$1")" sh -c "cat >$2"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Everything that talks to the API talks to it from the bastion, which
# is on the site network. The wrapper below picks a live control plane
# per invocation, because the bastion is not a cluster node and runs no
# forwarder of its own.
k() { in_node bastion kubectl "$@"; }

CP_ADDRS="$LAN.10 $LAN.13 $LAN.14"
cp_addr() {
  case "$1" in
    cp)  echo "$LAN.10" ;;
    cp2) echo "$LAN.13" ;;
    cp3) echo "$LAN.14" ;;
  esac
}

echo "--- the loopback forwarder, on every site node, before anything else ---"
# Preload the image (the site pulls nothing) and write the config and
# the static-pod manifest. kubelet reads /etc/kubernetes/manifests from
# disk whenever it runs, so the forwarder exists on a node before,
# during, and after any control plane's death, with no dependency on
# the API it fronts.
if ! docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1; then
  docker pull -q "$NGINX_IMAGE" >/dev/null || fail "could not pull $NGINX_IMAGE"
fi
for n in cp cp2 cp3 w1 w2; do
  docker save "$NGINX_IMAGE" | docker exec -i "$(c "$n")" ctr -n k8s.io images import - >/dev/null 2>&1 \
    || fail "could not import $NGINX_IMAGE into $n"
  in_node "$n" mkdir -p /etc/api-proxy /etc/kubernetes/manifests
  write_to "$n" /etc/api-proxy/nginx.conf <<EOF
worker_processes 1;
events { worker_connections 256; }
stream {
  upstream api {
$(for a in $CP_ADDRS; do echo "    server $a:6443 max_fails=1 fail_timeout=5s;"; done)
  }
  server {
    listen 127.0.0.1:$PROXY_PORT;
    proxy_connect_timeout 2s;
    proxy_pass api;
  }
}
EOF
  write_to "$n" /etc/kubernetes/manifests/api-proxy.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: api-proxy
  namespace: kube-system
spec:
  hostNetwork: true
  priorityClassName: system-node-critical
  containers:
    - name: nginx
      image: $NGINX_IMAGE
      imagePullPolicy: Never
      command: ["nginx", "-g", "daemon off;", "-c", "/etc/api-proxy/nginx.conf"]
      volumeMounts:
        - {name: conf, mountPath: /etc/api-proxy, readOnly: true}
  volumes:
    - name: conf
      hostPath:
        path: /etc/api-proxy
        type: Directory
EOF
done
echo "  $NGINX_IMAGE on 127.0.0.1:$PROXY_PORT, fanning to: $CP_ADDRS"

echo "--- first control plane on cp at $LAN.10 ---"
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
# The node-local forwarder: every kubelet.conf kubeadm writes points
# here, which on any node is that node's own path to whichever control
# plane is alive. Loopback in every API server's SANs is what lets a
# client verify the certificate for the address it dialed; the real
# addresses are there for the bastion and for anything that dials a
# member directly.
controlPlaneEndpoint: 127.0.0.1:$PROXY_PORT
apiServer:
  certSANs: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
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
  # can change. It also objects to a manifests directory that already
  # holds the forwarder, which is there on purpose. kind passes the
  # same flag for the same reasons.
  in_node cp kubeadm init --config /tmp/init.yaml --skip-token-print \
    --upload-certs --ignore-preflight-errors=all \
    >"$OUT/init.log" 2>&1 || { tail -30 "$OUT/init.log" >&2; fail "kubeadm init (see $OUT/init.log)"; }
fi
in_node cp cat /etc/kubernetes/admin.conf > "$OUT/kubeconfig"
in_node bastion mkdir -p /root/.kube
write_to bastion /root/.kube/config < "$OUT/kubeconfig"

# The bastion is not a cluster node: nothing serves its loopback, and
# its kubeconfig names the loopback endpoint. Pick a live member per
# invocation instead. The real addresses are in every server
# certificate's SANs, so --server needs no other accommodation.
if ! in_node bastion test -f /usr/local/bin/kubectl.real; then
  # command -v is a shell builtin, so it needs a shell to run in; the
  # copy lands the original wherever it was found, and the wrapper
  # then shadows it from /usr/local/bin.
  in_node bastion sh -c 'cp "$(command -v kubectl)" /usr/local/bin/kubectl.real' \
    || fail "no kubectl on the bastion to wrap"
fi
write_to bastion /usr/local/bin/kubectl <<EOF
#!/bin/sh
# Pick a live control plane, then run the real kubectl against it. A
# probe failure is connectivity, so trying the next member is right; a
# kubectl failure after a good probe is an answer, not a reason to ask
# someone else.
for s in $CP_ADDRS; do
  if curl -ksm 2 -o /dev/null "https://\$s:6443/livez" 2>/dev/null; then
    exec /usr/local/bin/kubectl.real --server="https://\$s:6443" "\$@"
  fi
done
exec /usr/local/bin/kubectl.real "\$@"
EOF
in_node bastion chmod 0755 /usr/local/bin/kubectl
k get --raw /healthz >/dev/null 2>&1 || fail "the API server is not answering"
echo "  up, kubeconfig at $OUT/kubeconfig"

echo "--- the other control planes ---"
# A fresh certificate key each run: upload-certs re-encrypts the CA
# bundle into the cluster for two minutes, which is all the join needs.
# Joins bootstrap through cp's real address; kubelet.conf comes out
# pointing at the loopback forwarder, per controlPlaneEndpoint.
JOIN=$(in_node cp kubeadm token create --print-join-command 2>/dev/null | tr -d '\r')
[ -n "$JOIN" ] || fail "no join command"
JOIN=$(echo "$JOIN" | sed "s/127.0.0.1:$PROXY_PORT/$LAN.10:6443/")
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

echo "--- every kubelet dials its own forwarder ---"
# The claim the design makes, checked rather than assumed: kubeadm
# writes kubelet.conf from controlPlaneEndpoint, so every node should
# depend on its own loopback and none on any single member.
for n in cp cp2 cp3 w1 w2; do
  server=$(in_node "$n" sh -c "grep -o 'server: .*' /etc/kubernetes/kubelet.conf" | awk '{print $2}')
  case "$server" in
    "https://127.0.0.1:$PROXY_PORT") ;;
    *) fail "$n's kubelet dials $server, not its own forwarder: that node just inherited a single point of failure" ;;
  esac
done
echo "  all five at https://127.0.0.1:$PROXY_PORT"

echo "--- every node registered by its segment address ---"
k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' |
  while read -r name ip; do
    case "$ip" in
      $LAN.*) echo "  $name $ip" ;;
      *) fail "$name registered $ip, which is not a site address: the cluster formed on the wrong network" ;;
    esac
  done