#!/bin/sh
# Ubuntu image layer; cloud-specific bootstrap prerequisites belong to adapters.
set -eu
test ! -d /var/snap/microk8s
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y snapd python3
command -v snap
python3 --version
# Installing MicroK8s itself starts cluster services. Leave that to native
# first-boot joining so captured images contain no cluster identity or CNI state.
test ! -d /var/snap/microk8s
