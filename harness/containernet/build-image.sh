#!/usr/bin/env bash
# Build the node image the topology test runs.
set -euo pipefail
cd "$(dirname "$0")/image"
docker build -t wg-node:test .
