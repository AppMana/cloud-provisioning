#!/bin/bash
# Stand-in for the provider's user_data/cloud-init runcmd. Real cloud-init
# reduces to "read instance metadata, write files, run commands once at
# first boot" — this does exactly that, reading the metadata containernet
# supplied via `docker run -e` (visible to PID 1 as /proc/1/environ,
# because systemd services don't inherit it automatically) and writing
# the WireGuard peer config from it. The CAPA/k0smotron manifests in
# ../../manifests/ populate the real equivalent (K0sWorkerConfig `files`).
set -euo pipefail
marker=/etc/wireguard/.provisioned
[ -f "$marker" ] && exit 0

mkdir -p /etc/wireguard
tr '\0' '\n' < /proc/1/environ | grep '^WG_' | sed 's/^WG_//' > /etc/wireguard/node.env

touch "$marker"
