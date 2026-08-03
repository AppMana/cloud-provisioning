#!/usr/bin/env bash
# Build the dialer binary this harness tests, from the working tree --
# the same code a release build ships. Output goes to ../bin/
# (gitignored; rebuilt on demand, not committed).
#
# There is no "old-vulnerable" RED build anymore: the pre-incident
# binary's whole failure class (CIDR-wide kernel routes) is now
# rejected at parse time by a grep-able invariant (cmd/dialer
# parseHostRoute: /32 and /128 only), unit-tested directly. The
# harness's GREEN scenarios assert the invariant against a real kernel;
# a live RED control would mean deliberately rebuilding and running a
# binary this project forbids ever deploying.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(cd "${HARNESS_DIR}/../.." && pwd)"
BIN_DIR="${HARNESS_DIR}/bin"

mkdir -p "${BIN_DIR}"

# Statically linked (CGO_ENABLED=0): the DaemonSet runs this binary inside a
# `busybox` container via a hostPath mount, which has no glibc/dynamic
# linker -- a dynamically linked build fails there with a bare "no such
# file or directory" (the missing /lib64/ld-linux-x86-64.so.2, not the
# binary itself).
echo "Building dialer-fixed from the working tree ($(git -C "${REPO_DIR}" rev-parse --short HEAD))..."
( cd "${REPO_DIR}/controller" && CGO_ENABLED=0 go build -o "${BIN_DIR}/dialer-fixed" ./cmd/dialer/ )

file "${BIN_DIR}/dialer-fixed"
echo "Built: ${BIN_DIR}/dialer-fixed"
