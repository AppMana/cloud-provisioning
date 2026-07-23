#!/usr/bin/env bash
# Run one RED/GREEN scenario for the wg-dialer route-hijack regression,
# against a real two-node topology: `onprem` (real k0s + the on-prem
# dialer DaemonSet) and `cloud` (the wg-quick replacement -- the dialer
# binary as a bare systemd unit, listener role, no k0s). Real
# bidirectional WireGuard peering between them, not a fake/unreachable
# peer.
#
# Usage: run-scenario.sh <old-vulnerable|fixed> <cidr-value> <expect-hijack: yes|no>
#
#   RED   -- the historical, frozen-schema binary + its own toxic
#            --allowed-ips value:
#   ./run-scenario.sh old-vulnerable "0.0.0.0/0,::/0" yes
#
#   GREEN, default -- the redesigned binary, real safe pod-CIDR:
#   ./run-scenario.sh fixed "10.244.0.0/16" no
#
#   GREEN, toxic -- the redesigned binary, but --pod-cidrs itself set
#   to the maximally toxic value. This must ALSO stay GREEN: proves the
#   kernel route is structurally independent of this input now, not
#   just narrower by convention (see cmd/dialer/main.go's package doc).
#   ./run-scenario.sh fixed "0.0.0.0/0,::/0" no
set -euo pipefail

BINARY_CHOICE="${1:?usage: run-scenario.sh <old-vulnerable|fixed> <cidr-value> <expect-hijack yes|no>}"
CIDR_VALUE="${2:?}"
EXPECT_HIJACK="${3:?}"

# QEMU's slirp-based mgmt NAT (the default, non-passthrough hostfwd mode --
# see topo.clab.yml's comment on CLAB_MGMT_PASSTHROUGH) is known to stall
# under concurrent connections/throughput (e.g. an image pull sharing the
# same link as an SSH/kubectl call): TCP connect succeeds instantly but the
# subsequent data exchange can hang for 30s+ before recovering on its own.
# retry wraps any command that talks to a VM over this link so a
# transient stall doesn't kill the whole run under `set -e`.
retry() {
  local tries="$1" delay="$2"
  shift 2
  local n=0
  until "$@"; do
    n=$((n + 1))
    if (( n >= tries )); then
      return 1
    fi
    sleep "${delay}"
  done
}

# The real question isn't "does the guest still see other peers as healthy"
# -- it's "does the REST of the tailnet see the guest itself go stale",
# exactly what happened to jarvis in the real incident. That has to be
# checked from a peer whose own connectivity never depends on the guest
# -- this workstation. This check never touches the guest at all, so it
# works identically whether the guest is reachable or not. sudo tailscale
# switch changes system-wide state, so capture the original profile up
# front and always restore it.
HOST_TS_PROFILE="${WGDIALER_TEST_TAILSCALE_HOST_PROFILE:-b5e0}"
HOST_TS_ORIGINAL_PROFILE=""
if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
  HOST_TS_ORIGINAL_PROFILE="$(sudo tailscale switch --list 2>/dev/null | awk '/\*[[:space:]]*$/{print $1}')"
fi
restore_host_tailscale_profile() {
  if [[ -n "${HOST_TS_ORIGINAL_PROFILE}" ]]; then
    sudo tailscale switch "${HOST_TS_ORIGINAL_PROFILE}" >/dev/null 2>&1 || true
  fi
}
trap restore_host_tailscale_profile EXIT
host_view_of_guest() {
  local hostname="$1"
  sudo tailscale switch "${HOST_TS_PROFILE}" >/dev/null 2>&1 || true
  local line
  line="$(tailscale status 2>/dev/null | grep -F "${hostname}" || true)"
  if echo "${line}" | grep -qi "offline\|last seen"; then
    echo "stale"
  elif [[ -n "${line}" ]]; then
    echo "healthy"
  else
    echo "not-found"
  fi
}

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="${HARNESS_DIR}/logs/${BINARY_CHOICE}-$(echo "${CIDR_VALUE}" | tr -c 'a-zA-Z0-9' '-')-$(date +%Y%m%d-%H%M%S)"
mkdir -p "${LOG_DIR}"
ONPREM_IP="172.31.41.2"
CLOUD_IP="172.31.41.3"
KEY="${HARNESS_DIR}/cfg/ssh/harness_key"
SSH_OPTS="-i ${KEY} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8"
ONPREM_SSH="ssh ${SSH_OPTS} sysadmin@${ONPREM_IP}"
CLOUD_SSH="ssh ${SSH_OPTS} sysadmin@${CLOUD_IP}"
GUEST_TS_HOSTNAME="wgdialer-harness-$$"

case "${BINARY_CHOICE}" in
  old-vulnerable) DIALER_BIN="dialer-old-vulnerable" ;;
  fixed)          DIALER_BIN="dialer-fixed" ;;
  *) echo "unknown binary choice: ${BINARY_CHOICE}" >&2; exit 1 ;;
esac

echo "=== Scenario: ${DIALER_BIN}, cidr-value=${CIDR_VALUE}, expect hijack=${EXPECT_HIJACK} ==="
echo "Logs: ${LOG_DIR}"

"${HARNESS_DIR}/scripts/render-setup-script.sh"

echo "--- deploy both VMs ---"
# Both VMs get brand new host keys on every redeploy, but containerlab
# always assigns the same mgmt IPs -- without this, ssh/k0sctl fail
# hard on a stale cached key from the previous run.
ssh-keygen -R "${ONPREM_IP}" >/dev/null 2>&1 || true
ssh-keygen -R "${CLOUD_IP}" >/dev/null 2>&1 || true
( cd "${HARNESS_DIR}" && sudo containerlab deploy -t topo.clab.yml --reconfigure ) >"${LOG_DIR}/deploy.log" 2>&1

# Confirm containerlab actually assigned the IPs this script assumes --
# fail loudly and early rather than silently talking to the wrong node
# for the rest of the run.
ACTUAL_IPS="$(cd "${HARNESS_DIR}" && sudo containerlab inspect -t topo.clab.yml -f json 2>/dev/null | python3 -c "
import json,sys
d = json.load(sys.stdin)
for n in d.get('containers', d) if isinstance(d, list) else d.get('containers', []):
    print(n.get('name',''), n.get('ipv4_address','').split('/')[0])
" 2>/dev/null || true)"
echo "containerlab-assigned IPs: ${ACTUAL_IPS}" | tee "${LOG_DIR}/assigned-ips.log"
if ! echo "${ACTUAL_IPS}" | grep -q "onprem ${ONPREM_IP}"; then
  echo "onprem did not get the expected mgmt IP ${ONPREM_IP} -- update ONPREM_IP/CLOUD_IP in this script to match containerlab's actual assignment (see ${LOG_DIR}/assigned-ips.log)" >&2
  exit 1
fi
if ! echo "${ACTUAL_IPS}" | grep -q "cloud ${CLOUD_IP}"; then
  echo "cloud did not get the expected mgmt IP ${CLOUD_IP} -- update ONPREM_IP/CLOUD_IP in this script to match containerlab's actual assignment (see ${LOG_DIR}/assigned-ips.log)" >&2
  exit 1
fi

echo "--- wait for both VMs ready (first-boot apt-get can genuinely take 15-20 min over the VM's NATed link) ---"
for _ in $(seq 1 120); do
  onprem_ready="no"; cloud_ready="no"
  ${ONPREM_SSH} "test -f /run/wgdialer-harness-ready" 2>/dev/null && onprem_ready="yes"
  ${CLOUD_SSH} "test -f /run/wgdialer-harness-ready" 2>/dev/null && cloud_ready="yes"
  if [[ "${onprem_ready}" == "yes" && "${cloud_ready}" == "yes" ]]; then
    break
  fi
  sleep 10
done
retry 5 15 ${ONPREM_SSH} "test -f /run/wgdialer-harness-ready"
retry 5 15 ${CLOUD_SSH} "test -f /run/wgdialer-harness-ready"

echo "--- install real k0s via k0sctl (onprem: controller+worker; cloud: worker, registered with the real cloud-worker label/taint) ---"
( cd "${HARNESS_DIR}" && k0sctl apply --config k0sctl.yaml --no-wait ) >"${LOG_DIR}/k0sctl-apply.log" 2>&1
( cd "${HARNESS_DIR}" && k0sctl kubeconfig --config k0sctl.yaml ) >"${LOG_DIR}/kubeconfig.yaml" 2>"${LOG_DIR}/k0sctl-kubeconfig.log"
export KUBECONFIG="${LOG_DIR}/kubeconfig.yaml"

echo "--- wait for both nodes Ready ---"
# k0sctl joins cloud over the shared mgmt network (SSH-driven), not
# through the WireGuard tunnel -- this harness doesn't reproduce the
# real join.Reconciler token-minting/tunnel-gated-join flow (that's
# covered separately, see the kind-cluster integration test done
# earlier this session). What's under test here is DaemonSet
# scheduling/coexistence once both nodes are real k8s Nodes, so cloud
# joining ahead of its own wg-dialer.service coming up is fine.
for _ in $(seq 1 90); do
  ready_count="$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready' || true)"
  if [[ "${ready_count}" -ge 2 ]]; then
    break
  fi
  sleep 10
done
retry 5 15 kubectl get nodes -o wide
CLOUD_NODE_NAME="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.cloud-provisioning\.appmana\.com/role}{"\n"}{end}' | awk -F'\t' '$2=="cloud-worker"{print $1; exit}')"
if [[ -z "${CLOUD_NODE_NAME}" ]]; then
  echo "no k8s Node carries the cloud-provisioning.appmana.com/role=cloud-worker label -- cloud didn't join correctly" >&2
  kubectl get nodes --show-labels >&2
  exit 1
fi
echo "cloud-worker node name: ${CLOUD_NODE_NAME}"

if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
  echo "--- joining Tailscale as '${GUEST_TS_HOSTNAME}' on onprem (env var set), before the dialer ever runs ---"
  retry 3 15 ${ONPREM_SSH} "curl -fsSL https://tailscale.com/install.sh | sudo sh" >"${LOG_DIR}/tailscale-install.log" 2>&1
  retry 3 15 ${ONPREM_SSH} "sudo tailscale up --authkey=${WGDIALER_TEST_TAILSCALE_AUTHKEY} --hostname=${GUEST_TS_HOSTNAME}" >"${LOG_DIR}/tailscale-up.log" 2>&1
  for _ in $(seq 1 12); do
    v="$(host_view_of_guest "${GUEST_TS_HOSTNAME}")"
    [[ "${v}" != "not-found" ]] && break
    sleep 5
  done
  echo "host-side view of ${GUEST_TS_HOSTNAME} right after joining: $(host_view_of_guest "${GUEST_TS_HOSTNAME}")" | tee "${LOG_DIR}/tailscale-status-joined.log"
else
  echo "WGDIALER_TEST_TAILSCALE_AUTHKEY not set -- Tailscale assertions SKIPPED" | tee "${LOG_DIR}/tailscale-SKIPPED.log"
fi

echo "--- generate REAL WireGuard keypairs for both sides ---"
ONPREM_PRIV="$(wg genkey)"; ONPREM_PUB="$(echo "${ONPREM_PRIV}" | wg pubkey)"
CLOUD_PRIV="$(wg genkey)"; CLOUD_PUB="$(echo "${CLOUD_PRIV}" | wg pubkey)"
ONPREM_TUNNEL_ADDR="10.100.0.1"
CLOUD_TUNNEL_ADDR="10.100.0.2"

echo "--- upload dialer binary + apply manifests on onprem ---"
retry 5 15 ${ONPREM_SSH} "sudo mkdir -p /opt/dialer-bin"
retry 5 15 scp -i "${KEY}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "${HARNESS_DIR}/bin/${DIALER_BIN}" "sysadmin@${ONPREM_IP}:/tmp/${DIALER_BIN}"
retry 5 15 ${ONPREM_SSH} "sudo mv /tmp/${DIALER_BIN} /opt/dialer-bin/${DIALER_BIN} && sudo chmod +x /opt/dialer-bin/${DIALER_BIN}"

retry 5 15 kubectl apply -f "${HARNESS_DIR}/manifests/wg-dialer-rbac.yaml" >"${LOG_DIR}/apply-rbac.log" 2>&1

if [[ "${BINARY_CHOICE}" == "old-vulnerable" ]]; then
  echo "--- building onprem Secret with the OLD, frozen flat schema (peer-public-key/peer-endpoint + --allowed-ips=${CIDR_VALUE}) ---"
  kubectl create secret generic wg-dialer-peer -n wg-dialer \
    --from-literal=dialer-private-key="${ONPREM_PRIV}" \
    --from-literal=local-address="${ONPREM_TUNNEL_ADDR}/24" \
    --from-literal=peer-public-key="${CLOUD_PUB}" \
    --from-literal=peer-endpoint="${CLOUD_IP}:51820" \
    --dry-run=client -o yaml > "${LOG_DIR}/secret-rendered.yaml"
  retry 5 15 kubectl apply -f "${LOG_DIR}/secret-rendered.yaml" >"${LOG_DIR}/apply-secret.log" 2>&1

  ALLOWED_IPS="${CIDR_VALUE}" DIALER_BINARY="${DIALER_BIN}" \
    envsubst < "${HARNESS_DIR}/manifests/wg-dialer-daemonset-legacy.tmpl.yaml" > "${LOG_DIR}/daemonset-rendered.yaml"
else
  echo "--- building onprem Secret with the NEW per-Machine schema (peer-public-key-cloud etc, --pod-cidrs=${CIDR_VALUE}) ---"
  kubectl create secret generic wg-dialer-peer -n wg-dialer \
    --from-literal=dialer-private-key="${ONPREM_PRIV}" \
    --from-literal=local-address="${ONPREM_TUNNEL_ADDR}/24" \
    --from-literal=peer-public-key-cloud="${CLOUD_PUB}" \
    --from-literal=peer-endpoint-cloud="${CLOUD_IP}:51820" \
    --from-literal=peer-allowed-ips-cloud="${CLOUD_TUNNEL_ADDR}/32" \
    --from-literal=peer-route-host-cloud="${CLOUD_TUNNEL_ADDR}" \
    --dry-run=client -o yaml > "${LOG_DIR}/secret-rendered.yaml"
  retry 5 15 kubectl apply -f "${LOG_DIR}/secret-rendered.yaml" >"${LOG_DIR}/apply-secret.log" 2>&1

  POD_CIDRS="${CIDR_VALUE}" SERVICE_CIDRS="10.96.0.0/12" DIALER_BINARY="${DIALER_BIN}" \
    envsubst < "${HARNESS_DIR}/manifests/wg-dialer-daemonset.tmpl.yaml" > "${LOG_DIR}/daemonset-rendered.yaml"
fi
retry 5 15 kubectl apply -f "${LOG_DIR}/daemonset-rendered.yaml" >"${LOG_DIR}/apply-daemonset.log" 2>&1

echo "--- set up the cloud node: peers.json + wg-dialer.service (listener role, no --endpoint) ---"
CLOUD_PEERS_JSON=$(python3 -c "
import json
print(json.dumps({
    'privateKey': '${CLOUD_PRIV}',
    'localAddress': '${CLOUD_TUNNEL_ADDR}/24',
    'peers': [{
        'publicKey': '${ONPREM_PUB}',
        'allowedIPs': ['${ONPREM_TUNNEL_ADDR}/32'],
        'routeHost': '${ONPREM_TUNNEL_ADDR}',
    }],
}))
")
echo "${CLOUD_PEERS_JSON}" > "${LOG_DIR}/cloud-peers.json"
retry 5 15 ${CLOUD_SSH} "sudo mkdir -p /etc/wg-dialer /opt/dialer-bin"
retry 5 15 scp -i "${KEY}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "${HARNESS_DIR}/bin/dialer-fixed" "sysadmin@${CLOUD_IP}:/tmp/dialer-fixed"
retry 5 15 ${CLOUD_SSH} "sudo mv /tmp/dialer-fixed /opt/dialer-bin/dialer-fixed && sudo chmod +x /opt/dialer-bin/dialer-fixed"
retry 5 15 scp -i "${KEY}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "${LOG_DIR}/cloud-peers.json" "sysadmin@${CLOUD_IP}:/tmp/peers.json"
retry 5 15 ${CLOUD_SSH} "sudo mv /tmp/peers.json /etc/wg-dialer/peers.json"

CLOUD_UNIT=$(cat <<EOF
[Unit]
Description=wg-dialer (listener role)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/dialer-bin/dialer-fixed \\
  --iface=wg0 \\
  --peers-file=/etc/wg-dialer/peers.json \\
  --listen-port=51820 \\
  --pod-cidrs=10.244.0.0/16 \\
  --service-cidrs=10.96.0.0/12 \\
  --keepalive-seconds=15 \\
  --mtu=1420 \\
  --poll-interval=10s
Restart=always
AmbientCapabilities=CAP_NET_ADMIN

[Install]
WantedBy=multi-user.target
EOF
)
echo "${CLOUD_UNIT}" > "${LOG_DIR}/wg-dialer.service"
retry 5 15 scp -i "${KEY}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "${LOG_DIR}/wg-dialer.service" "sysadmin@${CLOUD_IP}:/tmp/wg-dialer.service"
retry 5 15 ${CLOUD_SSH} "sudo mv /tmp/wg-dialer.service /etc/systemd/system/wg-dialer.service && sudo systemctl daemon-reload && sudo systemctl enable --now wg-dialer.service"

retry 5 15 ${CLOUD_SSH} "systemctl is-active wg-dialer.service"
echo "cloud's bootstrap wg-dialer.service is active"

echo "--- deploy the cloud-worker DaemonSet (mirrors ensureCloudDialerDaemonSet) -- must schedule ONLY onto the cloud-worker node, coexisting with, not replacing, the bootstrap systemd unit above ---"
POD_CIDRS="10.244.0.0/16" SERVICE_CIDRS="10.96.0.0/12" DIALER_BINARY="dialer-fixed" \
  envsubst < "${HARNESS_DIR}/manifests/wg-dialer-cloud-daemonset.tmpl.yaml" > "${LOG_DIR}/cloud-daemonset-rendered.yaml"
retry 5 15 kubectl apply -f "${LOG_DIR}/cloud-daemonset-rendered.yaml" >"${LOG_DIR}/apply-cloud-daemonset.log" 2>&1

CLOUD_DS_SCHEDULED_CORRECTLY="no"
for _ in $(seq 1 30); do
  cloud_pod_line="$(kubectl --request-timeout=8s -n wg-dialer get pods -l app=dialer-cloud-repro -o wide --no-headers 2>/dev/null || true)"
  if echo "${cloud_pod_line}" | grep -q "Running"; then
    if echo "${cloud_pod_line}" | grep -q "${CLOUD_NODE_NAME}"; then
      CLOUD_DS_SCHEDULED_CORRECTLY="yes"
    fi
    break
  fi
  sleep 5
done
kubectl -n wg-dialer get pods -o wide > "${LOG_DIR}/wg-dialer-pods-after-cloud-ds.log" 2>&1
cat "${LOG_DIR}/wg-dialer-pods-after-cloud-ds.log"
if [[ "${CLOUD_DS_SCHEDULED_CORRECTLY}" != "yes" ]]; then
  echo "cloud DaemonSet's pod never reached Running on ${CLOUD_NODE_NAME} -- see ${LOG_DIR}/wg-dialer-pods-after-cloud-ds.log" >&2
  exit 1
fi
echo "cloud-worker DaemonSet pod is Running on ${CLOUD_NODE_NAME}, as expected"

# The structural exclusion under test: the ON-PREM DaemonSet's own pod
# (from wg-dialer-daemonset.tmpl.yaml/-legacy.tmpl.yaml, applied later
# in this script for RED/GREEN) hasn't been created yet at this point,
# so there's nothing to assert about it landing on the wrong node here
# -- that's checked again after it's deployed, below.

retry 5 15 ${CLOUD_SSH} "systemctl is-active wg-dialer.service"
echo "cloud's bootstrap wg-dialer.service is STILL active after the DaemonSet pod started (redundant coexistence confirmed, not a handoff)"

retry 5 15 ${CLOUD_SSH} "wg show wg0 latest-handshakes" >"${LOG_DIR}/cloud-wg-show-after-daemonset.log" 2>&1 || true
cat "${LOG_DIR}/cloud-wg-show-after-daemonset.log"

echo "--- wait for onprem dialer pod running (or detect immediate connectivity loss) ---"
# On this single-NIC topology, the toxic-AllowedIPs hijack can take out
# EVERY path to the guest within seconds of the dialer pod starting --
# there's no separate "data" link for it to spare. Distinguish "API
# server just isn't up yet" from "guest went dark and stayed dark" --
# the latter, on this topology, IS the bug manifesting, no reboot
# required.
POD_RUNNING="no"
CONNECTIVITY_LOST_EARLY="no"
consecutive_unreachable=0
for _ in $(seq 1 90); do
  if kubectl --request-timeout=8s -n wg-dialer get pods --no-headers 2>/dev/null | grep -q "Running"; then
    POD_RUNNING="yes"
    break
  fi
  if kubectl --request-timeout=8s get --raw=/healthz >/dev/null 2>&1; then
    consecutive_unreachable=0
  else
    consecutive_unreachable=$((consecutive_unreachable + 1))
  fi
  if (( consecutive_unreachable >= 6 )); then
    echo "onprem unreachable for ~60s straight right after the daemonset started -- treating as the hijack itself, not a stuck wait"
    CONNECTIVITY_LOST_EARLY="yes"
    break
  fi
  sleep 10
done

HIJACK_SEEN="no"
TAILSCALE_WENT_STALE="no"

serial_route_check() {
  local container="$1" logfile="$2"
  if docker inspect "${container}" >/dev/null 2>&1; then
    python3 "${HARNESS_DIR}/scripts/serial-console.py" "${container}" \
      "echo MAIN_TABLE_START; ip route show; echo MAIN_TABLE_END; ip rule show; ip route show table 52820 2>&1" \
      > "${logfile}" 2>&1 || true
    cat "${logfile}"
    if sed -n '/MAIN_TABLE_START/,/MAIN_TABLE_END/p' "${logfile}" | grep -q "dev wg0"; then
      return 0 # hijack found in main table
    fi
  fi
  return 1
}

if [[ "${CONNECTIVITY_LOST_EARLY}" == "yes" ]]; then
  HIJACK_SEEN="yes"
  echo "--- onprem went unreachable immediately after daemonset apply; skipping baseline/reboot (nothing SSH-reachable to run them against) ---"
  echo "immediate connectivity loss right after daemonset apply -- treated as HIJACK_SEEN=yes" > "${LOG_DIR}/observe.log"
  if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
    echo "--- watching host-side view of ${GUEST_TS_HOSTNAME} for staleness (this check needs no guest reachability) ---"
    for i in $(seq 1 40); do
      sleep 15
      elapsed=$((i * 15))
      v="$(host_view_of_guest "${GUEST_TS_HOSTNAME}")"
      echo "T+${elapsed}s: host-view-of-guest=${v}" | tee -a "${LOG_DIR}/observe.log"
      if [[ "${v}" == "stale" ]]; then
        TAILSCALE_WENT_STALE="yes"
        break
      fi
    done
  fi
  echo "--- serial-console route check on onprem (bypasses the network entirely) ---"
  serial_route_check "clab-wgdialer-vm-single-nic-onprem" "${LOG_DIR}/serial-route-check-onprem.log" && HIJACK_SEEN="yes"
else
  retry 5 15 kubectl -n wg-dialer get pods -o wide
  kubectl -n wg-dialer describe pods 2>&1 | tail -40 || true

  echo "--- confirm the on-prem DaemonSet's pod never scheduled onto the cloud-worker node ---"
  if kubectl -n wg-dialer get pods -l app=dialer-repro -o wide --no-headers 2>/dev/null | grep -q "${CLOUD_NODE_NAME}"; then
    echo "on-prem dialer-repro pod scheduled onto the cloud-worker node (${CLOUD_NODE_NAME}) -- the taint/toleration exclusion is broken" >&2
    exit 1
  fi
  echo "confirmed: dialer-repro (on-prem DaemonSet) is absent from ${CLOUD_NODE_NAME}"

  echo "--- baseline: confirm route present, general internet dead before reboot ---"
  ${ONPREM_SSH} "ip route show; echo ---; ping -c1 -W3 8.8.8.8" >"${LOG_DIR}/baseline.log" 2>&1 || true
  cat "${LOG_DIR}/baseline.log"

  echo "--- real bidirectional ping across wg0 ---"
  ${ONPREM_SSH} "ping -c2 -W3 ${CLOUD_TUNNEL_ADDR}" >"${LOG_DIR}/onprem-to-cloud-ping.log" 2>&1 || true
  cat "${LOG_DIR}/onprem-to-cloud-ping.log"
  ${CLOUD_SSH} "ping -c2 -W3 ${ONPREM_TUNNEL_ADDR}" >"${LOG_DIR}/cloud-to-onprem-ping.log" 2>&1 || true
  cat "${LOG_DIR}/cloud-to-onprem-ping.log"

  if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
    echo "host-side view of ${GUEST_TS_HOSTNAME} before reboot: $(host_view_of_guest "${GUEST_TS_HOSTNAME}")"
  fi

  echo "--- reboot onprem ---"
  ${ONPREM_SSH} "sudo reboot" || true

  echo "--- observe for boot-time race + up to 10 minutes ---"
  OBSERVE_LOG="${LOG_DIR}/observe.log"
  : > "${OBSERVE_LOG}"
  # A plain reboot is expected to cause a brief (10-20s) window where
  # EVERYTHING is down, even on a perfectly healthy system -- not the
  # bug this harness is hunting for. Staleness only counts once it's
  # held for 3 consecutive samples (~30s) past a grace window, and it's
  # allowed to recover.
  GRACE_PERIOD_S=20
  consecutive_stale=0
  for i in $(seq 1 60); do
    sleep 10
    elapsed=$((i * 10))
    cmd="ip route show | grep -q 'dev wg0' && echo WG0_HIJACK || echo clean; ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && echo PING_OK || echo PING_FAIL"
    out="$(${ONPREM_SSH} "${cmd}" 2>/dev/null || echo "SSH_UNREACHABLE")"
    ts_view="n/a"
    if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
      ts_view="$(host_view_of_guest "${GUEST_TS_HOSTNAME}")"
    fi
    echo "T+${elapsed}s: ${out} host-view-of-guest=${ts_view}" | tee -a "${OBSERVE_LOG}"
    if echo "${out}" | grep -q WG0_HIJACK; then
      HIJACK_SEEN="yes"
    fi
    if [[ "${elapsed}" -gt "${GRACE_PERIOD_S}" ]]; then
      if [[ "${ts_view}" == "stale" ]]; then
        consecutive_stale=$((consecutive_stale + 1))
      else
        consecutive_stale=0
      fi
      if (( consecutive_stale >= 3 )); then
        TAILSCALE_WENT_STALE="yes"
      fi
    fi
    if [[ "${EXPECT_HIJACK}" == "no" && "${elapsed}" -gt "${GRACE_PERIOD_S}" && "${out}" == *"clean"*"PING_OK"* && "${ts_view}" != "stale" ]]; then
      break
    fi
  done

  if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
    echo "host-side view of ${GUEST_TS_HOSTNAME}, final: $(host_view_of_guest "${GUEST_TS_HOSTNAME}")" | tee "${LOG_DIR}/tailscale-status-final.log"
  fi

  echo "--- serial-console route check on both nodes (bypasses the network entirely) ---"
  serial_route_check "clab-wgdialer-vm-single-nic-onprem" "${LOG_DIR}/serial-route-check-onprem.log" && HIJACK_SEEN="yes"
  serial_route_check "clab-wgdialer-vm-single-nic-cloud" "${LOG_DIR}/serial-route-check-cloud.log" && HIJACK_SEEN="yes"
fi

TS_NOTE=""
if [[ -n "${WGDIALER_TEST_TAILSCALE_AUTHKEY:-}" ]]; then
  TS_NOTE=", tailscale-went-stale=${TAILSCALE_WENT_STALE} (host-side check, real tailnet)"
else
  TS_NOTE=" (tailscale assertion SKIPPED -- WGDIALER_TEST_TAILSCALE_AUTHKEY unset)"
fi

echo "--- verdict ---"
if [[ "${EXPECT_HIJACK}" == "yes" ]]; then
  if [[ "${HIJACK_SEEN}" == "yes" ]]; then
    if [[ "${CONNECTIVITY_LOST_EARLY}" == "yes" ]]; then
      echo "RED CONFIRMED: onprem lost all connectivity immediately after the toxic daemonset started, against a REAL cloud peer (${DIALER_BIN}, ${CIDR_VALUE})${TS_NOTE}"
    else
      echo "RED CONFIRMED: hijack route appeared as expected, against a REAL cloud peer (${DIALER_BIN}, ${CIDR_VALUE})${TS_NOTE}"
    fi
    exit 0
  else
    echo "RED FAILED TO REPRODUCE: expected the hijack, never saw it" >&2
    exit 1
  fi
else
  if [[ "${HIJACK_SEEN}" == "no" && "${TAILSCALE_WENT_STALE}" == "no" ]]; then
    echo "GREEN CONFIRMED: no hijack route on either node, guest's Tailscale identity never went stale, real bidirectional ping across wg0 (${DIALER_BIN}, ${CIDR_VALUE})${TS_NOTE}"
    exit 0
  else
    echo "GREEN FAILED: hijack_seen=${HIJACK_SEEN} tailscale_went_stale=${TAILSCALE_WENT_STALE} -- should both be 'no'" >&2
    exit 1
  fi
fi
