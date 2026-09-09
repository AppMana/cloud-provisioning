#!/usr/bin/env bash
# Pinned public release assets; run from harness/e2e.
set -euo pipefail
umask 077
destination=${1:-.state/okd-tools}
mkdir -p "$destination"
scratch=$(mktemp -d "$destination/.fetch-XXXXXX")
trap 'rm -rf "$scratch"' EXIT
release=4.21.0-okd-scos.11
base="https://github.com/okd-project/okd/releases/download/$release"

verify() {
    printf '%s  %s\n' "$2" "$1" | sha256sum --check --status
}

fetch() {
    local url=$1 name=$2 checksum=$3
    if [[ -f "$destination/$name" ]]; then
        verify "$destination/$name" "$checksum"
    else
        curl --fail --location --retry 3 --output "$scratch/$name" "$url"
        verify "$scratch/$name" "$checksum"
        mv "$scratch/$name" "$destination/$name"
    fi
}

installer="openshift-install-linux-$release.tar.gz"
client="openshift-client-linux-$release.tar.gz"
fetch "$base/$installer" "$installer" 4abaae4185b8a1f125fe010ae02496dcaa85cb081cfe5fa8a648167b3f6f68e1
fetch "$base/$client" "$client" f31296aa39ac3d46a1ecb8d4c2222231e7a8ea507146d4252357a82b9dbb1f9e
tar -xzf "$destination/$installer" -C "$scratch" openshift-install
tar -xzf "$destination/$client" -C "$scratch" oc kubectl
for binary in openshift-install oc kubectl; do
    install -m 0700 "$scratch/$binary" "$destination/$binary"
done

# openshift-install coreos print-stream-json for the pinned release selects
# this initial disk. The cluster release subsequently updates the machine OS.
disk_url=https://rhcos.mirror.openshift.com/art/storage/prod/streams/c10s/builds/10.0.20251103-0/x86_64/scos-10.0.20251103-0-qemu.x86_64.qcow2.gz
fetch "$disk_url" scos.qcow2.gz 5b8f269822647f1471c4ae6aab445a1a4e3bce1522fcb566a507f4881c437a0a
disk_checksum=70609c79412a7853f0634f34afdd0e0a2492d4e188d45b78d4c9ead41ff571c8
if [[ ! -f "$destination/scos.qcow2" ]]; then
    gzip -dc "$destination/scos.qcow2.gz" > "$scratch/scos.qcow2"
    verify "$scratch/scos.qcow2" "$disk_checksum"
    mv "$scratch/scos.qcow2" "$destination/scos.qcow2"
fi
verify "$destination/scos.qcow2" "$disk_checksum"
echo "Verified OKD $release tools and SCOS disk in $destination"
