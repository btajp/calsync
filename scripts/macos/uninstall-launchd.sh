#!/usr/bin/env bash
# calsync macOS ネイティブ常駐(launchd)アンインストーラ
#
# 設計の正: docs/superpowers/specs/2026-07-07-macos-launchd-design.md (3.3節)
# launchctl bootout + plist 削除のみ行う。バイナリ(CALSYNC_BIN)と
# データディレクトリ(CALSYNC_DATA)は削除しない。冪等(未登録・未配置でも成功する)。
set -euo pipefail

LABEL="com.btajp.calsync"
PLIST_PATH="${HOME}/Library/LaunchAgents/${LABEL}.plist"
UID_NUM="$(id -u)"
DOMAIN="gui/${UID_NUM}"

log() { echo "[uninstall-launchd] $*"; }

log "LaunchAgent を解除します(未登録なら何もしません)"
launchctl bootout "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || true

if [[ -f "${PLIST_PATH}" ]]; then
  log "plist を削除します: ${PLIST_PATH}"
  rm -f "${PLIST_PATH}"
else
  log "plist は既に存在しません(スキップ): ${PLIST_PATH}"
fi

log "完了しました(バイナリと data/ は削除していません)"
