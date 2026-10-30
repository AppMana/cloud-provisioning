#!/usr/bin/env bash
# Build image payloads and first-boot dialer binaries with the host Go toolchain.
set -euo pipefail
if (( $# < 3 )); then
  echo "usage: $0 OUTPUT_DIRECTORY {all|endpoint-controller|dialer|platform-tuple|windows-tunnel} ARCH..." >&2
  exit 2
fi
output=$1
target=$2
shift 2
case "$target" in
  all) targets=(endpoint-controller dialer platform-tuple) ;;
  endpoint-controller|dialer|platform-tuple|windows-tunnel) targets=("$target") ;;
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
export CGO_ENABLED=0 GOWORK=off
for arch in "$@"; do
  for component in "${targets[@]}"; do
    if [[ "$component" == windows-tunnel ]]; then
      if [[ "$arch" != amd64 ]]; then
        continue
      fi
      mkdir -p "$output/windows/$arch"
      GOOS=windows GOARCH="$arch" go build -trimpath -ldflags='-s -w' \
        -o "$output/windows/$arch/windows-tunnel.exe" "./cmd/windows-tunnel"
      cp "$output/windows/$arch/windows-tunnel.exe" "$output/windows-tunnel.exe"
      cp "$output/windows/$arch/windows-tunnel.exe" "$output/wg-dialer-windows-$arch.exe"
      continue
    fi
    mkdir -p "$output/linux/$arch"
    GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' \
      -o "$output/linux/$arch/$component" "./cmd/$component"
    if [[ "$component" == dialer ]]; then
      cp "$output/linux/$arch/dialer" "$output/wg-dialer-linux-$arch"
    fi
  done
done
