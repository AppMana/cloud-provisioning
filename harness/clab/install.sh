#!/usr/bin/env bash
# Put the product on the lab.
#
# Nothing here reaches a registry from inside the site, because the site
# has no route to one. Every image is carried in from this host, which
# is both faster and the only thing that works.
#
# Cluster API's controllers are not installed and are not needed: the
# join path never talks to them. What is needed is the Cluster and
# Machine CRDs, because the controllers watch those kinds and a manager
# cannot start an informer for a kind the API server does not serve.
# Applying two CRD files is also the only way to avoid clusterctl, which
# fetches its manifests from GitHub.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
NS=cloud-provisioning
LAN=10.10.0
REPO_DIR="$(cd ../.. && pwd)"
OUT="${OUT:-$PWD/out}"
CAPI_VERSION="${CAPI_VERSION:-v1.11.1}"
CALICO_MANIFEST="${CALICO_MANIFEST:-https://raw.githubusercontent.com/projectcalico/calico/v3.29.1/manifests/calico.yaml}"
TUNNEL_ENDPOINTS="${TUNNEL_ENDPOINTS:-kubernetes.io/hostname=w1}"
SITE_NODES="cp cp2 cp3 w1 w2"
CLOUD_NODES="remote1 remote2"
# The distribution the site was built as (cluster.sh), which is also
# which specialization mints join credentials: a k0s site hands out k0s
# tokens, and installing anything else would test a pairing no cluster
# has.
DISTRO="${DISTRO:-kubeadm}"
JOIN_PROVIDER="${JOIN_PROVIDER:-$DISTRO}"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
write_to() { docker exec -i "$(c "$1")" sh -c "cat >$2"; }
k() { in_node bastion kubectl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# An image on this host, whichever registry will part with it. Docker
# Hub rate limits by address, and the same images are on quay, so a name
# that implies Docker Hub is retried there and retagged.
ensure_image() {
  local image="$1"
  docker image inspect "$image" >/dev/null 2>&1 && return 0
  docker pull -q "$image" >/dev/null 2>&1 && return 0
  case "$image" in */*.*/*) return 1 ;; esac
  docker pull -q "quay.io/$image" >/dev/null 2>&1 || return 1
  docker tag "quay.io/$image" "$image"
}

# Where an import lands depends on whose containerd runs the node's
# pods. kubeadm nodes run the image's stock containerd, always
# present, so the import goes through its ctr. The self-installing
# distributions bring their runtime with the join, so on a remote
# there is nothing to exec into at install time; instead, every one
# of them auto-imports tarballs from an images directory inside its
# data root, which is a host bind here (see topo.clab.yml). Writing
# the tarball there from the host works before the distribution
# exists on the node and after: a running k0s watches the directory
# (its OCIBundleReconciler says so in the journal), and k3s/RKE2 scan
# theirs at agent start, which is exactly when a joining remote needs
# the images to appear.
import_into() {
  local node="$1" image="$2" name
  name=$(echo "$image" | tr '/:' '__')
  case "$DISTRO" in
    kubeadm) docker save "$image" | docker exec -i "$(c "$node")" ctr -n k8s.io images import - ;;
    k0s)     mkdir -p "var-k0s/$node/images" \
               && docker save "$image" -o "var-k0s/$node/images/$name.tar" ;;
    k3s)     mkdir -p "var-rancher/$node/k3s/agent/images" \
               && docker save "$image" -o "var-rancher/$node/k3s/agent/images/$name.tar" ;;
    rke2)    mkdir -p "var-rancher/$node/rke2/agent/images" \
               && docker save "$image" -o "var-rancher/$node/rke2/agent/images/$name.tar" ;;
    *) fail "no image import path for DISTRO=$DISTRO" ;;
  esac
}

preload() {
  local node="$1"; shift
  local image
  for image in "$@"; do
    [ -n "$image" ] || continue
    ensure_image "$image" || fail "could not obtain $image from any registry"
    import_into "$node" "$image" >/dev/null 2>&1 \
      || fail "could not import $image into $node"
  done
}

echo "--- building ---"
( cd "$REPO_DIR" && docker build -q --target dialer -t cldt-dialer:e2e -f controller/Dockerfile . >/dev/null \
  && docker build -q --target endpoint-controller -t cldt-controller:e2e -f controller/Dockerfile . >/dev/null ) \
  || fail "image build"
mkdir -p "$OUT/binaries"
( cd "$REPO_DIR/controller" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o "$OUT/binaries/wg-dialer-linux-amd64" ./cmd/dialer ) || fail "dialer binary"
BIN_SHA=$(sha256sum "$OUT/binaries/wg-dialer-linux-amd64" | awk '{print $1}')
echo "  dialer $BIN_SHA"

echo "--- cluster api's two kinds, without its controllers ---"
# Fetched once on the host, which does have a route out, then applied
# from the bastion, which does not.
if [ ! -f "$OUT/capi-crds.yaml" ]; then
  curl -fsSL "https://github.com/kubernetes-sigs/cluster-api/releases/download/$CAPI_VERSION/cluster-api-components.yaml" \
    -o "$OUT/capi-components.yaml" || fail "fetching cluster-api components"
  python3 - "$OUT/capi-components.yaml" "$OUT/capi-crds.yaml" <<'PY'
import sys, yaml
want = {"clusters.cluster.x-k8s.io", "machines.cluster.x-k8s.io"}
docs = [d for d in yaml.safe_load_all(open(sys.argv[1]))
        if d and d.get("kind") == "CustomResourceDefinition" and d["metadata"]["name"] in want]
# Conversion webhooks point at a controller that is not installed here,
# and the API server refuses to serve a kind whose webhook it cannot
# reach. Only one version is served anyway.
for d in docs:
    d["spec"].get("conversion", {}).clear()
    d["spec"]["conversion"] = {"strategy": "None"}
    d["spec"]["versions"] = [v for v in d["spec"]["versions"] if v.get("served")]
    for v in d["spec"]["versions"]:
        v["storage"] = v["name"] == d["spec"]["versions"][-1]["name"]
    d["metadata"].get("annotations", {}).pop("cert-manager.io/inject-ca-from", None)
yaml.safe_dump_all(docs, open(sys.argv[2], "w"))
print(f"  {len(docs)} crds")
PY
fi
docker cp "$OUT/capi-crds.yaml" "$(c bastion)":/tmp/capi-crds.yaml
k apply -f /tmp/capi-crds.yaml >/dev/null || fail "applying cluster api crds"
docker cp crds/containernet.yaml "$(c bastion)":/tmp/containernet.yaml
k apply -f /tmp/containernet.yaml >/dev/null || fail "applying containernet crds"
echo "  cluster, machine, and the containernet kinds"

echo "--- the network ---"
curl -fsSL "$CALICO_MANIFEST" -o "$OUT/calico.yaml" || fail "fetching calico"
# k3s keeps its CNI directories inside its own data dir (verified in
# k3s pkg/executor/embed/embed.go: conf under agent/etc/cni/net.d, bin
# beside its bundled host-local, reachable through the data/current
# symlink), so the stock manifest's hostPaths would install Calico
# where k3s never looks. kubeadm, k0s and RKE2 all use the stock
# paths.
if [ "$DISTRO" = k3s ]; then
  sed -i \
    -e 's|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|' \
    -e 's|path: /opt/cni/bin|path: /var/lib/rancher/k3s/data/current/bin|' \
    "$OUT/calico.yaml"
fi
CNI_IMAGES=$(grep -oE 'image: [^ ]+' "$OUT/calico.yaml" | awk '{print $2}' | sort -u)
[ -n "$CNI_IMAGES" ] || fail "no images in the calico manifest"
echo "  preloading $(echo $CNI_IMAGES | wc -w) network images plus the dialer and busybox"
for n in $SITE_NODES $CLOUD_NODES; do
  preload "$n" $CNI_IMAGES cldt-dialer:e2e busybox:1.37
done
for n in $SITE_NODES; do preload "$n" cldt-controller:e2e; done

docker cp "$OUT/calico.yaml" "$(c bastion)":/tmp/calico.yaml
k apply -f /tmp/calico.yaml >/dev/null || fail "installing calico"
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
for n in $SITE_NODES; do k wait --for=condition=Ready node/"$n" --timeout=420s >/dev/null 2>&1 || fail "$n never became ready"; done
echo "  every site node ready"

echo "--- the chart ---"
# Installed from the bastion, like everything else that talks to the
# cluster. The host cannot reach the API server and should not be able
# to. helm is carried in rather than downloaded, because the site has no
# route to anywhere to download it from.
# helm reads the kubeconfig's server, which names the loopback
# forwarder no one serves on the bastion; like kubectl (see
# cluster.sh), it gets a wrapper that picks a live control plane per
# invocation. The kubeconfig's credentials and CA still apply, and the
# members' real addresses are in every server certificate's SANs.
docker cp "$(command -v helm)" "$(c bastion)":/usr/local/bin/helm.real
write_to bastion /usr/local/bin/helm <<EOF
#!/bin/sh
# /readyz with -f, same as the kubectl wrapper (see cluster.sh): a
# member back from an outage answers /livez while its authorizer is
# still syncing, and a release operation sent there fails Forbidden.
for s in $LAN.10 $LAN.13 $LAN.14; do
  if curl -ksfm 2 -o /dev/null "https://\$s:6443/readyz" 2>/dev/null; then
    exec /usr/local/bin/helm.real --kube-apiserver="https://\$s:6443" "\$@"
  fi
done
exec /usr/local/bin/helm.real "\$@"
EOF
in_node bastion chmod 0755 /usr/local/bin/helm
docker cp "$REPO_DIR/charts/cloud-provisioning" "$(c bastion)":/tmp/chart
in_node bastion helm upgrade --install cloud-provisioning /tmp/chart \
  --namespace "$NS" --create-namespace --wait --timeout 6m \
  --set image.repository=cldt-controller --set image.tag=e2e --set image.pullPolicy=Never \
  --set dialerImage.repository=cldt-dialer --set dialerImage.tag=e2e \
  --set tunnel.endpoints="$TUNNEL_ENDPOINTS" \
  --set joinProvider="$JOIN_PROVIDER" \
  --set dialerBinary.amd64.url="file:///opt/dialer-dist/wg-dialer-linux-amd64" \
  --set dialerBinary.amd64.sha256="$BIN_SHA" \
  >/dev/null || fail "installing the chart"
echo "  installed, endpoints $TUNNEL_ENDPOINTS"

echo
echo "the site runs the product; nothing on it pulled an image from a registry"
