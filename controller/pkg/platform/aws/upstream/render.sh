#!/usr/bin/env bash
# Renders the pinned aws-vpc-cni chart into aws-vpc-cni.yaml (go generate).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
chart=aws-vpc-cni-1.23.2.tgz
echo "457260209b976423671e5d1f8a6839df06000e6de516a6e6cebfb419d0f1618f  $chart" | sha256sum --check --quiet
helm template aws-vpc-cni "$chart" --namespace kube-system --values values.yaml > aws-vpc-cni.yaml
