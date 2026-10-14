#!/usr/bin/env bash
# Run only inside a disposable Linux VM. Never changes its management interface.
set -euo pipefail
guard=${1:?absolute network-guard binary}
management=${2:?management probe IP}
[[ $EUID = 0 && $guard = /* ]] || exit 2
command -v python3 >/dev/null
if ip link show ng-guard-test >/dev/null 2>&1; then
  echo 'Refusing to reuse an existing test interface' >&2
  exit 1
fi
root=$(mktemp -d /var/tmp/network-guard-native.XXXXXXXX)
chmod 700 "$root"
units=()
sampler=
cleanup() {
  for unit in "${units[@]}"; do systemctl stop "$unit" >/dev/null 2>&1 || true; done
  if [[ -n $sampler ]]; then kill "$sampler" 2>/dev/null || true; fi
  ip link delete ng-guard-test
}
trap cleanup EXIT
ip link add ng-guard-test type dummy
ip link set ng-guard-test up mtu 1420
ping -n -i 0.2 -w 40 "$management" > "$root/management.log" &
sampler=$!
for repetition in 1 2 3; do
  run="$root/run-$repetition"
  mkdir -m 700 "$run"
  printf '1420\n' > "$run/mtu"
  chmod 600 "$run/mtu"
  cat > "$run/restore" <<RESTORE
#!/bin/sh
set -eu
ip link set ng-guard-test mtu "\$(cat '$run/mtu')"
RESTORE
  chmod 700 "$run/restore"
  candidate=$(printf '1280\n' | sha256sum | cut -d ' ' -f1)
  python3 - "$run" "$candidate" <<'PY'
import json,sys
from pathlib import Path
root=Path(sys.argv[1])
(root/'plan.json').write_text(json.dumps({'restore':str(root/'restore'),'files':[{'path':str(root/'mtu'),'candidateSHA256':sys.argv[2]}]}))
PY
  "$guard" -state "$run/state" -plan "$run/plan.json" -within 8s prepare > "$run/prepared.json"
  unit="network-guard-native-${root##*.}-$repetition"
  units+=("$unit")
  systemd-run --quiet --unit "$unit" --property Restart=on-failure --property RestartSec=200ms \
    --property 'RestrictAddressFamilies=AF_UNIX AF_NETLINK' \
    "$guard" -state "$run/state" watch
  for attempt in $(seq 1 50); do
    [[ ! -f "$run/state/ready.json" ]] || break
    sleep 0.1
  done
  test -f "$run/state/ready.json"
  python3 - "$run/state/state.json" <<'PY'
import json,sys
assert json.load(open(sys.argv[1]))['outcome']=='armed'
PY
  printf '1280\n' > "$run/mtu"
  ip link set ng-guard-test mtu 1280
  [[ $(cat /sys/class/net/ng-guard-test/mtu) = 1280 ]]
  if [[ $repetition = 2 ]]; then
    systemctl kill --signal=SIGKILL "$unit"
  fi
  for attempt in $(seq 1 150); do
    outcome=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["outcome"])' "$run/state/state.json")
    [[ $outcome != rolled-back ]] || break
    sleep 0.1
  done
  [[ $outcome = rolled-back && $(cat /sys/class/net/ng-guard-test/mtu) = 1420 ]]
  python3 - "$run" "$repetition" <<'PY'
import json,sys
from pathlib import Path
root=Path(sys.argv[1]);before=json.loads((root/'prepared.json').read_text());after=json.loads((root/'state/state.json').read_text())
assert before['deadlineUptimeSeconds']==after['deadlineUptimeSeconds']
assert before['bootID']==after['bootID']
assert after['restoreAttempts']==1 and after['restoreFailures']==0
print(json.dumps({'repetition':int(sys.argv[2]),'rollback':'passed','deadlineUnchanged':True,'serviceKilled':sys.argv[2]=='2','mtuRestored':1420}))
PY
done
wait "$sampler"
sampler=
python3 - "$root" <<'PY'
import re,sys,json
from pathlib import Path
root=Path(sys.argv[1]);text=(root/'management.log').read_text();match=re.search(r'(\d+) packets transmitted, (\d+) received',text)
assert match,text
sent,received=map(int,match.groups());assert sent>=100 and received==sent,text
print(json.dumps({'managementSent':sent,'managementReceived':received,'managementLoss':0,'artifactDirectory':str(root)}))
PY
