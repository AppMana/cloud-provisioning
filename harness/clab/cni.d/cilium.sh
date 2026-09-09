# Cilium, rendered on the host with helm template (the bastion has no
# route to a chart repository) and applied like any other manifest.
# Tunnel (vxlan) routing, cilium's default, so the network
# ENCAPSULATES and the controller's cni package permits node
# addresses only; the per-node blocks come from cluster-pool IPAM,
# pinned here to the lab's pod CIDR so the allocator and the model
# agree. kube-proxy stays: replacing it is cilium's business on
# clusters built for that, and these builders all run kube-proxy.
#
# Sourced by install.sh.

CILIUM_VERSION="${CILIUM_VERSION:-1.16.5}"

install_network() {
  command -v helm >/dev/null || fail "no helm on the host to render cilium with"
  helm template cilium cilium --repo https://helm.cilium.io \
    --version "$CILIUM_VERSION" --namespace kube-system \
    --set operator.replicas=1 \
    --set ipam.mode=cluster-pool \
    --set ipam.operator.clusterPoolIPv4PodCIDRList="{$POD_CIDR}" \
    --set routingMode=tunnel \
    --set tunnelProtocol=vxlan \
    > "$OUT/cilium.yaml" || fail "rendering cilium $CILIUM_VERSION"
  local images
  images=$(grep -oE 'image: "?[^ "]+' "$OUT/cilium.yaml" | sed 's/image: "\?//' | sed 's/@sha256.*//' | sort -u)
  [ -n "$images" ] || fail "no images in the cilium render"
  echo "  preloading $(echo $images | wc -w) cilium images"
  for n in $SITE_NODES $CLOUD_NODES; do
    preload "$n" $images
  done
  docker cp "$OUT/cilium.yaml" "$(c bastion)":/tmp/cni.yaml
  k apply -f /tmp/cni.yaml >/dev/null || fail "installing cilium"
  k -n kube-system rollout status daemonset/cilium --timeout=8m >/dev/null \
    || fail "cilium never rolled out"
}
