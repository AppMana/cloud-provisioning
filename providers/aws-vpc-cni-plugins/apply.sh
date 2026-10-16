#!/bin/sh
# Apply the explicit Windows host-route extension to the pinned upstream source.
set -eu
patch_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir=${1:?usage: apply.sh /path/to/clean/amazon-vpc-cni-plugins}
expected=8c8909eabf08b598af67afb9e540fab67a1f4ff3
[ "$(git -C "$source_dir" rev-parse HEAD)" = "$expected" ] || { echo 'Unexpected upstream revision' >&2; exit 1; }
[ -z "$(git -C "$source_dir" status --porcelain)" ] || { echo 'Expected a clean upstream checkout' >&2; exit 1; }
git -C "$source_dir" apply --check "$patch_dir/host-routes.patch"
git -C "$source_dir" apply "$patch_dir/host-routes.patch"
