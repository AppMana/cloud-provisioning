#!/usr/bin/env bash
# Build the cluster on the site's five nodes: three control planes and
# two workers, as whichever distribution DISTRO names.
#
# Three control planes, not two: stacked etcd needs a majority, and a
# majority of two is two, so a second control plane only adds a way to
# halt. Three is the smallest number that tolerates a death. Every
# distribution here runs stacked etcd or its equivalent, so the count
# is the same count everywhere.
#
# How the site is built is the distribution's business, so each one
# lives in cluster.d/<distro>.sh and defines two functions:
#
#   distro_build             init, joins, and $OUT/kubeconfig; calls
#                            install_bastion_kubectl once the API answers
#   distro_kubelet_invariant no kubelet depends on another node's
#                            survival, checked wherever that
#                            distribution keeps its kubelet's config
#
# What does not vary is asserted here: every node registered, no
# cross-node kubelet dependency, and every registration by a segment
# address. The management interface exists so this script can drive the
# lab and for nothing else.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
LAN=10.10.0
POD_CIDR=10.244.0.0/16
SVC_CIDR=10.96.0.0/12
DISTRO="${DISTRO:-kubeadm}"
OUT="${OUT:-$PWD/out}"
mkdir -p "$OUT"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
# Anything fed on stdin needs -i, or the command succeeds having read
# nothing and the file it was meant to write is empty.
write_to() { docker exec -i "$(c "$1")" sh -c "cat >$2"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Everything that talks to the API talks to it from the bastion, which
# is on the site network. The wrapper below picks a live control plane
# per invocation, because the bastion is not a cluster node and runs no
# forwarder of its own.
k() { in_node bastion kubectl "$@"; }

SITE_NODES="cp cp2 cp3 w1 w2"
CP_ADDRS="$LAN.10 $LAN.13 $LAN.14"
cp_addr() {
  case "$1" in
    cp)  echo "$LAN.10" ;;
    cp2) echo "$LAN.13" ;;
    cp3) echo "$LAN.14" ;;
  esac
}

# The bastion is not a cluster node: nothing serves its loopback, and
# its kubeconfig names whatever node-local endpoint the distribution
# chose. Pick a live member per invocation instead. The real addresses
# are in every server certificate's SANs, so --server needs no other
# accommodation. Called by the distribution once $OUT/kubeconfig exists.
install_bastion_kubectl() {
  in_node bastion mkdir -p /root/.kube
  write_to bastion /root/.kube/config < "$OUT/kubeconfig"
  if ! in_node bastion test -f /usr/local/bin/kubectl.real; then
    # command -v is a shell builtin, so it needs a shell to run in; the
    # copy lands the original wherever it was found, and the wrapper
    # then shadows it from /usr/local/bin.
    in_node bastion sh -c 'cp "$(command -v kubectl)" /usr/local/bin/kubectl.real' \
      || fail "no kubectl on the bastion to wrap"
  fi
  write_to bastion /usr/local/bin/kubectl <<EOF
#!/bin/sh
# Pick a READY control plane, then run the real kubectl against it. A
# probe failure is connectivity or a member that cannot serve yet, so
# trying the next member is right; a kubectl failure after a good
# probe is an answer, not a reason to ask someone else.
#
# The probe is an AUTHENTICATED /readyz, with this kubeconfig's own
# credentials, because nothing weaker distinguishes ready from not:
# /livez passes while a returning member's authorizer is still
# syncing and every request then comes back Forbidden (measured,
# kubeadm), and an anonymous /readyz is 401 on k0s before readiness
# is ever consulted, so with -f every member failed the probe and the
# wrapper fell through to the kubeconfig's default server, which was
# exactly the member that was down: the harness went blind and called
# it "cp never went NotReady" (measured, k0s). A 200 on an
# authenticated /readyz proves the member serves AND authorizes.
for s in $CP_ADDRS; do
  if /usr/local/bin/kubectl.real --server="https://\$s:6443" --request-timeout=2s \
       get --raw /readyz >/dev/null 2>&1; then
    exec /usr/local/bin/kubectl.real --server="https://\$s:6443" "\$@"
  fi
done
exec /usr/local/bin/kubectl.real "\$@"
EOF
  in_node bastion chmod 0755 /usr/local/bin/kubectl
  k get --raw /healthz >/dev/null 2>&1 || fail "the API server is not answering"
}

[ -r "cluster.d/$DISTRO.sh" ] \
  || fail "no site builder for DISTRO=$DISTRO (have: $(ls cluster.d/ | sed 's/\.sh$//' | tr '\n' ' '))"
. "cluster.d/$DISTRO.sh"

distro_build

# Readiness is not asserted here and cannot be: a node with no network
# installed is legitimately NotReady, and this runs before the network.
# What can be asserted now is that every node registered, and by which
# address. install.sh waits for Ready once there is a network to be
# ready for, and fails there.
for n in $SITE_NODES; do
  k get node "$n" >/dev/null 2>&1 || fail "$n never registered"
done

echo "--- no kubelet depends on another node ---"
# The claim the design makes, checked rather than assumed, in whichever
# file this distribution's kubelet reads its server from.
distro_kubelet_invariant

echo "--- every node registered by its segment address ---"
k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' |
  while read -r name ip; do
    case "$ip" in
      $LAN.*) echo "  $name $ip" ;;
      *) fail "$name registered $ip, which is not a site address: the cluster formed on the wrong network" ;;
    esac
  done
