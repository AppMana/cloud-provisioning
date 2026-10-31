#!/usr/bin/env bash
# Build image payloads, first-boot dialer binaries, universal fat Linux dialer,
# and multi-platform release manifest with the host Go toolchain.
set -euo pipefail
if (( $# < 3 )); then
  echo "usage: $0 OUTPUT_DIRECTORY {all|endpoint-controller|dialer|platform-tuple|windows-tunnel} ARCH..." >&2
  exit 2
fi
output=$1
target=$2
shift 2
case "$target" in
  all|endpoint-controller) targets=(endpoint-controller platform-tuple dialer windows-tunnel) ;;
  dialer|platform-tuple|windows-tunnel) targets=("$target") ;;
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

build_fat_linux_dialer() {
  local amd64_bin="$output/wg-dialer-linux-amd64"
  local arm64_bin="$output/wg-dialer-linux-arm64"
  local fat_out="$output/wg-dialer-linux"
  if [[ ! -f "$amd64_bin" || ! -f "$arm64_bin" ]]; then
    return 0
  fi
  local header_size=2048
  local amd64_size arm64_size amd64_sha arm64_sha amd64_off arm64_off
  amd64_size=$(wc -c < "$amd64_bin" | tr -d ' ')
  arm64_size=$(wc -c < "$arm64_bin" | tr -d ' ')
  amd64_sha=$(sha256sum "$amd64_bin" | awk '{print $1}')
  arm64_sha=$(sha256sum "$arm64_bin" | awk '{print $1}')
  amd64_off=$((header_size + 1))
  arm64_off=$((header_size + 1 + amd64_size))

  local stub
  stub=$(cat <<EOF
#!/bin/sh
# Universal multi-arch Linux wg-dialer fat binary (amd64 + arm64).
set -eu
AMD64_OFFSET=${amd64_off}
AMD64_SIZE=${amd64_size}
AMD64_SHA256=${amd64_sha}
ARM64_OFFSET=${arm64_off}
ARM64_SIZE=${arm64_size}
ARM64_SHA256=${arm64_sha}
case "\${WG_DIALER_TEST_ARCH:-\$(uname -m)}" in
  x86_64|amd64) off=\$AMD64_OFFSET; len=\$AMD64_SIZE; sha=\$AMD64_SHA256; arch=amd64 ;;
  aarch64|arm64) off=\$ARM64_OFFSET; len=\$ARM64_SIZE; sha=\$ARM64_SHA256; arch=arm64 ;;
  *) echo "wg-dialer: unsupported architecture \$(uname -m)" >&2; exit 1 ;;
esac
extract_to() {
  dst="\$1"
  tail -c +"\$off" "\$0" | head -c "\$len" > "\$dst"
  echo "\$sha  \$dst" | sha256sum -c - >/dev/null
  chmod 0755 "\$dst"
}
if [ "\${1:-}" = "--extract-native" ]; then
  [ -n "\${2:-}" ] || { echo "usage: \$0 --extract-native OUTPUT" >&2; exit 2; }
  tmp="\${2}.tmp.\$\$"
  extract_to "\$tmp"
  mv -f "\$tmp" "\$2"
  exit 0
fi
cache_dir="/run/wg-dialer-bin"
if [ ! -d "\$cache_dir" ] && ! mkdir -p "\$cache_dir" 2>/dev/null; then
  cache_dir="\${TMPDIR:-/var/tmp}/wg-dialer-bin-\$(id -u)"
  mkdir -p "\$cache_dir"
fi
bin="\$cache_dir/wg-dialer-\$arch-\$sha"
if [ ! -x "\$bin" ]; then
  tmp="\$bin.tmp.\$\$"
  extract_to "\$tmp"
  mv -f "\$tmp" "\$bin"
fi
exec "\$bin" "\$@"
EOF
)
  local stub_len=${#stub}
  if (( stub_len >= header_size - 3 )); then
    echo "fat binary header overflow: ${stub_len} >= ${header_size}" >&2
    exit 1
  fi
  local pad_len=$((header_size - stub_len - 3))
  {
    printf '%s\n#' "$stub"
    head -c "$pad_len" < /dev/zero | tr '\0' '#'
    printf '\n'
    cat "$amd64_bin" "$arm64_bin"
  } > "$fat_out"
  chmod 0755 "$fat_out"
}

write_dialer_manifest() {
  if [[ ! -f "$output/wg-dialer-linux-amd64" && ! -f "$output/wg-dialer-linux-arm64" ]]; then
    return 0
  fi
  local chart_version="0.0.0"
  if [[ -f "../charts/cloud-provisioning/Chart.yaml" ]]; then
    chart_version=$(awk '/^version:/ {print $2}' "../charts/cloud-provisioning/Chart.yaml")
  fi
  local version="${DIALER_RELEASE_VERSION:-v${chart_version}}"
  local repo="${DIALER_RELEASE_REPO:-AppMana/cloud-provisioning}"
  local base_url="${DIALER_RELEASE_BASE_URL:-https://github.com/${repo}/releases/download/${version}}"
  local latest_base_url="${DIALER_LATEST_BASE_URL:-https://github.com/${repo}/releases/latest/download}"

  local universal_json="null"
  if [[ -f "$output/wg-dialer-linux" ]]; then
    local fat_sha
    fat_sha=$(sha256sum "$output/wg-dialer-linux" | awk '{print $1}')
    universal_json=$(cat <<EOF
{
    "os": "linux",
    "architectures": ["amd64", "arm64"],
    "artifact": "wg-dialer-linux",
    "url": "${base_url}/wg-dialer-linux",
    "latestURL": "${latest_base_url}/wg-dialer-linux",
    "sha256": "${fat_sha}"
  }
EOF
)
  fi

  local platforms_entries=()
  if [[ -f "$output/wg-dialer-linux-amd64" ]]; then
    local sha
    sha=$(sha256sum "$output/wg-dialer-linux-amd64" | awk '{print $1}')
    platforms_entries+=("\"linux/amd64\": {
      \"os\": \"linux\",
      \"architecture\": \"amd64\",
      \"artifact\": \"wg-dialer-linux-amd64\",
      \"url\": \"${base_url}/wg-dialer-linux-amd64\",
      \"latestURL\": \"${latest_base_url}/wg-dialer-linux-amd64\",
      \"sha256\": \"${sha}\"
    }")
  fi
  if [[ -f "$output/wg-dialer-linux-arm64" ]]; then
    local sha
    sha=$(sha256sum "$output/wg-dialer-linux-arm64" | awk '{print $1}')
    platforms_entries+=("\"linux/arm64\": {
      \"os\": \"linux\",
      \"architecture\": \"arm64\",
      \"artifact\": \"wg-dialer-linux-arm64\",
      \"url\": \"${base_url}/wg-dialer-linux-arm64\",
      \"latestURL\": \"${latest_base_url}/wg-dialer-linux-arm64\",
      \"sha256\": \"${sha}\"
    }")
  fi
  if [[ -f "$output/windows-tunnel.exe" ]]; then
    local sha
    sha=$(sha256sum "$output/windows-tunnel.exe" | awk '{print $1}')
    platforms_entries+=("\"windows/amd64\": {
      \"os\": \"windows\",
      \"architecture\": \"amd64\",
      \"artifact\": \"windows-tunnel.exe\",
      \"url\": \"${base_url}/windows-tunnel.exe\",
      \"latestURL\": \"${latest_base_url}/windows-tunnel.exe\",
      \"sha256\": \"${sha}\"
    }")
  fi

  local joined_platforms=""
  local i
  for (( i=0; i<${#platforms_entries[@]}; i++ )); do
    if (( i > 0 )); then
      joined_platforms+=","$'\n'"    "
    fi
    joined_platforms+="${platforms_entries[$i]}"
  done

  cat > "$output/dialer-manifest.json" <<EOF
{
  "schemaVersion": 1,
  "version": "${version}",
  "repository": "${repo}",
  "universal": ${universal_json},
  "platforms": {
    ${joined_platforms}
  }
}
EOF
  cp "$output/dialer-manifest.json" "$output/manifest.json"
}

build_fat_linux_dialer
write_dialer_manifest
