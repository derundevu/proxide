#!/usr/bin/env bash
set -euo pipefail
mkdir -p resources
# Resolve the data branch once so both databases and their checksums share a snapshot.
data_commit=$(gh api repos/XTLS/Xray-rules-dat/commits/release --jq .sha)
[[ "$data_commit" =~ ^[0-9a-f]{40}$ ]]
download_database() {
  local entry=$1 destination=$2 checksum hash base
  base="https://raw.githubusercontent.com/XTLS/Xray-rules-dat/${data_commit}/${entry}"
  if curl --fail --silent --show-error --location "${base}.dat" -o "$destination" 2>/dev/null; then
    checksum=$(curl --fail --show-error --location "${base}.dat.sha256sum")
    read -r hash _ <<< "$checksum"
    [[ "$hash" =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$hash" "$destination" | shasum -a 256 -c
  else
    # Some databases are published only as .dat.gz on the release branch.
    local gz
    gz=$(mktemp)
    curl --fail --show-error --location "${base}.dat.gz" -o "$gz" || {
      rm -f "$gz"
      return 1
    }
    checksum=$(curl --fail --show-error --location "${base}.dat.gz.sha256sum")
    read -r hash _ <<< "$checksum"
    [[ "$hash" =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$hash" "$gz" | shasum -a 256 -c
    gunzip -c "$gz" > "$destination"
    rm -f "$gz"
    checksum=$(curl --fail --show-error --location "${base}.dat.sha256sum")
    read -r hash _ <<< "$checksum"
    [[ "$hash" =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$hash" "$destination" | shasum -a 256 -c
  fi
  test -s "$destination"
}
download_database geoip/geoip-lite resources/geoip.dat
download_database geosite/geosite resources/geosite.dat
if [[ "${1:-}" == '--geodata-only' ]]; then
  exit 0
fi
# Docker has historically shipped the full/expanded databases.
mkdir -p resources/docker
download_database geoip/geoip-full resources/docker/geoip.dat
download_database geosite/geosite-expand resources/docker/geosite.dat
archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
curl --fail --show-error --location https://www.wintun.net/builds/wintun-0.14.1.zip -o "$archive"
printf '%s  %s\n' 07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51 "$archive" | shasum -a 256 -c
unzip -q "$archive" -d resources
for arch in amd64 x86 arm64; do
  test -s "resources/wintun/bin/$arch/wintun.dll"
done
test -s resources/wintun/LICENSE.txt
