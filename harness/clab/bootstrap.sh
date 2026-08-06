#!/usr/bin/env bash
# Run a machine's rendered bootstrap on the container that machine is.
#
# In production the infrastructure provider does this: AWS passes the
# userdata to the instance, and the docker provider parses it and runs
# it inside the container it made. Here containerlab owns the machines,
# so nothing does it, and this is that missing half.
#
# The template it interprets constrains itself to write_files and runcmd
# for exactly this reason (see join-patterns/kubeadm-worker.cloud-config
# .tmpl), so a full cloud-init is not needed and would be worse: it
# would accept fields the real path silently ignores.
#
# Usage: bootstrap.sh <machine-name> <container>
set -euo pipefail
cd "$(dirname "$0")"

MACHINE="${1:?machine name}"
CONTAINER="${2:?container}"
# The address the site knows this machine by. A real instance has one
# NIC and kubelet cannot choose wrongly; a lab node also carries
# containerlab's management interface, and kubelet picked that, so the
# node registered an address the site cannot reach. Pinning it is the
# harness compensating for its own extra interface, not the product
# needing to know about it.
NODE_IP="${3:-}"
NS="${NS:-cloud-provisioning}"
LAB=cldt
DIALER_DIR="${DIALER_DIR:-$PWD/out/binaries}"

c() { echo "clab-$LAB-$1"; }
in_node() { docker exec "$(c "$1")" "${@:2}"; }
write_to() { docker exec -i "$(c "$1")" sh -c "cat >$2"; }
k() { in_node bastion kubectl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "--- the dialer binary, before anything asks for it ---"
# The userdata fetches this over a file:// URL, so it has to be on the
# machine already. Its digest is pinned by the controller and checked by
# the userdata, so a stale copy fails loudly rather than running.
[ -f "$DIALER_DIR/wg-dialer-linux-amd64" ] || fail "no dialer binary at $DIALER_DIR"
in_node "$CONTAINER" mkdir -p /opt/dialer-dist
docker cp "$DIALER_DIR/wg-dialer-linux-amd64" "$(c "$CONTAINER")":/opt/dialer-dist/wg-dialer-linux-amd64
echo "  in place"

echo "--- waiting for the rendered bootstrap ---"
# It does not exist until the mesh has at least one published peer, so
# this waits rather than failing: the site's dialers publish on their
# own schedule.
for _ in $(seq 1 60); do
  USERDATA=$(k -n "$NS" get secret "${MACHINE}-bootstrap" -o jsonpath='{.data.value}' 2>/dev/null | base64 -d || true)
  [ -n "$USERDATA" ] && break
  sleep 5
done
[ -n "$USERDATA" ] || fail "no ${MACHINE}-bootstrap secret: the mesh published no peer, so nothing was rendered"
echo "  $(printf '%s' "$USERDATA" | wc -c) bytes"

echo "--- write_files ---"
# Parsed with python rather than a yaml tool the node may not have, and
# applied one file at a time so a failure names the file.
printf '%s' "$USERDATA" | python3 -c '
import sys, yaml, json
doc = yaml.safe_load(sys.stdin.read()) or {}
out = []
for f in doc.get("write_files", []) or []:
    out.append({"path": f["path"], "permissions": str(f.get("permissions", "0644")), "content": f.get("content", "")})
print(json.dumps(out))
' > /tmp/cldt-writefiles.json || fail "could not parse write_files"

python3 -c '
import json
for f in json.load(open("/tmp/cldt-writefiles.json")):
    print(f["path"], f["permissions"], sep="\t")
' | while IFS=$'\t' read -r path perms; do
  # cloud-init makes the parent directory; a shell redirect does not,
  # and the failure names the file rather than the directory.
  in_node "$CONTAINER" mkdir -p "$(dirname "$path")"
  python3 -c '
import json,sys
for f in json.load(open("/tmp/cldt-writefiles.json")):
    if f["path"] == sys.argv[1]:
        sys.stdout.write(f["content"]); break
' "$path" | write_to "$CONTAINER" "$path" || fail "writing $path"
  in_node "$CONTAINER" chmod "$perms" "$path"
  echo "  $path ($perms)"
done

if [ -n "$NODE_IP" ]; then
  # After write_files, because the bootstrap writes this file too and
  # would otherwise overwrite the pin.
  in_node "$CONTAINER" sh -c "sed -i 's/\\(KUBELET_EXTRA_ARGS=.*\\)/\\1 --node-ip=$NODE_IP/' /etc/default/kubelet 2>/dev/null || echo 'KUBELET_EXTRA_ARGS=--node-ip=$NODE_IP' >> /etc/default/kubelet"
  in_node "$CONTAINER" grep -q -- "--node-ip=$NODE_IP" /etc/default/kubelet \
    || fail "could not pin the node address to $NODE_IP"
  echo "  registering as $NODE_IP"
fi

echo "--- runcmd ---"
# In order, stopping on the first failure. A bootstrap that half ran is
# not a node, and continuing would report the wrong step as the broken
# one.
printf '%s' "$USERDATA" | python3 -c '
import sys, yaml, json
doc = yaml.safe_load(sys.stdin.read()) or {}
cmds = []
for cmd in doc.get("runcmd", []) or []:
    cmds.append(cmd if isinstance(cmd, str) else " ".join(cmd))
print(json.dumps(cmds))
' > /tmp/cldt-runcmd.json || fail "could not parse runcmd"

COUNT=$(python3 -c 'import json;print(len(json.load(open("/tmp/cldt-runcmd.json"))))')
[ "$COUNT" -gt 0 ] || fail "the bootstrap has no runcmd at all, so nothing would happen"
for i in $(seq 0 $((COUNT - 1))); do
  script=$(python3 -c 'import json,sys;print(json.load(open("/tmp/cldt-runcmd.json"))[int(sys.argv[1])])' "$i")
  # kubeadm inspects the kernel it runs on, which here belongs to the
  # host, so it checks things the node neither owns nor can change. The
  # stock template carries no such flag because a real machine needs
  # none.
  case "$script" in
    *kubeadm\ join*) script="${script//kubeadm join/kubeadm join --ignore-preflight-errors=all}" ;;
  esac
  echo "  step $((i + 1))/$COUNT"
  in_node "$CONTAINER" bash -c "$script" || fail "runcmd step $((i + 1)) failed"
done

echo "--- joined ---"
for _ in $(seq 1 60); do k get node "$CONTAINER" >/dev/null 2>&1 && break; sleep 5; done
k get node "$CONTAINER" -o wide 2>/dev/null || fail "the node never registered"

# The last thing the infrastructure controller would have reported: which
# node this machine became. Without it the mesh cannot map the machine to
# a node, so it never reads that node's pod blocks and never permits
# them, and the tunnel comes up carrying node traffic and no pod traffic
# at all. That failure looks like a broken tunnel and is a missing field.
k -n "$NS" patch machine "$MACHINE" --subresource=status --type merge \
  -p "{\"status\":{\"nodeRef\":{\"apiVersion\":\"v1\",\"kind\":\"Node\",\"name\":\"$CONTAINER\"}}}" >/dev/null \
  || fail "reporting which node the machine became"
echo "  machine $MACHINE is node $CONTAINER"

# What the site permits for this machine, once there is anything to
# permit. The network gives a node a pod block when the node first needs
# one, so a node that has just joined and runs nothing has none, and an
# empty accept list here is the truth rather than a fault. The two cases
# are distinguished by asking the network, not by waiting longer.
# The grep finds nothing on the first pass, every time, because the
# block does not exist yet: that is what this loop is waiting for. Under
# set -e a command substitution that ends in a failing grep takes the
# script with it, so the wait could never reach a second iteration and
# the whole join was reported as "cloud B never joined" moments after
# the node had in fact joined and gone Ready.
for _ in $(seq 1 24); do
  block=$(k get blockaffinities.crd.projectcalico.org \
    -o jsonpath="{range .items[?(@.spec.node=='$CONTAINER')]}{.spec.cidr}{'\n'}{end}" 2>/dev/null | grep -v '^$' | head -1 || true)
  [ -n "$block" ] && break
  sleep 5
done
if [ -z "${block:-}" ]; then
  echo "  the network has given $CONTAINER no pod block yet, so there is nothing for the site to permit"
else
  for _ in $(seq 1 36); do
    permitted=$(k -n "$NS" get secret "$NS-peers" -o jsonpath="{.data.peer-allowed-ips-$MACHINE}" 2>/dev/null | base64 -d 2>/dev/null || true)
    case "$permitted" in *"$block"*) break ;; esac
    sleep 5
  done
  case "${permitted:-}" in
    *"$block"*) echo "  the site permits $permitted" ;;
    *) fail "the network gave $CONTAINER $block and the site permits only ${permitted:-nothing}: its pods are unreachable from here" ;;
  esac
fi
