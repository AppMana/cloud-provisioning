#!/bin/sh
# Apply the hybrid-cluster node selection extension to the pinned upstream source.
set -eu
patch_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir=${1:?usage: apply.sh /path/to/clean/amazon-vpc-resource-controller-k8s}
expected=ca7792b0485c7972af0d03af4a71b912c3a70add
[ "$(git -C "$source_dir" rev-parse HEAD)" = "$expected" ] || { echo 'Unexpected upstream revision' >&2; exit 1; }
[ -z "$(git -C "$source_dir" status --porcelain)" ] || { echo 'Expected a clean upstream checkout' >&2; exit 1; }
git -C "$source_dir" apply --check "$patch_dir/node-selector.patch"
git -C "$source_dir" apply "$patch_dir/node-selector.patch"
