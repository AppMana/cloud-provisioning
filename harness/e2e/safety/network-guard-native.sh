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
mkdir -p /var/lib/cldt
root=$(mktemp -d /var/lib/cldt/network-guard-native.XXXXXXXX)
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
ping -n -i 0.2 -w 60 "$management" > "$root/management.log" &
sampler=$!
for repetition in 1 2 3 4 5; do
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
  cp "$run/restore" "$run/apply"
  if [[ $repetition = 4 || $repetition = 5 ]]; then
    cat >> "$run/apply" <<APPLY
(sleep 10; ip link set ng-guard-test mtu 1280) &
touch '$run/apply-started'
wait
APPLY
  fi
  printf '1280\n' > "$run/candidate"
  chmod 600 "$run/candidate"
  candidate=$(printf '1280\n' | sha256sum | cut -d ' ' -f1)
  python3 - "$run" "$candidate" <<'PY'
import json,sys
from pathlib import Path
root=Path(sys.argv[1])
(root/'plan.json').write_text(json.dumps({'apply':str(root/'apply'),'restore':str(root/'restore'),'files':[{'path':str(root/'mtu'),'candidatePath':str(root/'candidate'),'candidateSHA256':sys.argv[2]}]}))
PY
  "$guard" -state "$run/state" -plan "$run/plan.json" -within 8s prepare > "$run/prepared.json"
  unit="network-guard-native-${root##*.}-$repetition"
  units+=("$unit")
  systemd-run --quiet --unit "$unit" --property Restart=on-failure --property RestartSec=200ms --property KillMode=control-group \
    --property 'RestrictAddressFamilies=AF_UNIX AF_NETLINK' \
    "$guard" -state "$run/state" watch
  readiness="$run/state/ready.json"
  if [[ $repetition = 4 || $repetition = 5 ]]; then readiness="$run/apply-started"; fi
  for attempt in $(seq 1 50); do
    [[ ! -f "$readiness" ]] || break
    sleep 0.1
  done
  test -f "$readiness"
  python3 - "$run/state/state.json" "$repetition" <<'PY'
import json,sys
assert json.load(open(sys.argv[1]))['outcome']==('applying' if sys.argv[2] in ('4','5') else 'armed')
PY
  [[ $(cat /sys/class/net/ng-guard-test/mtu) = 1280 ]]
  if [[ $repetition = 2 || $repetition = 5 ]]; then
    systemctl kill --signal=SIGKILL "$unit"
  fi
  for attempt in $(seq 1 150); do
    outcome=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["outcome"])' "$run/state/state.json")
    [[ $outcome != rolled-back ]] || break
    sleep 0.1
  done
  [[ $outcome = rolled-back && $(cat /sys/class/net/ng-guard-test/mtu) = 1420 ]]
  if [[ $repetition = 4 || $repetition = 5 ]]; then
    sleep 11
    [[ $(cat /sys/class/net/ng-guard-test/mtu) = 1420 ]]
    test ! -f "$run/state/ready.json"
  fi
  python3 - "$run" "$repetition" <<'PY'
import json,sys
from pathlib import Path
root=Path(sys.argv[1]);before=json.loads((root/'prepared.json').read_text());after=json.loads((root/'state/state.json').read_text())
assert before['deadlineUptimeSeconds']==after['deadlineUptimeSeconds']
assert before['bootID']==after['bootID']
assert after['applyFailures']==(1 if sys.argv[2] in ('4','5') else 0)
assert after['restoreAttempts']==1 and after['restoreFailures']==0
print(json.dumps({'repetition':int(sys.argv[2]),'rollback':'passed','deadlineUnchanged':True,'serviceKilled':sys.argv[2] in ('2','5'),'applyFailures':after['applyFailures'],'mtuRestored':1420}))
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
