#!/bin/bash
#
# go install github.com/fyne-io/fyne-cross@latest
#
set -e
cd "$(dirname "$0")"
# darwin needs a copy of the macOS SDK, see the "OSX build" section in README.md
SDK=${MACOSX_SDK:-$HOME/SDKs/MacOSX12.3.sdk}
# zig does not apply the sysroot to the framework search path that fyne-cross
# passes, so point /System/Library/Frameworks at the mounted SDK instead
DARWIN_IMAGE=fyne-cross-darwin-sdk
docker build -q -t $DARWIN_IMAGE - <<'EOF'
FROM fyneio/fyne-cross-images:darwin
RUN mkdir -p /System/Library && ln -s /sdk/System/Library/Frameworks /System/Library/Frameworks
EOF
~/go/bin/fyne-cross windows -arch=amd64 -tags=no_animations,gles
~/go/bin/fyne-cross windows -arch=arm64 -tags=no_animations,gles
~/go/bin/fyne-cross linux   -arch=amd64 -tags=no_animations,gles
~/go/bin/fyne-cross linux   -arch=arm64 -tags=no_animations,gles
# gles is a no-op on darwin, fyne always takes the desktop GL path there
~/go/bin/fyne-cross darwin  -arch=amd64 -tags=no_animations -app-id com.tqdev.tunnel-launcher -macosx-sdk-path "$SDK" -image $DARWIN_IMAGE
~/go/bin/fyne-cross darwin  -arch=arm64 -tags=no_animations -app-id com.tqdev.tunnel-launcher -macosx-sdk-path "$SDK" -image $DARWIN_IMAGE
mv fyne-cross/dist/linux-arm64/tunnel-launcher.tar.xz   fyne-cross/dist/tunnel-launcher-arm64.tar.xz
mv fyne-cross/dist/linux-amd64/tunnel-launcher.tar.xz   fyne-cross/dist/tunnel-launcher-amd64.tar.xz
mv fyne-cross/dist/windows-arm64/tunnel-launcher.exe.zip fyne-cross/dist/tunnel-launcher-arm64.exe.zip
mv fyne-cross/dist/windows-amd64/tunnel-launcher.exe.zip fyne-cross/dist/tunnel-launcher-amd64.exe.zip
(cd fyne-cross/dist/darwin-arm64 && zip -qry ../tunnel-launcher-arm64.app.zip tunnel-launcher.app)
(cd fyne-cross/dist/darwin-amd64 && zip -qry ../tunnel-launcher-amd64.app.zip tunnel-launcher.app)
