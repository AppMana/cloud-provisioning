# The kubeadm site: three control planes and two workers, no VIP.
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
# addresses. kubeadm is the one distribution that ships nothing of the
# sort, which is why this file carries a forwarder and the others
# do not (see join-patterns/README.md for who balances where).
#
# Sourced by cluster.sh, which provides the helpers and runs the
# distribution-independent assertions afterwards.

PROXY_PORT=7445
NGINX_IMAGE="${NGINX_IMAGE:-nginx:1.27-alpine}"

# kubeadm ships no network at all, so "the distribution's default CNI"
# names nothing here.
[ "${CNI:-calico}" = default ] && fail "kubeadm has no built-in network; pick an installer from cni.d/"

# A joining node needs its loopback forwarder before kubelet exists to
# run the static pod: kubeadm join reads the cluster's configuration
# through the server cluster-info names, which is the loopback. So the
# same nginx runs transiently under containerd for the join's duration
# and is killed as soon as the join returns, freeing the port for the
# static pod kubelet then keeps forever. cp never needs this: its
# kubelet, started by init, launches the API server and the forwarder
# as static pods together.
boot_forwarder() {
  in_node "$1" ctr -n k8s.io run -d --net-host \
    --mount "type=bind,src=/etc/api-proxy,dst=/etc/api-proxy,options=rbind:ro" \
    "docker.io/library/$NGINX_IMAGE" api-proxy-boot \
    nginx -g "daemon off;" -c /etc/api-proxy/nginx.conf 2>/dev/null || true
}
stop_boot_forwarder() {
  in_node "$1" ctr -n k8s.io task kill -s SIGKILL api-proxy-boot 2>/dev/null || true
  sleep 1
  in_node "$1" ctr -n k8s.io container rm api-proxy-boot 2>/dev/null || true
}

distro_build() {
  echo "--- the loopback forwarder, on every site node, before anything else ---"
  # Preload the image (the site pulls nothing) and write the config and
  # the static-pod manifest. kubelet reads /etc/kubernetes/manifests from
  # disk whenever it runs, so the forwarder exists on a node before,
  # during, and after any control plane's death, with no dependency on
  # the API it fronts.
  if ! docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1; then
    docker pull -q "$NGINX_IMAGE" >/dev/null || fail "could not pull $NGINX_IMAGE"
  fi
  for n in $SITE_NODES; do
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
  install_bastion_kubectl
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
      boot_forwarder "$n"
      in_node "$n" sh -c "$JOIN --control-plane --certificate-key $CERT_KEY \
        --apiserver-advertise-address $ip --ignore-preflight-errors=all" \
        >"$OUT/join-$n.log" 2>&1 \
        || { stop_boot_forwarder "$n"; tail -20 "$OUT/join-$n.log" >&2; fail "$n did not join as a control plane"; }
      stop_boot_forwarder "$n"
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
      boot_forwarder "$w"
      in_node "$w" sh -c "$JOIN --ignore-preflight-errors=all" >"$OUT/join-$w.log" 2>&1 \
        || { stop_boot_forwarder "$w"; tail -20 "$OUT/join-$w.log" >&2; fail "$w did not join"; }
      stop_boot_forwarder "$w"
    fi
  done
}

distro_kubelet_invariant() {
  # A worker's kubelet must dial its own forwarder, or it inherited a
  # single point of failure from whichever member its join went
  # through. A control plane's kubelet dialing its own API server is
  # kubeadm's own choice and just as good: that server dies only when
  # the node does, which is no cross-node dependency at all.
  for n in $SITE_NODES; do
    server=$(in_node "$n" sh -c "grep -o 'server: .*' /etc/kubernetes/kubelet.conf" | awk '{print $2}')
    ok=""
    case "$n" in
      cp|cp2|cp3) [ "$server" = "https://127.0.0.1:$PROXY_PORT" ] || [ "$server" = "https://$(cp_addr "$n"):6443" ] && ok=1 ;;
      *)          [ "$server" = "https://127.0.0.1:$PROXY_PORT" ] && ok=1 ;;
    esac
    [ -n "$ok" ] || fail "$n's kubelet dials $server, a dependency on another node's survival"
    echo "  $n $server"
  done
}
