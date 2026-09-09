#!/bin/sh
# Apply the bounded Windows secure-bootstrap extension to a clean CAPA checkout.
set -eu
patch_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
capa_source=${1:?usage: apply.sh /path/to/clean/capa-v2.12.1}
expected=$(git -C "$capa_source" rev-parse 'v2.12.1^{commit}')
actual=$(git -C "$capa_source" rev-parse HEAD)
[ "$actual" = "$expected" ] || { echo 'Expected CAPA v2.12.1' >&2; exit 1; }
[ -z "$(git -C "$capa_source" status --porcelain)" ] || { echo 'Expected a clean CAPA checkout' >&2; exit 1; }
git -C "$capa_source" apply --check "$patch_dir/windows-bootstrap.patch"
git -C "$capa_source" apply "$patch_dir/windows-bootstrap.patch"
cp -R "$patch_dir/overlay/." "$capa_source/"
