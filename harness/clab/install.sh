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
POD_CIDR=10.244.0.0/16
REPO_DIR="$(cd ../.. && pwd)"
OUT="${OUT:-$PWD/out}"
CAPI_VERSION="${CAPI_VERSION:-v1.11.1}"
TUNNEL_ENDPOINTS="${TUNNEL_ENDPOINTS:-kubernetes.io/hostname=w1}"
SITE_NODES="cp cp2 cp3 w1 w2"
CLOUD_NODES="remote1 remote2"
# The distribution the site was built as (cluster.sh), which is also
# which specialization mints join credentials: a k0s site hands out k0s
# tokens, and installing anything else would test a pairing no cluster
# has.
DISTRO="${DISTRO:-kubeadm}"
JOIN_PROVIDER="${JOIN_PROVIDER:-$DISTRO}"
# Which container network the cluster runs: a cni.d/<name>.sh
# installer, or "default" for a distribution that enabled its own
# built-in at build time (cluster.d honors the same variable), in
# which case there is nothing to install here. The controller detects
# whichever network is running from that network's own resources
# (pkg/cni), so nothing product-side is told.
CNI="${CNI:-calico}"

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
  # Through sudo, because a running distribution owns its data root:
  # k0s pre-creates its images directory as root, and a user-level
  # write into it is refused.
  local node="$1" image="$2" name dir
  name=$(echo "$image" | tr '/:' '__')
  case "$DISTRO" in
    kubeadm) docker save "$image" | docker exec -i "$(c "$node")" ctr -n k8s.io images import -; return ;;
    k0s)     dir="var-k0s/$node/images" ;;
    k3s)     dir="var-rancher/$node/k3s/agent/images" ;;
    rke2)    dir="var-rancher/$node/rke2/agent/images" ;;
    *) fail "no image import path for DISTRO=$DISTRO" ;;
  esac
  sudo mkdir -p "$dir" \
    && docker save "$image" | sudo tee "$dir/$name.tar" >/dev/null
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

# The standard CNI plugins, for the networks that delegate to them:
# flannel's conflist chains bridge, kube-router's does too, and the
# kindest image ships kind's own set (ptp and friends) with no bridge
# in it. Calico never noticed because it installs its own binaries.
# One tarball download on the host serves every node.
CNI_PLUGINS_VERSION="${CNI_PLUGINS_VERSION:-v1.6.2}"
ensure_cni_plugins() {
  local tgz="$OUT/binaries/cni-plugins-$CNI_PLUGINS_VERSION.tgz"
  if [ ! -f "$tgz" ]; then
    mkdir -p "$OUT/binaries"
    curl -fsSL -o "$tgz.part" \
      "https://github.com/containernetworking/plugins/releases/download/$CNI_PLUGINS_VERSION/cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz" \
      || fail "could not download the CNI plugins $CNI_PLUGINS_VERSION"
    mv "$tgz.part" "$tgz"
  fi
  local n
  for n in "$@"; do
    docker cp "$tgz" "$(c "$n")":/tmp/cni-plugins.tgz
    in_node "$n" tar -xzf /tmp/cni-plugins.tgz -C /opt/cni/bin
    in_node "$n" rm -f /tmp/cni-plugins.tgz
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

echo "--- the network ($CNI) ---"
# One installer per network, under cni.d/, each defining
# install_network; "default" means the distribution's own built-in was
# enabled at build time and there is nothing to install. The product's
# images ride in regardless: the dialer and busybox everywhere, the
# controller on the site.
echo "  preloading the dialer and busybox"
for n in $SITE_NODES $CLOUD_NODES; do
  preload "$n" cldt-dialer:e2e busybox:1.37
done
for n in $SITE_NODES; do preload "$n" cldt-controller:e2e; done

if [ "$CNI" != default ]; then
  [ -r "cni.d/$CNI.sh" ] \
    || fail "no installer for CNI=$CNI (have: $(ls cni.d/ | sed 's/\.sh$//' | tr '\n' ' ') default)"
  . "cni.d/$CNI.sh"
  install_network
fi
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
# The same authenticated /readyz probe as the kubectl wrapper (see
# cluster.sh for why nothing weaker works), through kubectl.real,
# which cluster.sh installed before this wrapper could exist.
for s in $LAN.10 $LAN.13 $LAN.14; do
  if /usr/local/bin/kubectl.real --server="https://\$s:6443" --request-timeout=2s \
       get --raw /readyz >/dev/null 2>&1; then
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
