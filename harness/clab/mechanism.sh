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
CNI="${CNI:-calico}"
PROXY_PORT="${PROXY_PORT:-7445}"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
k() { in_node bastion kubectl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# What this row installed, in the terms the controller reports. The
# encapsulation is as much of the claim as the name: it decides
# whether a peer is permitted pod prefixes or node addresses alone,
# and getting it wrong publishes nothing while every component looks
# healthy (measured on kube-router, whose overlay the model believed
# in for two full rows).
expected_network() {
  case "$CNI" in
    calico)      echo "calico/native" ;;
    kube-router) echo "kube-router/native" ;;
    flannel)     echo "flannel/encapsulated" ;;
    cilium)      echo "cilium/encapsulated" ;;
    default)
      # The distribution's own network, named by what it actually
      # ships: k0s bundles kube-router, k3s embeds flannel, and RKE2's
      # canal is flannel carrying calico's policy, which the detector
      # must report as flannel rather than the calico its CRDs
      # advertise.
      case "$DISTRO" in
        k0s)      echo "kube-router/native" ;;
        k3s|rke2) echo "flannel/encapsulated" ;;
        *) fail "no built-in network is defined for $DISTRO" ;;
      esac ;;
    *) fail "no expected network for CNI=$CNI" ;;
  esac
}

# The model the mesh is actually running on, read from the controller
# rather than assumed. Absence is a failure, not a skip: an assertion
# that cannot find its evidence has proved nothing.
echo "--- the network the controller models (CNI=$CNI DISTRO=$DISTRO) ---"
want=$(expected_network)
got=$(k -n cloud-provisioning logs deploy/cloud-provisioning-endpoint-controller --tail=2000 2>/dev/null \
  | grep -oE "network=[a-z0-9-]+/[a-z]+" | tail -1)
[ -n "$got" ] || fail "the controller never reported which network it detected"
got=${got#network=}
[ "$got" = "$want" ] \
  || fail "the controller models $got, but this row installed $CNI on $DISTRO, which is $want"
echo "  $got, as installed"

remotes=$(k get nodes -l cloud-provisioning.appmana.com/role=cloud-worker \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' || true)
[ -n "$remotes" ] || fail "no provisioned remotes to assert on"

echo "--- who balances the API path on: $(echo $remotes | tr '\n' ' ') (DISTRO=$DISTRO) ---"
for r in $remotes; do
  unit=$(in_node "$r" systemctl cat wg-dialer 2>/dev/null) \
    || fail "$r has no wg-dialer unit to inspect"
  case "$DISTRO" in
    kubeadm)
      # The balancer is its own unit, its own lifecycle. The dialer's
      # unit must carry no trace of it: an API path that shares the
      # dialer's restarts is the coupling wg-apiproxy exists to end.
      proxy_unit=$(in_node "$r" systemctl cat wg-apiproxy 2>/dev/null) \
        || fail "$r has no wg-apiproxy unit: nothing balances for kubeadm"
      echo "$proxy_unit" | grep -q -- "--api-proxy-port=$PROXY_PORT" \
        || fail "$r's wg-apiproxy unit does not carry the balancer port"
      echo "$unit" | grep -q -- "--api-proxy-port" \
        && fail "$r's dialer unit still carries the balancer: the two lifecycles are meant to be decoupled"
      server=$(in_node "$r" sh -c "grep -o 'server: .*' /etc/kubernetes/kubelet.conf" 2>/dev/null | awk '{print $2}')
      [ "$server" = "https://127.0.0.1:$PROXY_PORT" ] \
        || fail "$r's kubelet dials ${server:-nothing}, not its own balancer"
      # Reachability through it, not just its presence: a listener
      # whose every backend is wrong still listens.
      in_node "$r" curl -ksm 3 -o /dev/null "https://127.0.0.1:$PROXY_PORT/livez" \
        || fail "$r's loopback balancer does not answer /livez"
      # The decoupling itself, measured: kill the dialer and the API
      # path must not blink. The tunnel is kernel state, the balancer
      # is another process; if this probe fails, kubelet's API path
      # dies with our most-frequently-restarted component.
      in_node "$r" systemctl stop wg-dialer
      if ! in_node "$r" curl -ksm 3 -o /dev/null "https://127.0.0.1:$PROXY_PORT/livez"; then
        in_node "$r" systemctl start wg-dialer
        fail "$r's balancer died with the dialer, the coupling wg-apiproxy exists to end"
      fi
      in_node "$r" systemctl start wg-dialer
      echo "  $r: wg-apiproxy balances on 127.0.0.1:$PROXY_PORT, kubelet dials it, and it survives the dialer's death"
      ;;
    k0s)
      echo "$unit" | grep -q -- "--api-proxy-port" \
        && fail "$r's dialer unit carries --api-proxy-port, a second balancer stacked on nllb"
      # The kubeconfig the running kubelet actually loads: nllb hands
      # it its own file under /run/k0s/nllb, aimed at the envoy on the
      # node's own loopback, k0s's choice of family included ([::1]).
      # /var/lib/k0s/kubelet.conf keeps the bootstrap-time join server
      # forever and proves nothing about the running node.
      server=$(in_node "$r" sh -c "grep -o 'server: .*' /run/k0s/nllb/kubeconfig.yaml" 2>/dev/null | awk '{print $2}')
      case "$server" in
        "https://[::1]:7443"|"https://127.0.0.1:7443") ;;
        *) fail "$r's kubelet dials ${server:-nothing}, not its own nllb envoy" ;;
      esac
      in_node "$r" sh -c 'curl -ksm 3 -o /dev/null https://127.0.0.1:7443/livez || curl -ksm 3 -o /dev/null "https://[::1]:7443/livez"' \
        || fail "$r's nllb envoy does not answer /livez"
      echo "  $r: nllb balances on the node's loopback ($server), kubelet dials it, it answers"
      ;;
    k3s|rke2)
      echo "$unit" | grep -q -- "--api-proxy-port" \
        && fail "$r's dialer unit carries --api-proxy-port, a second balancer stacked on the agent's own"
      # The agent's client-side balancer persists its server list in
      # its state file (k3s pkg/agent/loadbalancer: <dataDir>/etc/
      # <service>.json); every control plane must be in it, or the
      # balancer balances across less than the cluster.
      lb_state="/var/lib/rancher/$DISTRO/agent/etc/$DISTRO-agent-load-balancer.json"
      state=$(in_node "$r" cat "$lb_state" 2>/dev/null) \
        || fail "$r has no agent load-balancer state at $lb_state"
      for a in 10.10.0.10 10.10.0.13 10.10.0.14; do
        echo "$state" | grep -q "$a" \
          || fail "$r's agent balancer does not hold control plane $a; it has: $state"
      done
      server=$(in_node "$r" sh -c "grep -o 'server: .*' /var/lib/rancher/$DISTRO/agent/kubelet.kubeconfig" 2>/dev/null | awk '{print $2}')
      case "$server" in
        https://127.0.0.1:*) ;;
        *) fail "$r's kubelet dials ${server:-nothing}, not the agent's own loopback balancer" ;;
      esac
      echo "  $r: the agent balancer holds all three control planes, kubelet dials $server"
      ;;
    *)
      fail "no mechanism assertions written for DISTRO=$DISTRO: the table row exists, its check does not"
      ;;
  esac
done
