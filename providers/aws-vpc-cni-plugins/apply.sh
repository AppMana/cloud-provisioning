#!/bin/sh
# Apply the Windows host-route and endpoint MTU extensions to pinned upstream source.
set -eu
patch_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir=${1:?usage: apply.sh /path/to/clean/amazon-vpc-cni-plugins}
expected=8c8909eabf08b598af67afb9e540fab67a1f4ff3
[ "$(git -C "$source_dir" rev-parse HEAD)" = "$expected" ] || { echo 'Unexpected upstream revision' >&2; exit 1; }
[ -z "$(git -C "$source_dir" status --porcelain)" ] || { echo 'Expected a clean upstream checkout' >&2; exit 1; }
for patch in host-routes.patch endpoint-mtu.patch; do
    git -C "$source_dir" apply --check "$patch_dir/$patch"
    git -C "$source_dir" apply "$patch_dir/$patch"
done
