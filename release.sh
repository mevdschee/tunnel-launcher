#!/bin/bash
set -e
cd "$(dirname "$0")"
if [ ! -f fyne-cross/dist/tunnel-launcher-amd64.app.zip ]; then
  ./package.sh
fi
newTag=v$(grep '^Version' FyneApp.toml | cut -d'"' -f2)
#gh release delete $newTag
gh release create $newTag --title "${1:-$newTag}" \
  fyne-cross/dist/tunnel-launcher-amd64.tar.xz \
  fyne-cross/dist/tunnel-launcher-arm64.tar.xz \
  fyne-cross/dist/tunnel-launcher-amd64.exe.zip \
  fyne-cross/dist/tunnel-launcher-arm64.exe.zip \
  fyne-cross/dist/tunnel-launcher-amd64.app.zip \
  fyne-cross/dist/tunnel-launcher-arm64.app.zip
