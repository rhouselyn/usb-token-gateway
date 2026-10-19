#!/usr/bin/env sh
# build.sh — Linux/macOS 构建（网关本体跨平台；托盘/接管等 Windows 专属能力自动降级）
#
#   ./build.sh          # 开发版 go/usbgw
#   ./build.sh release  # 产物 usb/usb-bridge（无 .exe 后缀）
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
GO=${GO_BIN:-go}
MODE=${1:-dev}

echo "[build] go      = $GO"
echo "[build] project = $ROOT"

if [ "$MODE" = "release" ]; then
    mkdir -p "$ROOT/usb"
    (cd "$ROOT/go" && "$GO" build -trimpath -ldflags "-s -w" -o "$ROOT/usb/usb-bridge" .)
    echo "[build] 完成：usb/usb-bridge"
else
    (cd "$ROOT/go" && "$GO" build -o usbgw .)
    echo "[build] 完成：go/usbgw"
fi
