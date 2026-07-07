#!/usr/bin/env bash
# calsync macOS ネイティブ常駐(launchd)インストーラ
#
# 設計の正: docs/superpowers/specs/2026-07-07-macos-launchd-design.md (3.2節)
# 冪等: 何度実行してもビルド → plist 再生成 → 再登録 → 再起動をやり直すだけで安全。
#
# 環境変数による上書き:
#   CALSYNC_BIN   calsync バイナリの配置ディレクトリ(既定 ~/.local/bin)
#   CALSYNC_DATA  データディレクトリ(既定 <repo>/data。calsync.yaml もここに置く)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

LABEL="com.btajp.calsync"
BIN_DIR="${CALSYNC_BIN:-${HOME}/.local/bin}"
DATA_DIR="${CALSYNC_DATA:-${REPO_ROOT}/data}"
LOG_FILE="${HOME}/Library/Logs/calsync.log"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST_PATH="${PLIST_DIR}/${LABEL}.plist"
TEMPLATE_PATH="${SCRIPT_DIR}/com.btajp.calsync.plist.template"
CONFIG_PATH="${DATA_DIR}/calsync.yaml"
COMPOSE_FILE="${REPO_ROOT}/docker-compose.yaml"

log() { echo "[install-launchd] $*"; }
die() { echo "[install-launchd] エラー: $*" >&2; exit 1; }

# --- 1. 前提チェック -------------------------------------------------------

if [[ "$(uname -s)" != "Darwin" ]]; then
  die "このスクリプトは macOS 専用です"
fi

if ! command -v python3 >/dev/null 2>&1; then
  die "python3 が見つかりません。Xcode Command Line Tools をインストールしてください(xcode-select --install)"
fi

if [[ ! -d "${DATA_DIR}" ]]; then
  die "データディレクトリが見つかりません: ${DATA_DIR}(CALSYNC_DATA で変更可)"
fi

if [[ ! -f "${CONFIG_PATH}" ]]; then
  die "設定ファイルが見つかりません: ${CONFIG_PATH}"
fi

# コンテナ運用からの移行時の落とし穴(実測 2026-07-07): credentials_file が
# コンテナ内パス(/data/...)のままだとネイティブでは読めずクラッシュループする。
if grep -qE '^[[:space:]]*credentials_file:[[:space:]]*/data/' "${CONFIG_PATH}"; then
  die "calsync.yaml の credentials_file がコンテナ内パス(/data/...)のままです。ホストの絶対パス(例: ${DATA_DIR}/google-client.json)に変更してください"
fi

if [[ ! -f "${TEMPLATE_PATH}" ]]; then
  die "plist テンプレートが見つかりません: ${TEMPLATE_PATH}"
fi

TOKEN_ENV_NAME=""
TOKEN_VALUE=""
# 注意: この正規表現はトップレベルのブロック形式キー(`notifications:` が行末で
# 単独)のみを検知する。フロースタイル(`notifications: {...}`)やコメントアウト・
# インデント違いなど YAML の他の書き方には対応していない。
if grep -Eq '^[[:space:]]*notifications:[[:space:]]*$' "${CONFIG_PATH}"; then
  TOKEN_ENV_NAME="$(grep -m1 -E '^[[:space:]]*bot_token_env:' "${CONFIG_PATH}" \
    | sed -E 's/^[^:]*:[[:space:]]*"?([A-Za-z_][A-Za-z0-9_]*)"?.*/\1/')"
  if [[ -z "${TOKEN_ENV_NAME}" ]]; then
    TOKEN_ENV_NAME="SLACK_BOT_TOKEN"
    log "bot_token_env の指定がないため既定値 ${TOKEN_ENV_NAME} を使います"
  fi

  TOKEN_VALUE="${!TOKEN_ENV_NAME:-}"
  if [[ -z "${TOKEN_VALUE}" ]]; then
    die "notifications が設定されていますが、環境変数 ${TOKEN_ENV_NAME} が現在のシェルに存在しません(または空です)。export ${TOKEN_ENV_NAME}=xoxb-... のあと再実行してください"
  fi
  log "Slack 通知用トークンを環境変数 ${TOKEN_ENV_NAME} から取得しました"
else
  log "notifications 未設定のため、トークンチェックをスキップします(EnvironmentVariables なしの plist を生成します)"
fi

# --- 2. ビルド --------------------------------------------------------------

mkdir -p "${BIN_DIR}"
log "calsync をビルドします: ${BIN_DIR}/calsync"
( cd "${REPO_ROOT}" && go build -o "${BIN_DIR}/calsync" ./cmd/calsync )

# --- 3. データディレクトリ確認(1.で実施済み) --------------------------------
log "データディレクトリ: ${DATA_DIR}"

# --- 4. Docker 稼働中コンテナのチェック --------------------------------------

if command -v docker >/dev/null 2>&1; then
  if docker info >/dev/null 2>&1; then
    running_services=""
    if [[ -f "${COMPOSE_FILE}" ]]; then
      running_services="$(docker compose -f "${COMPOSE_FILE}" ps --status running --services 2>/dev/null || true)"
    fi
    if grep -qx "calsync" <<< "${running_services}"; then
      die "Docker の calsync コンテナが稼働中です。先に 'docker compose down' を実行してから再実行してください(移行時の二重運用事故防止)"
    fi
    log "Docker の calsync コンテナは稼働していません"
  else
    log "Docker デーモンが起動していないため、コンテナチェックをスキップします"
  fi
else
  log "docker コマンドが見つからないため、コンテナチェックをスキップします"
fi

# --- 5. plist 生成(sed ではなく python3。トークンの特殊文字対策) -----------

mkdir -p "${PLIST_DIR}"
mkdir -p "$(dirname "${LOG_FILE}")"
touch "${LOG_FILE}"

log "plist を生成します: ${PLIST_PATH}"
CALSYNC_BIN_DIR="${BIN_DIR}" \
CALSYNC_DATA_DIR="${DATA_DIR}" \
CALSYNC_LOG_FILE="${LOG_FILE}" \
CALSYNC_TOKEN_ENV_NAME="${TOKEN_ENV_NAME}" \
CALSYNC_TOKEN_VALUE="${TOKEN_VALUE}" \
CALSYNC_TEMPLATE_PATH="${TEMPLATE_PATH}" \
CALSYNC_PLIST_PATH="${PLIST_PATH}" \
python3 - <<'PYEOF'
import os
from xml.sax.saxutils import escape

template_path = os.environ["CALSYNC_TEMPLATE_PATH"]
plist_path = os.environ["CALSYNC_PLIST_PATH"]
# plist は XML なので、埋め込み値に & < > が含まれると構造が壊れる。
# xml.sax.saxutils.escape() で実体参照化してから置換する。
bin_dir = escape(os.environ["CALSYNC_BIN_DIR"])
data_dir = escape(os.environ["CALSYNC_DATA_DIR"])
log_file = escape(os.environ["CALSYNC_LOG_FILE"])
token_env_name = escape(os.environ.get("CALSYNC_TOKEN_ENV_NAME", ""))
token_value = escape(os.environ.get("CALSYNC_TOKEN_VALUE", ""))

with open(template_path, "r", encoding="utf-8") as f:
    content = f.read()

start_marker = "<!-- ENVIRONMENT_VARIABLES_START -->"
end_marker = "<!-- ENVIRONMENT_VARIABLES_END -->"
start = content.index(start_marker)
end = content.index(end_marker) + len(end_marker)

if token_env_name:
    # notifications 設定あり: マーカーだけ剥がしてトークン名・値を埋め込む
    block = content[start:end]
    block = block.replace(start_marker, "").replace(end_marker, "")
    block = block.replace("__TOKEN_ENV_NAME__", token_env_name)
    block = block.replace("__TOKEN_VALUE__", token_value)
    content = content[:start] + block + content[end:]
else:
    # notifications 未設定: EnvironmentVariables ブロックごと削除
    content = content[:start] + content[end:]

content = content.replace("__BIN__", bin_dir)
content = content.replace("__DATA__", data_dir)
content = content.replace("__LOG__", log_file)

# トークンを含みうるため、最初から 600 で作成する(作成後の chmod だと
# 一瞬でも既定パーミッションの窓が空くため os.open の mode で直接指定)
fd = os.open(plist_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as f:
    f.write(content)
os.chmod(plist_path, 0o600)
PYEOF

log "plist を配置しました(600): ${PLIST_PATH}"

# --- 6. 登録・起動 -----------------------------------------------------------

UID_NUM="$(id -u)"
DOMAIN="gui/${UID_NUM}"

log "既存の LaunchAgent を解除します(未登録なら何もしません)"
launchctl bootout "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || true
# bootout は非同期に完了することがあり、直後の bootstrap が EIO(5)で失敗する
# (実測 2026-07-07)。解除の完了(print が失敗する状態)を待ってから登録する。
for _ in $(seq 1 20); do
  launchctl print "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || break
  sleep 0.5
done

log "LaunchAgent を登録します"
bootstrap_ok=0
for _ in $(seq 1 5); do
  if launchctl bootstrap "${DOMAIN}" "${PLIST_PATH}" 2>/dev/null; then
    bootstrap_ok=1
    break
  fi
  sleep 1
done
if [[ "${bootstrap_ok}" != 1 ]]; then
  die "launchctl bootstrap に失敗しました(数秒おいて再実行してください)"
fi

log "起動します"
# -k: 既に稼働中なら kill してから再起動、未稼働なら起動する。
# bootstrap の RunAtLoad で既に起動済みの状態でも set -e で落ちないための冪等化。
launchctl kickstart -k "${DOMAIN}/${LABEL}"

# --- 7. 起動確認 --------------------------------------------------------------

sleep 1
log "起動状態:"
launchctl print "${DOMAIN}/${LABEL}" 2>/dev/null | grep -E "state = " || log "  (状態を取得できませんでした。launchctl print ${DOMAIN}/${LABEL} で確認してください)"

log "ログ末尾(${LOG_FILE}):"
tail -n 3 "${LOG_FILE}" 2>/dev/null || true

log "完了しました"
