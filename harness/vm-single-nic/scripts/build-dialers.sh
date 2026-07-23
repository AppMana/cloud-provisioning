#!/usr/bin/env bash
# Build the two dialer binaries this harness tests against, from real git
# revisions in this repo -- not stand-ins. Output goes to ../bin/ (gitignored;
# rebuilt on demand, not committed).
#
#   dialer-old-vulnerable  -- fb01961~1: hardcoded AllowedIPs, RouteReplace()
#                             with no Table field (installs into the main
#                             table). The actual pre-fix shape that caused
#                             the jarvis incident.
#   dialer-fixed           -- current HEAD (395d2fa and later): AllowedIPs
#                             is a flag, routes land in the isolated
#                             fallback table (52820, priority 32800).
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(cd "${HARNESS_DIR}/../.." && pwd)"
BIN_DIR="${HARNESS_DIR}/bin"
OLD_REV="fb01961~1"

mkdir -p "${BIN_DIR}"

# Statically linked (CGO_ENABLED=0): the DaemonSet runs this binary inside a
# `busybox` container via a hostPath mount, which has no glibc/dynamic
# linker -- a dynamically linked build fails there with a bare "no such
# file or directory" (the missing /lib64/ld-linux-x86-64.so.2, not the
# binary itself).
echo "Building dialer-old-vulnerable from ${OLD_REV}..."
WORKTREE="$(mktemp -d)"
trap 'git -C "${REPO_DIR}" worktree remove "${WORKTREE}" --force >/dev/null 2>&1 || true' EXIT
git -C "${REPO_DIR}" worktree add "${WORKTREE}" "${OLD_REV}" >/dev/null
( cd "${WORKTREE}/controller" && CGO_ENABLED=0 go build -o "${BIN_DIR}/dialer-old-vulnerable" ./cmd/dialer/ )

echo "Building dialer-fixed from HEAD ($(git -C "${REPO_DIR}" rev-parse --short HEAD))..."
( cd "${REPO_DIR}/controller" && CGO_ENABLED=0 go build -o "${BIN_DIR}/dialer-fixed" ./cmd/dialer/ )

file "${BIN_DIR}/dialer-old-vulnerable" "${BIN_DIR}/dialer-fixed"
echo "Built: ${BIN_DIR}/dialer-old-vulnerable, ${BIN_DIR}/dialer-fixed"
