#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: scripts/build-release-assets.sh <version>}"
version="${version#v}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dist="$root/dist"

rm -rf "$dist"
mkdir -p "$dist"

platforms=(
  "darwin arm64"
  "darwin amd64"
  "linux arm64"
  "linux amd64"
)

for platform in "${platforms[@]}"; do
  read -r goos goarch <<<"$platform"
  name="pallium_${version}_${goos}_${goarch}"
  work="$dist/$name"
  mkdir -p "$work"
  echo "building $name"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X github.com/tszaks/pallium/cmd.buildVersion=v${version}" \
    -o "$work/pallium" .
  cp "$root/README.md" "$root/LICENSE" "$work/"
  tar -C "$dist" -czf "$dist/$name.tar.gz" "$name"
  rm -rf "$work"
done

(cd "$dist" && shasum -a 256 *.tar.gz > checksums.txt)
