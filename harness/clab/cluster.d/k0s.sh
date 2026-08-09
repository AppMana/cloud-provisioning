# The k0s site: three controllers with worker components enabled and
# two pure workers, nllb on, no VIP.
#
# k0s is the production distribution, and it ships the property the
# kubeadm builder has to construct: nodeLocalLoadBalancing gives every
# worker an envoy on its own loopback (127.0.0.1:7443) holding all
# controller addresses, so no forwarder, no static pod, and no
# transient bootstrap proxy appear anywhere in this file. Controllers
# dial their own API server, which k0s arranges itself, and nllb's
# documented limitation on controller+worker nodes is therefore no
# loss: nllb exists for nodes whose API server is elsewhere.
#
# kube-proxy's conntrack sysctl is pinned off through
# spec.network.kubeProxy.extraArgs for the same reason the kubeadm
# builder pins maxPerCore to zero: /proc/sys is not writable from a
# container, and the kernel's values belong to the host.
#
# Sourced by cluster.sh, which provides the helpers and runs the
# distribution-independent assertions afterwards.

K0S_VERSION="${K0S_VERSION:-}"
NLLB_PORT=7443

# One download on the host serves all five nodes; the site itself
# pulls nothing. The version defaults to the latest release so the lab
# tracks what a new site would actually install, and can be pinned
# through the environment when a specific one is under test. Sets
# K0S_BIN and K0S_VERSION rather than echoing: a command substitution
# would run this in a subshell and the resolved version would never
# reach the caller.
fetch_k0s() {
  if [ -z "$K0S_VERSION" ]; then
    K0S_VERSION=$(curl -fsSL https://api.github.com/repos/k0sproject/k0s/releases/latest \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])') \
      || fail "could not resolve the latest k0s release"
  fi
  K0S_BIN="$OUT/binaries/k0s-$K0S_VERSION"
  if [ ! -f "$K0S_BIN" ]; then
    mkdir -p "$OUT/binaries"
    curl -fsSL -o "$K0S_BIN.part" \
      "https://github.com/k0sproject/k0s/releases/download/$K0S_VERSION/k0s-$K0S_VERSION-amd64" \
      || fail "could not download k0s $K0S_VERSION"
    chmod 0755 "$K0S_BIN.part" && mv "$K0S_BIN.part" "$K0S_BIN"
  fi
}

write_k0s_config() {
  local n="$1" ip="$2"
  in_node "$n" mkdir -p /etc/k0s
  write_to "$n" /etc/k0s/k0s.yaml <<EOF
apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
metadata:
  name: k0s
spec:
  api:
    address: $ip
    sans: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
  storage:
    type: etcd
    etcd:
      peerAddress: $ip
  network:
    # "custom" hands the pod network to install.sh's cni.d installer;
    # CNI=default keeps kuberouter, the network k0s itself ships.
    provider: $( [ "${CNI:-calico}" = default ] && echo kuberouter || echo custom )
    podCIDR: $POD_CIDR
    serviceCIDR: $SVC_CIDR
    nodeLocalLoadBalancing:
      enabled: true
      type: EnvoyProxy
    kubeProxy:
      extraArgs:
        conntrack-max-per-core: "0"
EOF
}

# k0s installs itself as a systemd unit (k0scontroller/k0sworker), so
# a reboot row's docker start brings the node back through the
# distribution's own supervision, which is exactly what those rows
# exist to prove.
start_k0s() {
  local n="$1" role_args="$2"
  if ! in_node "$n" test -f /etc/systemd/system/k0scontroller.service \
    && ! in_node "$n" test -f /etc/systemd/system/k0sworker.service; then
    in_node "$n" sh -c "k0s install $role_args" || fail "$n: k0s install"
  fi
  in_node "$n" k0s start 2>/dev/null || true
}

distro_build() {
  echo "--- the k0s binary, carried onto every site node ---"
  fetch_k0s
  echo "  $K0S_VERSION"
  for n in $SITE_NODES; do
    in_node "$n" test -x /usr/local/bin/k0s \
      || docker cp "$K0S_BIN" "$(c "$n")":/usr/local/bin/k0s
  done

  echo "--- first controller on cp at $LAN.10 ---"
  write_k0s_config cp "$LAN.10"
  start_k0s cp "controller --enable-worker -c /etc/k0s/k0s.yaml --kubelet-extra-args=--node-ip=$LAN.10"
  # The API answering is the gate for everything after it: tokens are
  # minted through it and the kubeconfig is read from it.
  for _ in $(seq 1 60); do
    in_node cp k0s kubeconfig admin >/dev/null 2>&1 && break
    sleep 5
  done
  in_node cp k0s kubeconfig admin > "$OUT/kubeconfig" 2>/dev/null \
    || fail "cp's API never answered, so there is no kubeconfig"
  install_bastion_kubectl
  echo "  up, kubeconfig at $OUT/kubeconfig"

  echo "--- the other controllers ---"
  # k0s's own join tokens, k0s's own CLI, because this is the site
  # being provisioned the way an operator provisions it; the REMOTE
  # join is what must go through the product's provider instead.
  for n in cp2 cp3; do
    ip=$(cp_addr "$n")
    if ! k get node "$n" >/dev/null 2>&1; then
      token=$(in_node cp k0s token create --role controller 2>/dev/null | tr -d '\r\n')
      [ -n "$token" ] || fail "no controller token for $n"
      write_k0s_config "$n" "$ip"
      printf '%s' "$token" | docker exec -i "$(c "$n")" sh -c 'cat >/etc/k0s/controller-token'
      start_k0s "$n" "controller --enable-worker --token-file /etc/k0s/controller-token -c /etc/k0s/k0s.yaml --kubelet-extra-args=--node-ip=$ip"
    fi
  done

  echo "--- workers ---"
  for w in w1 w2; do
    ip=$LAN.$( [ "$w" = w1 ] && echo 11 || echo 12 )
    if ! k get node "$w" >/dev/null 2>&1; then
      token=$(in_node cp k0s token create --role worker 2>/dev/null | tr -d '\r\n')
      [ -n "$token" ] || fail "no worker token for $w"
      in_node "$w" mkdir -p /etc/k0s
      printf '%s' "$token" | docker exec -i "$(c "$w")" sh -c 'cat >/etc/k0s/worker-token'
      start_k0s "$w" "worker --token-file /etc/k0s/worker-token --kubelet-extra-args=--node-ip=$ip"
    fi
  done

  # Registration is the gate here (readiness needs a network that
  # install.sh has not put on yet), and k0s starts its components
  # asynchronously, so give every node time to appear.
  for _ in $(seq 1 60); do
    all=1
    for n in $SITE_NODES; do k get node "$n" >/dev/null 2>&1 || { all=; break; }; done
    [ -n "$all" ] && break
    sleep 5
  done

  # k0s registers a controller's node with no role label at all
  # (ROLES <none>), unlike every other distribution here, and the
  # placement rows select control planes by the standard label. The
  # role is a fact about the site, so the site's builder states it,
  # exactly as an operator labeling their nodes would.
  for n in cp cp2 cp3; do
    k label node "$n" node-role.kubernetes.io/control-plane= --overwrite >/dev/null 2>&1 \
      || fail "could not label $n as a control plane"
  done
}

distro_kubelet_invariant() {
  # nllb is the claim: a pure worker's kubelet dials its own loopback
  # envoy, which holds every controller address, so no single
  # controller's death strands it. Measured, not assumed, against the
  # kubeconfig the running kubelet actually loads: nllb hands kubelet
  # its own file under /run/k0s/nllb (the kubelet process's
  # --kubeconfig says so), aimed at the envoy on the node's own
  # loopback, k0s's choice of family included ([::1]). The file at
  # /var/lib/k0s/kubelet.conf keeps the bootstrap-time join server
  # forever and describes nothing the kubelet still does. The envoy
  # image is pulled on first start, so this converges rather than
  # holding instantly, and a check without a deadline's patience
  # reports the wrong verdict on a healthy node.
  #
  # A controller's kubelet dialing its own API server is k0s's
  # arrangement for --enable-worker nodes (nllb's documented
  # limitation there), and is no cross-node dependency either.
  local deadline=$((SECONDS + 300)) server ok
  for n in $SITE_NODES; do
    while :; do
      ok=""
      case "$n" in
        cp|cp2|cp3)
          server=$(in_node "$n" sh -c "grep -o 'server: .*' /var/lib/k0s/kubelet.conf" 2>/dev/null | awk '{print $2}')
          case "$server" in
            https://127.0.0.1:*|https://localhost:*|"https://$(cp_addr "$n"):6443") ok=1 ;;
          esac ;;
        *)
          server=$(in_node "$n" sh -c "grep -o 'server: .*' /run/k0s/nllb/kubeconfig.yaml" 2>/dev/null | awk '{print $2}')
          case "$server" in
            "https://[::1]:$NLLB_PORT"|"https://127.0.0.1:$NLLB_PORT") ok=1 ;;
          esac ;;
      esac
      if [ -n "$ok" ]; then echo "  $n $server"; break; fi
      if [ "$SECONDS" -ge "$deadline" ]; then
        fail "$n's kubelet dials ${server:-nothing}, a dependency on another node's survival"
      fi
      sleep 5
    done
  done
}
