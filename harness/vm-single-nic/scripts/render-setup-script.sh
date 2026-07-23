#!/usr/bin/env bash
# Generates a fresh SSH keypair for this harness (if missing) and renders
# every node's *-setup.sh from its *-setup.sh.tmpl with the public key
# embedded. Both the keypair and the rendered scripts are gitignored --
# regenerated per checkout, not committed. One shared keypair for every
# node in the topology -- this key is purely for the harness driver's own
# SSH/k0sctl access, not identity material used inside the test itself,
# so there's no reason for it to differ per node.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEY_DIR="${HARNESS_DIR}/cfg/ssh"
KEY_PATH="${KEY_DIR}/harness_key"

mkdir -p "${KEY_DIR}"
if [[ ! -f "${KEY_PATH}" ]]; then
  ssh-keygen -t ed25519 -N "" -f "${KEY_PATH}" -C "wgdialer-vm-single-nic-harness" >/dev/null
fi

PUBKEY="$(cat "${KEY_PATH}.pub")"
for node in onprem cloud; do
  sed "s#HARNESS_PUBLIC_KEY#${PUBKEY}#" "${HARNESS_DIR}/cfg/${node}-setup.sh.tmpl" > "${HARNESS_DIR}/cfg/${node}-setup.sh"
  chmod +x "${HARNESS_DIR}/cfg/${node}-setup.sh"
  echo "Rendered cfg/${node}-setup.sh with key ${KEY_PATH}.pub"
done
