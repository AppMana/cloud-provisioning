# Flannel, from the stock kube-flannel manifest. VXLAN backend, so the
# network ENCAPSULATES: the tunnel sees packets addressed to nodes,
# never to pods, and the controller's cni package reads per-node
# blocks from node.spec.podCIDR (the allocation flannel itself routes
# by). The clusters here all allocate node CIDRs (podSubnet /
# cluster-cidr is set in every builder), which is flannel's one
# requirement.
#
# The stock manifest's Network defaults to 10.244.0.0/16, which is
# exactly this lab's POD_CIDR; asserted rather than assumed, because a
# flannel whose Network disagrees with the allocator routes nothing
# and says little.
#
# Sourced by install.sh.

FLANNEL_MANIFEST="${FLANNEL_MANIFEST:-https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml}"

install_network() {
  # The delegated plugins first: this network chains the standard
  # bridge plugin, which the node image does not ship.
  ensure_cni_plugins $SITE_NODES $CLOUD_NODES
  curl -fsSL "$FLANNEL_MANIFEST" -o "$OUT/flannel.yaml" || fail "fetching flannel"
  grep -q "\"Network\": \"$POD_CIDR\"" "$OUT/flannel.yaml" \
    || fail "the flannel manifest's Network is not $POD_CIDR; align it before installing"
  local images
  images=$(grep -oE 'image: [^ ]+' "$OUT/flannel.yaml" | awk '{print $2}' | sort -u)
  [ -n "$images" ] || fail "no images in the flannel manifest"
  echo "  preloading $(echo $images | wc -w) flannel images"
  for n in $SITE_NODES $CLOUD_NODES; do
    preload "$n" $images
  done
  docker cp "$OUT/flannel.yaml" "$(c bastion)":/tmp/cni.yaml
  k apply -f /tmp/cni.yaml >/dev/null || fail "installing flannel"
  k -n kube-flannel rollout status daemonset/kube-flannel-ds --timeout=5m >/dev/null \
    || fail "flannel never rolled out"
}
