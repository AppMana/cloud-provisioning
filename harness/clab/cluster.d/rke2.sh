# The RKE2 site: three servers and two agents, the agents balancing
# for themselves (the same client-side balancer as k3s, supervisor on
# 9345), no VIP.
#
# RKE2 in a container is officially unsupported; Rancher's own CI runs
# it in systemd-in-docker images, which is what kindest/node is. This
# builder is therefore a feasibility probe as much as a builder: if
# RKE2 refuses the container boundary, the finding goes in a commit
# message and the rke2 row defers to the VM phase, rather than
# stacking workarounds here.
#
# Sourced by cluster.sh, which provides the helpers and runs the
# distribution-independent assertions afterwards.

RKE2_VERSION="${RKE2_VERSION:-}"

# One tarball download on the host serves all five nodes: bin/rke2 and
# the systemd units RKE2 itself ships. Its images it pulls from the
# registry on first start, which the site's outbound path allows. Sets
# RKE2_TARBALL and RKE2_VERSION rather than echoing: a command
# substitution would run this in a subshell and the resolved version
# would never reach the caller.
fetch_rke2() {
  if [ -z "$RKE2_VERSION" ]; then
    local tag_url
    tag_url=$(curl -fsS -o /dev/null -w "%{redirect_url}" "https://update.rke2.io/v1-release/channels/stable") \
      || fail "could not resolve rke2's stable channel"
    RKE2_VERSION="${tag_url##*/}"
    [ -n "$RKE2_VERSION" ] || fail "the stable channel redirect carried no tag"
  fi
  RKE2_TARBALL="$OUT/binaries/rke2-$RKE2_VERSION.tar.gz"
  if [ ! -f "$RKE2_TARBALL" ]; then
    mkdir -p "$OUT/binaries"
    curl -fsSL -o "$RKE2_TARBALL.part" \
      "https://github.com/rancher/rke2/releases/download/$RKE2_VERSION/rke2.linux-amd64.tar.gz" \
      || fail "could not download rke2 $RKE2_VERSION"
    mv "$RKE2_TARBALL.part" "$RKE2_TARBALL"
  fi
}

install_rke2() {
  local n="$1"
  in_node "$n" test -x /usr/local/bin/rke2 && return 0
  docker cp "$RKE2_TARBALL" "$(c "$n")":/tmp/rke2.tar.gz
  in_node "$n" tar -xzf /tmp/rke2.tar.gz -C /usr/local
  in_node "$n" sh -c 'cp /usr/local/lib/systemd/system/rke2-*.service /etc/systemd/system/'
  in_node "$n" rm -f /tmp/rke2.tar.gz
  in_node "$n" systemctl daemon-reload
}

start_rke2() {
  local n="$1" unit="$2"
  in_node "$n" systemctl enable --now "$unit" >/dev/null 2>&1 || in_node "$n" systemctl restart "$unit"
}

rke2_config_common() {
  cat <<EOF
node-ip: $2
node-name: $1
kube-proxy-arg:
  - "conntrack-max-per-core=0"
EOF
}

distro_build() {
  echo "--- the rke2 tarball, carried onto every site node ---"
  fetch_rke2 >/dev/null
  echo "  $RKE2_VERSION"
  for n in $SITE_NODES; do
    install_rke2 "$n"
    in_node "$n" mkdir -p /etc/rancher/rke2
  done

  echo "--- first server on cp at $LAN.10 ---"
  write_to cp /etc/rancher/rke2/config.yaml <<EOF
tls-san: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
cluster-cidr: $POD_CIDR
service-cidr: $SVC_CIDR
# "none" hands the pod network to install.sh's cni.d installer;
# CNI=default keeps canal (flannel vxlan + calico policy), the
# network RKE2 itself ships.
cni: $( [ "${CNI:-calico}" = default ] && echo canal || echo none )
disable: [rke2-ingress-nginx, rke2-metrics-server]
$(rke2_config_common cp "$LAN.10")
EOF
  start_rke2 cp rke2-server.service
  for _ in $(seq 1 120); do
    in_node cp test -f /etc/rancher/rke2/rke2.yaml && break
    sleep 5
  done
  in_node cp cat /etc/rancher/rke2/rke2.yaml 2>/dev/null \
    | sed "s|https://127.0.0.1:6443|https://$LAN.10:6443|" > "$OUT/kubeconfig"
  [ -s "$OUT/kubeconfig" ] || fail "cp never wrote a kubeconfig; rke2 may refuse the container boundary (see the header)"
  install_bastion_kubectl
  echo "  up, kubeconfig at $OUT/kubeconfig"

  TOKEN=$(in_node cp cat /var/lib/rancher/rke2/server/node-token | tr -d '\r\n')
  [ -n "$TOKEN" ] || fail "cp has no node token"

  echo "--- the other servers ---"
  for n in cp2 cp3; do
    ip=$(cp_addr "$n")
    if ! k get node "$n" >/dev/null 2>&1; then
      printf '%s' "$TOKEN" | docker exec -i "$(c "$n")" sh -c 'umask 077; cat >/etc/rancher/rke2/cluster-token'
      write_to "$n" /etc/rancher/rke2/config.yaml <<EOF
server: https://$LAN.10:9345
token-file: /etc/rancher/rke2/cluster-token
tls-san: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
cluster-cidr: $POD_CIDR
service-cidr: $SVC_CIDR
cni: $( [ "${CNI:-calico}" = default ] && echo canal || echo none )
disable: [rke2-ingress-nginx, rke2-metrics-server]
$(rke2_config_common "$n" "$ip")
EOF
      start_rke2 "$n" rke2-server.service
    fi
  done

  echo "--- agents ---"
  for w in w1 w2; do
    ip=$LAN.$( [ "$w" = w1 ] && echo 11 || echo 12 )
    if ! k get node "$w" >/dev/null 2>&1; then
      printf '%s' "$TOKEN" | docker exec -i "$(c "$w")" sh -c 'umask 077; cat >/etc/rancher/rke2/cluster-token'
      write_to "$w" /etc/rancher/rke2/config.yaml <<EOF
server: https://$LAN.10:9345
token-file: /etc/rancher/rke2/cluster-token
$(rke2_config_common "$w" "$ip")
EOF
      start_rke2 "$w" rke2-agent.service
    fi
  done

  for _ in $(seq 1 120); do
    all=1
    for n in $SITE_NODES; do k get node "$n" >/dev/null 2>&1 || { all=; break; }; done
    [ -n "$all" ] && break
    sleep 5
  done
}

distro_kubelet_invariant() {
  # Same claim as k3s, same mechanism: kubelet dials the node's own
  # loopback (the agent balancer on agents, the local apiserver's
  # balancer port on servers), so no other node's death strands it.
  for n in $SITE_NODES; do
    server=$(in_node "$n" sh -c "grep -o 'server: .*' /var/lib/rancher/rke2/agent/kubelet.kubeconfig" 2>/dev/null | awk '{print $2}')
    case "$server" in
      https://127.0.0.1:*) echo "  $n $server" ;;
      *) fail "$n's kubelet dials ${server:-nothing}, a dependency on another node's survival" ;;
    esac
  done
}
