#!/usr/bin/env bash
# Go 本体をビルドして Tauri の externalBin 命名(ターゲットトリプル付き)で配置する
set -euo pipefail
cd "$(dirname "$0")/../.."
TRIPLE=$(rustc -vV | awk '/^host:/{print $2}')
mkdir -p desktop/src-tauri/binaries
go build -o "desktop/src-tauri/binaries/calsync-${TRIPLE}" ./cmd/calsync
echo "sidecar: desktop/src-tauri/binaries/calsync-${TRIPLE}"
