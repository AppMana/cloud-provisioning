#!/usr/bin/env bash
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_dir=${1:?usage: build.sh PINNED_MCO_CHECKOUT OUTPUT_DIRECTORY}
output_dir=${2:?output directory required}
source_dir=$(cd "$source_dir" && pwd)
mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
expected=1a0b9eee54cc8b45c91c5780bac2dbc32a6585ee
[[ $(git -C "$source_dir" rev-parse HEAD) == "$expected" ]] || { echo 'MCO source does not match the observed release' >&2; exit 1; }
[[ -z $(git -C "$source_dir" status --porcelain) ]] || { echo 'MCO source must be clean' >&2; exit 1; }
(cd "$source_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -o "$output_dir/ignition-exporter" "$here/main.go")
cp "$here/Dockerfile" "$output_dir/Dockerfile"
