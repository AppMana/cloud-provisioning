#!/usr/bin/env bash
# Build the dialer binary this harness tests, from the working tree:
# the same code a release build ships. Output goes to ../bin/
# (gitignored; rebuilt on demand, not committed).
#
# There is no failing-control build: the CIDR-wide kernel route failure
# class is rejected at parse time by cmd/dialer's parseHostRoute (/32
# and /128 only) and unit-tested directly. The harness's scenarios
# assert the invariant against a real kernel; a live failing control
# would mean rebuilding and running a binary this project does not
# deploy.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(cd "${HARNESS_DIR}/../.." && pwd)"
BIN_DIR="${HARNESS_DIR}/bin"

mkdir -p "${BIN_DIR}"

# Statically linked (CGO_ENABLED=0): the DaemonSet runs this binary inside a
# `busybox` container via a hostPath mount, which has no glibc/dynamic
# linker, so a dynamically linked build fails there with a bare "no
# such file or directory" (the missing /lib64/ld-linux-x86-64.so.2, not
# the binary itself).
echo "Building dialer-fixed from the working tree ($(git -C "${REPO_DIR}" rev-parse --short HEAD))..."
( cd "${REPO_DIR}/controller" && CGO_ENABLED=0 go build -o "${BIN_DIR}/dialer-fixed" ./cmd/dialer/ )

file "${BIN_DIR}/dialer-fixed"
echo "Built: ${BIN_DIR}/dialer-fixed"
