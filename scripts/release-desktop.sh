#!/usr/bin/env bash
# calsync デスクトップアプリの署名済みリリース(dmg・公証・GitHub Release)。
#
# 使い方: ./scripts/release-desktop.sh <version 例: 0.1.0>
# 前提: ${CALSYNC_RELEASE_ENV:-$HOME/.config/calsync/release.env}
#       (無ければテンプレートを生成して終了する。値の埋め方は docs/desktop-release-runbook.md 参照)
# 前提ツール: gh(GitHub CLI)・cargo・npm・xcrun notarytool(Xcode Command Line Tools)
# 実行条件: push 済み・クリーンな main からのみ実行できる(下記 3. で強制する)。
# 署名・公証の資格情報(Developer ID Application 証明書・公証用 API キー)はチーム/マシン単位で
# 構築済みのものを使う。新規作成はしない。
#
# やること(順に・失敗したら即中断):
#   1. release.env 読込 + 必須変数・.p8・updater 署名鍵(TAURI_SIGNING_PRIVATE_KEY)の存在確認
#      (Apple ID 方式の env は unset)
#   2. preflight: gh・cargo・npm・xcrun notarytool の存在確認
#   3. Git 前提(push 済み・クリーンな main のみ)・タグ desktop-v<version> の未使用チェック
#   4. バージョン整合(package.json・tauri.conf.json・Cargo.toml・CHANGELOG)
#   5. cd desktop && npm ci && npm run build-sidecar
#   6. tauri build(署名・公証・staple(.app)は bundler が env から自動実行。updater
#      アーティファクト(.app.tar.gz/.sig)は tauri.release.conf.json の
#      createUpdaterArtifacts で有効化・TAURI_SIGNING_PRIVATE_KEY で minisign 署名)
#   7. 生成物検証(.app / dmg / .app.tar.gz / .sig の存在・署名・公証・サイドカー個別署名)
#   8. dmg 自体の公証 + staple(Tauri が staple するのは .app のみ)
#   9. checksums.txt 生成(shasum -a 256)
#  10. latest.json 生成(signature には .sig ファイルの中身を埋め込む)
#  11. GitHub Release(draft で全アセットを揃えてから publish = 原子的公開)
set -euo pipefail

usage() {
  cat <<USAGE
使い方: $0 <version 例: 0.1.0>
USAGE
}

VERSION="${1:-}"
if [[ "$VERSION" == "-h" || "$VERSION" == "--help" ]]; then
  usage
  exit 0
fi
if [[ -z "$VERSION" ]]; then
  usage >&2
  exit 2
fi

REPO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." >/dev/null 2>&1 && pwd -P)"
ENV_FILE="${CALSYNC_RELEASE_ENV:-$HOME/.config/calsync/release.env}"
BUNDLE_DIR="$REPO_DIR/desktop/src-tauri/target/release/bundle"
APP="$BUNDLE_DIR/macos/calsync.app"
DMG="$BUNDLE_DIR/dmg/calsync_${VERSION}_aarch64.dmg"
TARGZ="$BUNDLE_DIR/macos/calsync.app.tar.gz"
SIG="$TARGZ.sig"
TAG="desktop-v$VERSION"

# 1. release.env(無ければテンプレートを生成して人間に返す)
if [[ ! -f "$ENV_FILE" ]]; then
  mkdir -p "$(dirname -- "$ENV_FILE")"
  cat > "$ENV_FILE" <<'TMPL'
# calsync デスクトップリリース用シークレット(このファイルはリポジトリ外に置く・chmod 600)
# --- Apple 署名(`security find-identity -v -p codesigning` の表示をそのまま) ---
APPLE_SIGNING_IDENTITY="Developer ID Application: YOUR ORG (TEAMID)"
# --- 公証(App Store Connect API キー方式) ---
APPLE_API_KEY="ABC123DEFG"                                    # Key ID(10桁)
APPLE_API_ISSUER="00000000-0000-0000-0000-000000000000"       # Issuer ID(UUID)
APPLE_API_KEY_PATH="$HOME/.appstoreconnect/private_keys/AuthKey_ABC123DEFG.p8"
# --- updater 署名(Tauri minisign 鍵。Apple 署名とは別物) ---
TAURI_SIGNING_PRIVATE_KEY="$HOME/.tauri/calsync-updater.key"
TAURI_SIGNING_PRIVATE_KEY_PASSWORD=""                          # 鍵にパスワードが無ければ空のまま
TMPL
  chmod 600 "$ENV_FILE"
  echo "release.env のテンプレートを生成しました: $ENV_FILE"
  echo "値を埋めてから再実行してください(書式・入手先は docs/desktop-release-runbook.md を参照)。"
  exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"
for v in APPLE_SIGNING_IDENTITY APPLE_API_KEY APPLE_API_ISSUER APPLE_API_KEY_PATH TAURI_SIGNING_PRIVATE_KEY; do
  [[ -n "${!v:-}" ]] || { echo "ERROR: $ENV_FILE の $v が未設定です" >&2; exit 1; }
done
[[ -f "$APPLE_API_KEY_PATH" ]] || { echo "ERROR: APPLE_API_KEY_PATH が存在しません: $APPLE_API_KEY_PATH" >&2; exit 1; }
[[ -f "$TAURI_SIGNING_PRIVATE_KEY" ]] || { echo "ERROR: TAURI_SIGNING_PRIVATE_KEY が存在しません: $TAURI_SIGNING_PRIVATE_KEY" >&2; exit 1; }
export APPLE_SIGNING_IDENTITY APPLE_API_KEY APPLE_API_ISSUER APPLE_API_KEY_PATH TAURI_SIGNING_PRIVATE_KEY
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}"
# Apple ID 方式(APPLE_ID/APPLE_PASSWORD/APPLE_TEAM_ID)が API キー方式より先に評価される
# (tauri-cli 実装)ため、環境に混入していたら外して API キー方式に固定する。
unset APPLE_ID APPLE_PASSWORD APPLE_TEAM_ID APPLE_CERTIFICATE APPLE_CERTIFICATE_PASSWORD 2>/dev/null || true

# 2. preflight: 前提ツールの不足は、長いビルド・公証が走る前に検知して即中断する。
command -v gh >/dev/null 2>&1 || { echo "ERROR: gh (GitHub CLI) が見つかりません。導入: brew install gh" >&2; exit 1; }
command -v cargo >/dev/null 2>&1 || { echo "ERROR: cargo が見つかりません。導入: https://rustup.rs" >&2; exit 1; }
command -v npm >/dev/null 2>&1 || { echo "ERROR: npm が見つかりません。導入: https://nodejs.org (または brew install node)" >&2; exit 1; }
xcrun notarytool --version >/dev/null 2>&1 || { echo "ERROR: xcrun notarytool が見つかりません。導入: xcode-select --install" >&2; exit 1; }

echo "== calsync desktop release v$VERSION =="

# 3. Git 前提チェック: push 済みのクリーンな main からのみリリースできる。
#    gh release create はタグをリモートに新規作成するため、ローカルのビルド元コミットが
#    origin/main の HEAD と一致していないと「タグ・Source アーカイブと配布物の不一致」という
#    壊れたリリースになる。
BRANCH="$(git -C "$REPO_DIR" branch --show-current)"
[[ "$BRANCH" == "main" ]] || { echo "ERROR: リリースは main ブランチから実行してください(現在: $BRANCH)" >&2; exit 1; }
[[ -z "$(git -C "$REPO_DIR" status --porcelain --untracked-files=all)" ]] || {
  echo "ERROR: 作業ツリーに未コミットの変更があります" >&2
  exit 1
}
git -C "$REPO_DIR" fetch origin main --quiet
git -C "$REPO_DIR" fetch --quiet --tags origin
HEAD_SHA="$(git -C "$REPO_DIR" rev-parse HEAD)"
[[ "$HEAD_SHA" == "$(git -C "$REPO_DIR" rev-parse origin/main)" ]] || {
  echo "ERROR: ローカル main が origin/main と一致しません。先に git push してください" >&2
  exit 1
}
if git -C "$REPO_DIR" rev-parse "$TAG" >/dev/null 2>&1; then
  echo "ERROR: タグ $TAG は既に存在します" >&2
  exit 1
fi

# 4. バージョン整合(package.json・tauri.conf.json・Cargo.toml・CHANGELOG)
json_ver() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['version'])" "$1"; }
[[ "$(json_ver "$REPO_DIR/desktop/package.json")" == "$VERSION" ]] || {
  echo "ERROR: desktop/package.json の version が $VERSION ではありません" >&2
  exit 1
}
[[ "$(json_ver "$REPO_DIR/desktop/src-tauri/tauri.conf.json")" == "$VERSION" ]] || {
  echo "ERROR: desktop/src-tauri/tauri.conf.json の version が $VERSION ではありません" >&2
  exit 1
}
grep -q "^version = \"$VERSION\"" "$REPO_DIR/desktop/src-tauri/Cargo.toml" || {
  echo "ERROR: desktop/src-tauri/Cargo.toml の version が $VERSION ではありません" >&2
  exit 1
}
grep -q "^## \[$VERSION\]" "$REPO_DIR/CHANGELOG.md" || {
  echo "ERROR: CHANGELOG.md に [$VERSION] 節がありません" >&2
  exit 1
}

# 5. サイドカー同梱ビルドの準備
echo "-- npm ci && build-sidecar"
(cd "$REPO_DIR/desktop" && npm ci && npm run build-sidecar)

# 6. ビルド(署名・公証・.app の staple は bundler が env から自動実行)
# CI=true は Tauri の create-dmg へ --skip-jenkins を渡し、Finder AppleScript への依存を外すため。
echo "-- tauri build(署名・公証込み・数分かかります)"
(cd "$REPO_DIR/desktop" && CI=true npm run tauri build -- --config src-tauri/tauri.release.conf.json)

# 7. 生成物の存在・署名・公証を検証
for p in "$APP" "$DMG"; do
  [[ -e "$p" ]] || { echo "ERROR: 生成物がありません: $p" >&2; exit 1; }
done
for p in "$TARGZ" "$SIG"; do
  [[ -e "$p" ]] || {
    echo "ERROR: updater 生成物がありません: $p(TAURI_SIGNING_PRIVATE_KEY の設定を確認してください)" >&2
    exit 1
  }
done
echo "-- 署名・公証の検証"
codesign --verify --deep --strict --verbose=2 "$APP"
xcrun stapler validate "$APP"
spctl -a -t exec -vv "$APP"

echo "-- サイドカーの個別署名検証"
SIDECAR_DIR="$APP/Contents/MacOS"
[[ -d "$SIDECAR_DIR" ]] || { echo "ERROR: $SIDECAR_DIR がありません" >&2; exit 1; }
SIDECAR_FAILED=0
while IFS= read -r -d '' f; do
  if file "$f" | grep -q "Mach-O"; then
    if ! codesign --verify --strict "$f"; then
      echo "ERROR: サイドカーの署名検証に失敗しました: $f" >&2
      SIDECAR_FAILED=1
    fi
  fi
done < <(find "$SIDECAR_DIR" -type f -print0)
if [[ "$SIDECAR_FAILED" -ne 0 ]]; then
  echo "ERROR: 既知の問題(tauri-apps/tauri#11992)の可能性があります。docs/desktop-release-runbook.md のトラブルシュート節(サイドカーのプレ署名の回避策)を参照してください" >&2
  exit 1
fi

# 8. dmg 自体の公証 + staple(Tauri が staple するのは .app のみ)
echo "-- dmg 公証(数分かかります)"
xcrun notarytool submit "$DMG" --key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait
xcrun stapler staple "$DMG"
xcrun stapler validate "$DMG"

# 9. checksums.txt 生成
CHECKSUMS="$BUNDLE_DIR/dmg/checksums.txt"
(cd "$(dirname "$DMG")" && shasum -a 256 "$(basename "$DMG")" > "$CHECKSUMS")

# 10. latest.json 生成(signature は .sig ファイルの中身)
LATEST_JSON="$BUNDLE_DIR/latest.json"
SIG_CONTENT="$(cat "$SIG")" \
ASSET_URL="https://github.com/btajp/calsync/releases/download/${TAG}/calsync.app.tar.gz" \
REL_VERSION="$VERSION" PUB_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
python3 - "$LATEST_JSON" <<'PY'
import json, os, sys
json.dump({
    "version": os.environ["REL_VERSION"],
    "pub_date": os.environ["PUB_DATE"],
    "platforms": {
        "darwin-aarch64": {
            "signature": os.environ["SIG_CONTENT"],
            "url": os.environ["ASSET_URL"],
        }
    },
}, open(sys.argv[1], "w"), indent=2)
PY

# 11. GitHub Release(draft で全アセットを揃えてから publish = 原子的公開)
echo "-- GitHub Release 作成"
NOTES_FILE="$(mktemp)"
python3 - "$REPO_DIR/CHANGELOG.md" "$VERSION" > "$NOTES_FILE" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
m = re.search(rf"^## \[{re.escape(sys.argv[2])}\][^\n]*\n(.*?)(?=^## \[|\Z)", text, re.S | re.M)
print(m.group(1).strip() if m else "")
PY
gh release create "$TAG" --draft --target "$HEAD_SHA" --title "$TAG" --notes-file "$NOTES_FILE" \
  "$DMG" "$CHECKSUMS" "$TARGZ" "$LATEST_JSON"
gh release edit "$TAG" --draft=false
rm -f "$NOTES_FILE"
git -C "$REPO_DIR" fetch --tags

echo ""
echo "== リリース完了: $TAG =="
echo "事後スモーク(docs/desktop-release-runbook.md 参照):"
echo "  ブラウザで dmg を実ダウンロード → マウント → /Applications へコピー → 警告なしに起動すること"
