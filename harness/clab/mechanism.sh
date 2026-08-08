#!/usr/bin/env bash
# Who balances the API path is a property of the distribution.
# join-patterns/README.md tracks it as a table; this asserts the
# table's row held on the live remotes, because a table nobody checks
# is how a distribution quietly ends up with two balancers stacked, or
# none.
#
# Per distribution, on every provisioned remote:
#   kubeadm  the dialer's host unit carries --api-proxy-port, kubelet
#            dials 127.0.0.1:<port>, and that loopback answers /livez:
#            the operator's balancer, because kubeadm ships none.
#   k0s      no --api-proxy-port anywhere; k0s's own nllb balances.
#   k3s/rke2 no --api-proxy-port anywhere; the agent's client-side
#            balancer holds every server.
#
# Zero remotes is a failure, not a pass: an assertion that found
# nothing to assert on proved nothing.
set -euo pipefail
cd "$(dirname "$0")"

LAB=cldt
DISTRO="${DISTRO:-kubeadm}"
PROXY_PORT="${PROXY_PORT:-7445}"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
k() { in_node bastion kubectl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

remotes=$(k get nodes -l cloud-provisioning.appmana.com/role=cloud-worker \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' || true)
[ -n "$remotes" ] || fail "no provisioned remotes to assert on"

echo "--- who balances the API path on: $(echo $remotes | tr '\n' ' ') (DISTRO=$DISTRO) ---"
for r in $remotes; do
  unit=$(in_node "$r" systemctl cat wg-dialer 2>/dev/null) \
    || fail "$r has no wg-dialer unit to inspect"
  case "$DISTRO" in
    kubeadm)
      echo "$unit" | grep -q -- "--api-proxy-port=$PROXY_PORT" \
        || fail "$r's dialer unit carries no --api-proxy-port: nothing balances for kubeadm"
      server=$(in_node "$r" sh -c "grep -o 'server: .*' /etc/kubernetes/kubelet.conf" 2>/dev/null | awk '{print $2}')
      [ "$server" = "https://127.0.0.1:$PROXY_PORT" ] \
        || fail "$r's kubelet dials ${server:-nothing}, not its own balancer"
      # Reachability through it, not just its presence: a listener
      # whose every backend is wrong still listens.
      in_node "$r" curl -ksm 3 -o /dev/null "https://127.0.0.1:$PROXY_PORT/livez" \
        || fail "$r's loopback balancer does not answer /livez"
      echo "  $r: dialer balances on 127.0.0.1:$PROXY_PORT, kubelet dials it, it answers"
      ;;
    *)
      fail "no mechanism assertions written for DISTRO=$DISTRO: the table row exists, its check does not"
      ;;
  esac
done
