#!/usr/bin/env bash

set -euo pipefail
IFS=$'\n\t'

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <numeric-version> <binary-directory> <output-directory> <source-commit>" >&2
  exit 2
fi

version="$1"
binary_dir="$2"
output_dir="$3"
source_commit="$4"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
  echo "version must contain only three or four numeric components" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
binary_dir=$(cd "$binary_dir" && pwd)
mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)

if find "$output_dir" -mindepth 1 -print -quit | grep -q .; then
  echo "output directory is not empty: $output_dir" >&2
  exit 2
fi

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
dependency_dir="$work_dir/dependencies"
package_dir="$work_dir/packages"
mkdir -p "$dependency_dir/rules" "$package_dir"

curl_args=(--fail --location --retry 3 --retry-delay 2 --silent --show-error)

rules_json=$(curl "${curl_args[@]}" \
  https://api.github.com/repos/Loyalsoldier/v2ray-rules-dat/releases/latest)
rules_tag=$(jq -er '.tag_name' <<<"$rules_json")
for data_file in geoip.dat geosite.dat; do
  curl "${curl_args[@]}" \
    "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/download/$rules_tag/$data_file" \
    -o "$dependency_dir/rules/$data_file"
  test -s "$dependency_dir/rules/$data_file"
done

tinytun_version="0.0.2-alpha.7"
curl "${curl_args[@]}" \
  "https://raw.githubusercontent.com/v2rayA/TinyTun/v${tinytun_version}/LICENSE" \
  -o "$dependency_dir/TinyTun-LICENSE.txt"

wintun_archive="$dependency_dir/wintun-0.14.1.zip"
curl "${curl_args[@]}" \
  https://www.wintun.net/builds/wintun-0.14.1.zip \
  -o "$wintun_archive"
echo "07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51  $wintun_archive" \
  | sha256sum --check --status
unzip -p "$wintun_archive" wintun/LICENSE.txt >"$dependency_dir/Wintun-LICENSE.txt"

declare -A tinytun_assets=(
  [linux_x64]="tinytun-v0.0.2-alpha.7-x86_64-unknown-linux-musl.zip|8917092ef78b3dc73227d68c26b9d884134a562ea59ea87ca82d27d1e5ad543d"
  [linux_arm64]="tinytun-v0.0.2-alpha.7-aarch64-unknown-linux-musl.zip|3b21ea2f269ff83eb5e69967d4edd6085df674d3576c59e2b83c4439680cc448"
  [windows_x64]="tinytun-v0.0.2-alpha.7-x86_64-pc-windows-gnullvm.zip|56fc9d62e5ddac537f74794dd2e704edd70103b1c1f3cc9ad00081a1e49e792e"
  [windows_arm64]="tinytun-v0.0.2-alpha.7-aarch64-pc-windows-gnullvm.zip|59e94de419e91c98957644cd2e07e6b8f3fee22907edef9c08876174439a8689"
  [darwin_x64]="tinytun-v0.0.2-alpha.7-x86_64-apple-darwin.zip|9ae229f15548b1d32e4da57314028f6ca760148dda8cae271c7ebf51f8102984"
  [darwin_arm64]="tinytun-v0.0.2-alpha.7-aarch64-apple-darwin.zip|360cd0d5501bc0f6332aa6fb55cf1e000b875c4a1740ce5be3d83d4a1137bc28"
)

targets=(
  "linux|amd64||linux_x64|true"
  "linux|arm64||linux_arm64|true"
  "linux|386||linux_x86|false"
  "linux|riscv64||linux_riscv64|false"
  "linux|mips64||linux_mips64|false"
  "linux|mips64le||linux_mips64le|false"
  "linux|mipsle||linux_mips32le|false"
  "linux|mips||linux_mips32|false"
  "linux|loong64||linux_loongarch64|false"
  "linux|arm|7|linux_armv7|false"
  "windows|amd64||windows_x64|true"
  "windows|arm64||windows_arm64|true"
  "darwin|amd64||darwin_x64|true"
  "darwin|arm64||darwin_arm64|true"
  "freebsd|amd64||freebsd_x64|false"
  "freebsd|arm64||freebsd_arm64|false"
  "openbsd|amd64||openbsd_x64|false"
  "openbsd|arm64||openbsd_arm64|false"
)

binary_count=$(find "$binary_dir" -type f -name "v2raya*_${version}*" | wc -l)
if [ "$binary_count" -ne 36 ]; then
  echo "expected 36 official-matrix binaries, found $binary_count" >&2
  exit 1
fi

find_binary() {
  local name="$1"
  local -a matches
  mapfile -t matches < <(find "$binary_dir" -type f -name "$name")
  if [ "${#matches[@]}" -ne 1 ]; then
    echo "expected exactly one binary named $name, found ${#matches[@]}" >&2
    exit 1
  fi
  printf '%s\n' "${matches[0]}"
}

install_tinytun() {
  local friendly_name="$1"
  local target_os="$2"
  local destination="$3"
  local metadata="${tinytun_assets[$friendly_name]}"
  local archive_name="${metadata%%|*}"
  local expected_hash="${metadata##*|}"
  local archive="$dependency_dir/$archive_name"
  local extract_dir="$dependency_dir/${friendly_name}-tinytun"

  if [ ! -f "$archive" ]; then
    curl "${curl_args[@]}" \
      "https://github.com/v2rayA/TinyTun/releases/download/v${tinytun_version}/$archive_name" \
      -o "$archive"
    echo "$expected_hash  $archive" | sha256sum --check --status
  fi

  rm -rf "$extract_dir"
  mkdir -p "$extract_dir"
  unzip -q "$archive" -d "$extract_dir"

  local -a executables
  if [ "$target_os" = windows ]; then
    mapfile -t executables < <(find "$extract_dir" -type f -name 'tinytun*.exe')
  else
    mapfile -t executables < <(find "$extract_dir" -type f -name 'tinytun*' ! -name '*.zip')
  fi
  if [ "${#executables[@]}" -ne 1 ]; then
    echo "expected one TinyTun executable in $archive_name" >&2
    exit 1
  fi

  if [ "$target_os" = windows ]; then
    install -m 0755 "${executables[0]}" "$destination/tinytun.exe"
    local -a wintun_files
    mapfile -t wintun_files < <(find "$extract_dir" -type f -iname 'wintun.dll')
    if [ "${#wintun_files[@]}" -ne 1 ]; then
      echo "expected one wintun.dll in $archive_name" >&2
      exit 1
    fi
    install -m 0644 "${wintun_files[0]}" "$destination/wintun.dll"
  else
    install -m 0755 "${executables[0]}" "$destination/tinytun"
  fi
}

for target in "${targets[@]}"; do
  IFS='|' read -r target_os target_arch target_arm friendly_name has_tinytun <<<"$target"
  extension=""
  [ "$target_os" = windows ] && extension=".exe"

  service_name="v2raya_${friendly_name}_${version}${extension}"
  core_name="v2raya_core_${friendly_name}_${version}${extension}"
  service_source=$(find_binary "$service_name")
  core_source=$(find_binary "$core_name")

  install -m 0755 "$service_source" "$output_dir/$service_name"
  install -m 0755 "$core_source" "$output_dir/$core_name"

  portable_name="v2raya-${version}-${friendly_name//_/-}-portable"
  portable_root="$package_dir/$portable_name"
  mkdir -p "$portable_root/bin" "$portable_root/config" \
    "$portable_root/data" "$portable_root/licenses"

  if [ "$target_os" = windows ]; then
    install -m 0755 "$service_source" "$portable_root/bin/v2raya.exe"
    install -m 0755 "$core_source" "$portable_root/bin/v2raya_core.exe"
    install -m 0644 "$repo_root/install/windows-inno/v2raya.ico" "$portable_root/v2raya.ico"
  else
    install -m 0755 "$service_source" "$portable_root/bin/v2raya"
    install -m 0755 "$core_source" "$portable_root/bin/v2raya_core"
  fi

  install -m 0644 "$dependency_dir/rules/geoip.dat" "$portable_root/data/geoip.dat"
  install -m 0644 "$dependency_dir/rules/geosite.dat" "$portable_root/data/geosite.dat"
  install -m 0644 "$dependency_dir/rules/geosite.dat" "$portable_root/data/LoyalsoldierSite.dat"
  install -m 0644 "$repo_root/LICENSE" "$portable_root/licenses/v2rayA-AGPL-3.0.txt"
  install -m 0644 "$repo_root/core/LICENSE" "$portable_root/licenses/v2raya-core-MPL-2.0.txt"

  if [ "$has_tinytun" = true ]; then
    install_tinytun "$friendly_name" "$target_os" "$portable_root/bin"
    install -m 0644 "$dependency_dir/TinyTun-LICENSE.txt" \
      "$portable_root/licenses/TinyTun-GPL-3.0.txt"
  fi
  if [ "$target_os" = windows ]; then
    install -m 0644 "$dependency_dir/Wintun-LICENSE.txt" \
      "$portable_root/licenses/Wintun-MIT.txt"
  fi

  cgo_enabled=0
  case "$target_arch" in
    mips|mipsle|mips64|mips64le|loong64) cgo_enabled=1 ;;
  esac
  build_tags="none"
  [ "$has_tinytun" = true ] && build_tags="tinytun"

  cat >"$portable_root/BUILD_INFO.txt" <<EOF
Binary version: $version
Source commit: $source_commit
Target: $target_os/$target_arch${target_arm:+ (GOARM=$target_arm)}
CGO_ENABLED: $cgo_enabled
Service build tags: $build_tags
Go toolchain: 1.26
Rules data: Loyalsoldier/v2ray-rules-dat $rules_tag
TinyTun: $([ "$has_tinytun" = true ] && printf 'v%s' "$tinytun_version" || printf 'not included')
Build definition: .github/workflows/_build_binaries.yml
EOF

  runtime_note="This target was cross-built and must be runtime-tested on matching hardware or an equivalent virtual machine."
  case "$target_os/$target_arch" in
    linux/mips|linux/mipsle|linux/mips64|linux/mips64le)
      runtime_note="This build uses glibc CGO and the Go default hard-float MIPS ABI; it is not an OpenWrt/musl build."
      ;;
    darwin/*)
      runtime_note="These binaries are not Apple-signed or notarized."
      ;;
  esac

  cat >"$portable_root/README.txt" <<EOF
v2rayA $version portable package for $friendly_name

Run start-v2raya.cmd on Windows or ./start-v2raya.sh on Unix-like systems,
then open http://127.0.0.1:2017/.

The launcher keeps the database, generated configuration and log inside the
extracted config directory. Administrator/root privileges are required for
system proxy or transparent proxy features; use --lite for a non-root session.

$runtime_note

The archive contains no user database, proxy configuration, subscription URL,
node credential or controller secret.
EOF

  if [ "$target_os" = windows ]; then
    cat >"$portable_root/start-v2raya.cmd" <<'EOF'
@echo off
setlocal
set "V2RAYA_CONFIG=%~dp0config"
set "V2RAYA_V2RAY_BIN=%~dp0bin\v2raya_core.exe"
set "V2RAYA_V2RAY_ASSETSDIR=%~dp0data"
set "V2RAYA_LOG_FILE=%~dp0config\v2raya.log"
if exist "%~dp0bin\tinytun.exe" set "V2RAYA_TINYTUN_BIN=%~dp0bin\tinytun.exe"
if not exist "%V2RAYA_CONFIG%" mkdir "%V2RAYA_CONFIG%"
"%~dp0bin\v2raya.exe" %*
exit /b %ERRORLEVEL%
EOF
  else
    cat >"$portable_root/start-v2raya.sh" <<'EOF'
#!/bin/sh
set -eu
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$root_dir/config"
export V2RAYA_CONFIG="$root_dir/config"
export V2RAYA_V2RAY_BIN="$root_dir/bin/v2raya_core"
export V2RAYA_V2RAY_ASSETSDIR="$root_dir/data"
export V2RAYA_LOG_FILE="$root_dir/config/v2raya.log"
if [ -x "$root_dir/bin/tinytun" ]; then
  export V2RAYA_TINYTUN_BIN="$root_dir/bin/tinytun"
fi
exec "$root_dir/bin/v2raya" "$@"
EOF
    chmod 0755 "$portable_root/start-v2raya.sh"
  fi

  checksum_file="$work_dir/$portable_name.SHA256SUMS.txt"
  (
    cd "$portable_root"
    find . -type f -print0 \
      | sort -z \
      | xargs -0 sha256sum
  ) >"$checksum_file"
  install -m 0644 "$checksum_file" "$portable_root/SHA256SUMS.txt"

  if [ "$target_os" = windows ]; then
    if command -v zip >/dev/null 2>&1; then
      (
        cd "$package_dir"
        zip -q -r -9 "$output_dir/$portable_name.zip" "$portable_name"
      )
    elif tar --version 2>&1 | grep -qi bsdtar; then
      tar -C "$package_dir" -a -cf "$output_dir/$portable_name.zip" "$portable_name"
    elif command -v pwsh.exe >/dev/null 2>&1 || command -v powershell.exe >/dev/null 2>&1; then
      powershell_bin=$(command -v pwsh.exe || command -v powershell.exe)
      package_source=$(cygpath -w "$package_dir/$portable_name")
      package_output=$(cygpath -w "$output_dir/$portable_name.zip")
      # PowerShell expands these environment variables.
      # shellcheck disable=SC2016
      PACKAGE_SOURCE="$package_source" PACKAGE_OUTPUT="$package_output" \
        "$powershell_bin" -NoProfile -NonInteractive -Command \
        'Compress-Archive -LiteralPath $env:PACKAGE_SOURCE -DestinationPath $env:PACKAGE_OUTPUT -CompressionLevel Optimal'
    else
      echo "creating Windows portable archives requires zip, bsdtar or PowerShell" >&2
      exit 1
    fi
  else
    tar -C "$package_dir" -czf "$output_dir/$portable_name.tar.gz" "$portable_name"
  fi
done

asset_count=$(find "$output_dir" -maxdepth 1 -type f | wc -l)
if [ "$asset_count" -ne 54 ]; then
  echo "expected 54 assets before checksums, found $asset_count" >&2
  exit 1
fi

while IFS= read -r -d '' asset; do
  sha256sum "$asset" | awk '{print $1}' >"$asset.sha256.txt"
done < <(find "$output_dir" -maxdepth 1 -type f -print0 | sort -z)

final_count=$(find "$output_dir" -maxdepth 1 -type f | wc -l)
if [ "$final_count" -ne 108 ]; then
  echo "expected 108 release files including checksums, found $final_count" >&2
  exit 1
fi

echo "Created 36 official-name binaries, 18 portable archives and 54 checksums."
