#!/usr/bin/env sh
# Cross-compiles the node operator's programs for every supported platform
# into ./release/ with zips and SHA256SUMS. Usage: scripts/release.sh [version]
# The browser app is the product for users; these archives are for people
# who run a node (docs/NODE.md, "Download").
set -eu
cd "$(dirname "$0")/.."
VERSION="${1:-1.1.0}"
OUT=release
mkdir -p "$OUT"
CMDS="argonmined argonmine-wallet argonmine-explorer argonmine-miner genesis"

# Fetch modules once so the cross-compiles never hit the network, and build
# the archiver for this machine (forward-slash paths, programs marked
# executable, fixed timestamps).
go mod download
go build -o "$OUT/mkzip" ./scripts/mkzip

build() {
  os=$1; arch=$2; shift 2
  name="argonmine-$VERSION-$os-$arch"
  dir="$OUT/$name"
  rm -rf "$dir"; mkdir -p "$dir"
  for c in "$@"; do
    exe="$c"; [ "$os" = windows ] && exe="$c.exe"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$dir/$exe" "./cmd/$c"
  done
  cp SECURITY.md "$dir/"; cp scripts/README-node.txt "$dir/README.txt"
  mkdir -p "$dir/docs"; cp docs/NODE.md docs/SPEC.md "$dir/docs/"
  if [ "$os" = windows ]; then cp scripts/install-windows.ps1 "$dir/"; else cp scripts/install.sh "$dir/"; fi
  rm -f "$OUT/$name.zip"; "$OUT/mkzip" "$OUT/$name.zip" "$dir"
  rm -rf "$dir"
  echo "built $OUT/$name.zip"
}

build windows amd64 $CMDS
build windows arm64 $CMDS
build linux   amd64 $CMDS
build linux   arm64 $CMDS
build darwin  amd64 $CMDS
build darwin  arm64 $CMDS

rm -f "$OUT/mkzip"
(cd "$OUT" && sha256sum *.zip > SHA256SUMS)
echo "wrote $OUT/SHA256SUMS"
