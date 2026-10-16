#!/usr/bin/env bash
# Build image payloads and first-boot dialer binaries with the host Go toolchain.
set -euo pipefail
if (( $# < 3 )); then
  echo "usage: $0 OUTPUT_DIRECTORY {all|endpoint-controller|dialer} ARCH..." >&2
  exit 2
fi
output=$1
target=$2
shift 2
case "$target" in
  all) targets=(endpoint-controller dialer) ;;
  endpoint-controller|dialer) targets=("$target") ;;
  *) echo "unsupported target: $target" >&2; exit 2 ;;
esac
for arch in "$@"; do
  case "$arch" in
    amd64|arm64) ;;
    *) echo "unsupported architecture: $arch" >&2; exit 2 ;;
  esac
done
mkdir -p "$output"
output=$(cd "$output" && pwd)
cd "$(dirname "${BASH_SOURCE[0]}")"
export CGO_ENABLED=0 GOOS=linux GOWORK=off
for arch in "$@"; do
  mkdir -p "$output/linux/$arch"
  for component in "${targets[@]}"; do
    GOARCH="$arch" go build -trimpath -ldflags='-s -w' \
      -o "$output/linux/$arch/$component" "./cmd/$component"
    if [[ "$component" == dialer ]]; then
      cp "$output/linux/$arch/dialer" "$output/wg-dialer-linux-$arch"
    fi
  done
done
