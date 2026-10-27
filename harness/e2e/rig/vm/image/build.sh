#!/usr/bin/env bash
set -euo pipefail
# Build the helper and launcher from the same clean source as the pinned SDK.
source_dir=$(realpath "${1:?labcontainers checkout required}")
base_image=${2:?base VM image required}
target=${3:-cloud-provisioning/vm:single-nic}
here=$(cd -- "$(dirname -- "$0")" && pwd)
revision=$(git -C "$source_dir" rev-parse HEAD)
pin=$(sed -n 's/.*github.com\/appmana\/labcontainers .*-[^-]*-\([0-9a-f]*\)$/\1/p' "$here/../../../go.mod")
test "${revision:0:12}" = "$pin" || { echo 'SDK/source revision mismatch' >&2; exit 1; }
test -z "$(git -C "$source_dir" status --porcelain --untracked-files=no)" || { echo 'Labcontainers source has tracked changes' >&2; exit 1; }
# The build context goes in the repository's private state directory.
state="$here/../../../.state"
mkdir -p "$state"
context=$(mktemp -d "$state/vm-image-build.XXXXXXXX")
trap 'rm -rf -- "$context"' EXIT
GOWORK=off CGO_ENABLED=0 go -C "$source_dir" build -trimpath -o "$context/labcontainers-guest" ./cmd/labcontainers-guest
cp "$here/Dockerfile" "$context/Dockerfile"
cp "$source_dir/images/ubuntu-single-nic/launcher.py" "$context/launcher.py"
cp "$source_dir/images/common/interfaces.py" "$context/interfaces.py"
docker build --build-arg "LABCONTAINERS_VM_IMAGE=$base_image" --build-arg "SOURCE_REVISION=$revision" -t "$target" "$context"
