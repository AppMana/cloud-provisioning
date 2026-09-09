#!/bin/sh
# Ubuntu candidate recipe. Run in a disposable VM; complete a reboot before
# verification/capture. Cloud bootstrap and imaging agents belong to adapters.
set -eu
. /etc/os-release
[ "$ID" = ubuntu ] && [ "$VERSION_ID" = 24.04 ] || {
  echo 'This recipe requires Ubuntu 24.04' >&2
  exit 1
}
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y "linux-headers-$(uname -r)" \
  nvidia-driver-580-server=580.173.02-0ubuntu0.24.04.1 vulkan-tools ffmpeg
mkdir -p /etc/cloud-provisioning-image
dpkg-query -W -f='${Package}=${Version}\n' > /etc/cloud-provisioning-image/packages.txt
