# kube-router, CNI and routing only (the variant that keeps
# kube-proxy): native BGP routing between nodes, so the tunnel sees
# packets addressed to pods and the controller's cni package reads
# per-node blocks from node.spec.podCIDR, which is what kube-router
# announces.
#
# The interaction worth watching, and the reason this combination is
# in the matrix at all: the dialer REFUSES BGP across the tunnel by
# design (the tunnel does not carry the network's control plane), so
# kube-router's full mesh forms among the site nodes only, and the
# remote pod blocks reach the site through this operator's own
# mechanism: accept lists on the endpoints, derived transit routes on
# everyone else. If kube-router fights those routes instead of
# coexisting, this row is where it shows.
#
# Sourced by install.sh.

KUBE_ROUTER_MANIFEST="${KUBE_ROUTER_MANIFEST:-https://raw.githubusercontent.com/cloudnativelabs/kube-router/master/daemonset/kubeadm-kuberouter.yaml}"

install_network() {
  # The delegated plugins first: this network chains the standard
  # bridge plugin, which the node image does not ship.
  ensure_cni_plugins $SITE_NODES $CLOUD_NODES
  curl -fsSL "$KUBE_ROUTER_MANIFEST" -o "$OUT/kube-router.yaml" || fail "fetching kube-router"
  local images
  images=$(grep -oE 'image: [^ ]+' "$OUT/kube-router.yaml" | awk '{print $2}' | sort -u)
  [ -n "$images" ] || fail "no images in the kube-router manifest"
  echo "  preloading $(echo $images | wc -w) kube-router images"
  for n in $SITE_NODES $CLOUD_NODES; do
    preload "$n" $images
  done
  docker cp "$OUT/kube-router.yaml" "$(c bastion)":/tmp/cni.yaml
  k apply -f /tmp/cni.yaml >/dev/null || fail "installing kube-router"
  k -n kube-system rollout status daemonset/kube-router --timeout=5m >/dev/null \
    || fail "kube-router never rolled out"
}
