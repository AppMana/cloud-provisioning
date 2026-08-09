# Calico, from the stock manifest, carried in and switched to native
# routing. The controller's cni package detects it from its own
# resources and reads per-node blocks from Calico's IPAM.
#
# Sourced by install.sh, which provides the helpers (k, preload, fail,
# LAN, OUT, node lists) and calls install_network between the CRDs and
# the site-Ready wait.

CALICO_MANIFEST="${CALICO_MANIFEST:-https://raw.githubusercontent.com/projectcalico/calico/v3.29.1/manifests/calico.yaml}"

install_network() {
  curl -fsSL "$CALICO_MANIFEST" -o "$OUT/calico.yaml" || fail "fetching calico"
  # No per-distribution CNI path overrides, k3s included. k3s moves its
  # CNI directories into its data dir ONLY when its embedded flannel
  # runs: the assignment sits inside the flannel branch
  # (pkg/executor/embed/embed.go, "if Flannel.Backend != BackendNone"),
  # so with flannel-backend none the dirs stay unset and k3s's
  # containerd falls back to the stock /etc/cni/net.d and /opt/cni/bin.
  # Measured before reading the branch: redirected hostPaths delivered
  # Calico's conflist and binaries into the data dir perfectly, and
  # containerd, looking at the stock paths, said "cni plugin not
  # initialized" on every node.
  local images
  images=$(grep -oE 'image: [^ ]+' "$OUT/calico.yaml" | awk '{print $2}' | sort -u)
  [ -n "$images" ] || fail "no images in the calico manifest"
  echo "  preloading $(echo $images | wc -w) calico images"
  for n in $SITE_NODES $CLOUD_NODES; do
    preload "$n" $images
  done

  docker cp "$OUT/calico.yaml" "$(c bastion)":/tmp/cni.yaml
  k apply -f /tmp/cni.yaml >/dev/null || fail "installing calico"
  # Autodetect the address a node reaches the cluster by, not the first
  # interface that has one. On a site node the two agree. On a remote
  # they cannot: first-found lands on the interface that faces the
  # internet, an address the site has no route to, and calico's address
  # monitor re-detects at every interface change, so it restates that
  # address exactly when a tunnel moves. can-reach follows the route to
  # the API server, which on a remote is the tunnel, so the monitor's
  # own re-detection converges on the address the mesh gave the node.
  k -n kube-system set env daemonset/calico-node \
    IP_AUTODETECTION_METHOD="can-reach=$LAN.10" >/dev/null \
    || fail "setting calico's autodetection method"
  # The stock manifest encapsulates. This mesh carries pod traffic
  # natively, and the model the controller reads back has to match what
  # the network actually does.
  for _ in $(seq 1 48); do k get ippools.crd.projectcalico.org default-ipv4-ippool >/dev/null 2>&1 && break; sleep 5; done
  k patch ippools.crd.projectcalico.org default-ipv4-ippool --type merge \
    -p '{"spec":{"ipipMode":"Never","vxlanMode":"Never"}}' >/dev/null || fail "setting the pool's mode"
  # Waited for, not fired and forgotten. calico-node programs its routes
  # from the pool it saw when it started, so a pool changed afterwards
  # leaves the old encapsulation's routes in place. The node readiness
  # check below is satisfied by the pods that are already running, so
  # without this the harness reports a cluster whose routes and whose
  # model disagree, and says nothing.
  k -n kube-system rollout restart daemonset/calico-node >/dev/null 2>&1 \
    || fail "could not restart calico-node after changing the pool"
  k -n kube-system rollout status daemonset/calico-node --timeout=5m >/dev/null \
    || fail "calico-node did not come back after the pool change, so its routes still describe the old encapsulation"
}
