#!/usr/bin/env bash
# Renders the vendored VPC resource controller config (go generate): the
# Deployment for the controller to adapt, the rest for the product chart.
# Arguments override the two output paths.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
deployment=${1:-deployment.yaml}
chart=${2:-../../../../../../charts/cloud-provisioning/templates/aws-windows-ipam.yaml}
kustomize build overlay 2>/dev/null | go run split.go "$deployment" "$chart"
