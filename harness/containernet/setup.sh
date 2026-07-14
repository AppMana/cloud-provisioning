#!/usr/bin/env bash
# One-shot, idempotent environment setup for topology.py. Automates the
# manual steps documented in README.md so the test setup lives in the
# repo, not in whoever's scratch directory happened to run it first.
#
# Clones containernet next to this script (gitignored -- it's a large
# upstream checkout, not something this repo should vendor) and builds
# a venv + mnexec + the wg-node:test image inside it. Safe to re-run:
# every step either checks first or is itself idempotent.
set -euo pipefail
cd "$(dirname "$0")"

CONTAINERNET_DIR="${CONTAINERNET_DIR:-$(pwd)/containernet}"

if ! command -v docker >/dev/null; then
  echo "docker is required and not on PATH" >&2
  exit 1
fi

if ! dpkg -s openvswitch-switch >/dev/null 2>&1; then
  echo "installing openvswitch-switch (mininet's default switch backend)..."
  sudo apt-get update && sudo apt-get install -y openvswitch-switch
fi

if [ ! -d "$CONTAINERNET_DIR" ]; then
  echo "cloning containernet into $CONTAINERNET_DIR..."
  git clone --depth 1 https://github.com/containernet/containernet.git "$CONTAINERNET_DIR"
fi

if [ ! -d "$CONTAINERNET_DIR/venv" ]; then
  echo "creating venv and installing containernet (editable) + python-iptables..."
  python3 -m venv "$CONTAINERNET_DIR/venv"
  # shellcheck disable=SC1091
  source "$CONTAINERNET_DIR/venv/bin/activate"
  pip install -e "$CONTAINERNET_DIR" python-iptables
  deactivate
fi

if [ ! -x /usr/local/bin/mnexec ]; then
  echo "building and installing mnexec..."
  make -C "$CONTAINERNET_DIR" mnexec
  sudo cp "$CONTAINERNET_DIR/mnexec" /usr/local/bin/mnexec
fi

echo "building wg-node:test image..."
bash "$(dirname "$0")/build-image.sh"

cat <<EOF

Setup complete. Run the test with:
  source "$CONTAINERNET_DIR/venv/bin/activate"
  sudo -E env PATH=\$PATH python3 -u "$(dirname "$(readlink -f "$0")")/topology.py"
EOF
