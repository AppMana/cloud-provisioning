# The k3s site: three servers and two agents, the agents balancing
# for themselves, no VIP.
#
# k3s needs neither the kubeadm builder's forwarder nor k0s's nllb
# toggle: every agent runs a client-side balancer on its own loopback
# (127.0.0.1:6444, pkg/agent/proxy) across every server it learns from
# the supervisor, persisted in its state file across restarts. Servers
# dial their own apiserver. So this file only places binaries, writes
# configs, and starts units.
#
# kube-proxy's conntrack sysctl is pinned off through kube-proxy-arg
# for the same reason every builder here pins it: /proc/sys is not
# writable from a container, and the kernel's values belong to the
# host.
#
# Sourced by cluster.sh, which provides the helpers and runs the
# distribution-independent assertions afterwards.

K3S_VERSION="${K3S_VERSION:-}"

# CNI=default keeps k3s's embedded flannel (vxlan), the distribution's
# own network; anything else disables it so install.sh's cni.d
# installer owns the pod network. Emitted where both server configs
# are written, so the two can never disagree.
k3s_network_lines() {
  if [ "${CNI:-calico}" = default ]; then
    return 0
  fi
  cat <<EOF
flannel-backend: none
disable-network-policy: true
EOF
}

# One download on the host serves all five nodes. The version defaults
# to k3s's own stable channel, pinnable through the environment. Sets
# K3S_BIN and K3S_VERSION rather than echoing: a command substitution
# would run this in a subshell and the resolved version would never
# reach the caller.
fetch_k3s() {
  if [ -z "$K3S_VERSION" ]; then
    local tag_url
    tag_url=$(curl -fsS -o /dev/null -w "%{redirect_url}" "https://update.k3s.io/v1-release/channels/stable") \
      || fail "could not resolve k3s's stable channel"
    K3S_VERSION="${tag_url##*/}"
    [ -n "$K3S_VERSION" ] || fail "the stable channel redirect carried no tag"
  fi
  K3S_BIN="$OUT/binaries/k3s-$K3S_VERSION"
  if [ ! -f "$K3S_BIN" ]; then
    mkdir -p "$OUT/binaries"
    curl -fsSL -o "$K3S_BIN.part" \
      "https://github.com/k3s-io/k3s/releases/download/$K3S_VERSION/k3s" \
      || fail "could not download k3s $K3S_VERSION"
    chmod 0755 "$K3S_BIN.part" && mv "$K3S_BIN.part" "$K3S_BIN"
  fi
}

# The unit the official installer would write, because the binary is
# carried in rather than installed from the network. Real systemd
# supervision is what lets a reboot row's docker start bring the node
# back through the distribution's own boot path.
write_k3s_unit() {
  local n="$1" role="$2" unit="k3s.service"
  [ "$role" = agent ] && unit="k3s-agent.service"
  write_to "$n" "/etc/systemd/system/$unit" <<EOF
[Unit]
Description=k3s ($role)
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/k3s $role
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
  in_node "$n" systemctl daemon-reload
  in_node "$n" systemctl enable --now "$unit" >/dev/null 2>&1 || in_node "$n" systemctl restart "$unit"
}

k3s_config_common() {
  cat <<EOF
node-ip: $2
node-name: $1
kube-proxy-arg:
  - "conntrack-max-per-core=0"
EOF
}

distro_build() {
  echo "--- the k3s binary, carried onto every site node ---"
  fetch_k3s
  echo "  $K3S_VERSION"
  for n in $SITE_NODES; do
    in_node "$n" test -x /usr/local/bin/k3s \
      || docker cp "$K3S_BIN" "$(c "$n")":/usr/local/bin/k3s
    in_node "$n" mkdir -p /etc/rancher/k3s
  done

  echo "--- first server on cp at $LAN.10 ---"
  write_to cp /etc/rancher/k3s/config.yaml <<EOF
cluster-init: true
tls-san: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
cluster-cidr: $POD_CIDR
service-cidr: $SVC_CIDR
$(k3s_network_lines)
disable: [traefik, servicelb, metrics-server, local-storage]
$(k3s_config_common cp "$LAN.10")
EOF
  write_k3s_unit cp server
  for _ in $(seq 1 60); do
    in_node cp test -f /etc/rancher/k3s/k3s.yaml && break
    sleep 5
  done
  in_node cp cat /etc/rancher/k3s/k3s.yaml 2>/dev/null \
    | sed "s|https://127.0.0.1:6443|https://$LAN.10:6443|" > "$OUT/kubeconfig"
  [ -s "$OUT/kubeconfig" ] || fail "cp never wrote a kubeconfig"
  install_bastion_kubectl
  echo "  up, kubeconfig at $OUT/kubeconfig"

  # The cluster token, for the other members. The site is provisioned
  # the way an operator provisions it; the REMOTE join is what must go
  # through the product's provider instead.
  TOKEN=$(in_node cp cat /var/lib/rancher/k3s/server/token | tr -d '\r\n')
  [ -n "$TOKEN" ] || fail "cp has no server token"

  echo "--- the other servers ---"
  for n in cp2 cp3; do
    ip=$(cp_addr "$n")
    if ! k get node "$n" >/dev/null 2>&1; then
      printf '%s' "$TOKEN" | docker exec -i "$(c "$n")" sh -c 'umask 077; cat >/etc/rancher/k3s/cluster-token'
      write_to "$n" /etc/rancher/k3s/config.yaml <<EOF
server: https://$LAN.10:6443
token-file: /etc/rancher/k3s/cluster-token
tls-san: [127.0.0.1, $LAN.10, $LAN.13, $LAN.14, cp, cp2, cp3]
cluster-cidr: $POD_CIDR
service-cidr: $SVC_CIDR
$(k3s_network_lines)
disable: [traefik, servicelb, metrics-server, local-storage]
$(k3s_config_common "$n" "$ip")
EOF
      write_k3s_unit "$n" server
    fi
  done

  echo "--- agents ---"
  for w in w1 w2; do
    ip=$LAN.$( [ "$w" = w1 ] && echo 11 || echo 12 )
    if ! k get node "$w" >/dev/null 2>&1; then
      printf '%s' "$TOKEN" | docker exec -i "$(c "$w")" sh -c 'umask 077; cat >/etc/rancher/k3s/cluster-token'
      write_to "$w" /etc/rancher/k3s/config.yaml <<EOF
server: https://$LAN.10:6443
token-file: /etc/rancher/k3s/cluster-token
$(k3s_config_common "$w" "$ip")
EOF
      write_k3s_unit "$w" agent
    fi
  done

  for _ in $(seq 1 60); do
    all=1
    for n in $SITE_NODES; do k get node "$n" >/dev/null 2>&1 || { all=; break; }; done
    [ -n "$all" ] && break
    sleep 5
  done
}

distro_kubelet_invariant() {
  # The agent's claim: kubelet dials the node's own loopback balancer
  # (which holds every server), and a server's kubelet dials its own
  # apiserver through the same local port. Either way the address is
  # the node's own, so no other node's death strands it.
  for n in $SITE_NODES; do
    server=$(in_node "$n" sh -c "grep -o 'server: .*' /var/lib/rancher/k3s/agent/kubelet.kubeconfig" 2>/dev/null | awk '{print $2}')
    case "$server" in
      https://127.0.0.1:*) echo "  $n $server" ;;
      *) fail "$n's kubelet dials ${server:-nothing}, a dependency on another node's survival" ;;
    esac
  done
}
