# デスクトップアプリ 署名・公証・GitHub 公開ランブック

設計の正: [docs/superpowers/specs/2026-07-21-desktop-release-signing-design.md](superpowers/specs/2026-07-21-desktop-release-signing-design.md)

calsync デスクトップアプリ(macOS)の dmg は、Developer ID Application 署名 + Notary Service 公証付きで
GitHub Releases に公開する。Mac App Store 版は作らない。self-updater は対象外(更新は `git pull` +
再ビルドが正)。

Apple Account のパスワード・2 要素認証コード・秘密鍵を他人と共有して作業してはいけない。

## 1. 公開方式

| 項目 | 方針 |
| --- | --- |
| 配布先 | GitHub Releases(タグ `desktop-v<version>`。デーモン本体のタグ `v*` とは独立) |
| 署名 | Developer ID Application(hardened runtime) |
| 公証 | notarytool(App Store Connect API キー方式) |
| 成果物 | dmg・checksums.txt |
| 更新 | なし(`git pull` + 再ビルドが正) |

## 2. 初回準備

署名・公証の資格情報はこのリポジトリ単位ではなく、チーム/マシン単位で管理する。**新規に証明書や
API キーを作成する必要はない**。チーム共有の Developer ID Application 証明書と公証用 App Store
Connect API キーが、リリースを実行する Mac にすでに用意されている前提で進める(未整備の場合は
リリース作業を行う前にチーム内で確認すること。作成手順自体は本書の対象外)。

### 2.1 前提ツール

リリース実行環境に次を用意し、`scripts/release-desktop.sh` の preflight で存在確認する。

- `gh`(GitHub CLI): `gh auth status` で確認。導入: `brew install gh`
- `cargo` / `npm` / `xcrun notarytool`(Xcode Command Line Tools): `xcode-select --install`

### 2.2 Developer ID Application 証明書

キーチェーンに証明書と秘密鍵が組で入っていることを確認する。

```bash
security find-identity -v -p codesigning
```

`release.env` の `APPLE_SIGNING_IDENTITY` には、この出力に表示される文字列(`Developer ID
Application: ORG NAME (TEAMID)` の形式)をそのまま設定する。証明書が無い/秘密鍵と組にならない
場合は、証明書を発行したものとは別の Mac で作業しようとしている可能性が高い。チームの証明書管理者に
確認すること。

### 2.3 公証用 API キー

以下がリリース実行環境に置かれている前提。

- Key ID
- Issuer ID
- `~/.appstoreconnect/private_keys/AuthKey_<KEY_ID>.p8`

`.p8` は一度しかダウンロードできない秘密鍵。リポジトリ・Issue・PR・チャットに貼らないこと。

## 3. release.env

シークレットはリポジトリ外の `~/.config/calsync/release.env` に置く(`CALSYNC_RELEASE_ENV` 環境変数で
上書き可能。テスト等で別パスを使いたい場合に使う)。`scripts/release-desktop.sh` を初回実行すると
テンプレートを自動生成して終了する(`chmod 600` 済み)。

```bash
# --- Apple 署名(`security find-identity -v -p codesigning` の表示をそのまま) ---
APPLE_SIGNING_IDENTITY="Developer ID Application: YOUR ORG (TEAMID)"
# --- 公証(App Store Connect API キー方式) ---
APPLE_API_KEY="ABC123DEFG"                                    # Key ID(10桁)
APPLE_API_ISSUER="00000000-0000-0000-0000-000000000000"       # Issuer ID(UUID)
APPLE_API_KEY_PATH="$HOME/.appstoreconnect/private_keys/AuthKey_ABC123DEFG.p8"
```

値を埋めたら、ファイルとキー本体の権限を確認する(テンプレート生成時に `release.env` 自体は
自動で 600 になるが、`.p8` は各自ダウンロード時のパーミッションのまま残るため必要なら締める)。

```bash
chmod 600 "$HOME/.config/calsync/release.env"
chmod 600 "$HOME/.appstoreconnect/private_keys/AuthKey_ABC123DEFG.p8"
```

## 4. 公開手順

1. `desktop/package.json`・`desktop/src-tauri/tauri.conf.json`・`desktop/src-tauri/Cargo.toml` の
   `version` を揃え、`CHANGELOG.md` の `[Unreleased]` を `## [<version>] - <日付>` に変更する
2. PR を作成し、レビューと通常の検証(`go build`・`go test ./... -race -count=1`・`go vet`・
   `gofmt`・`docker compose config -q`。デスクトップ側の変更があれば `cd desktop && npm run build && npm test`)
   を通してから `main` へ merge する
3. push 済みでクリーンな `main` に同期する(`git checkout main && git pull`)
4. `desktop/src-tauri/binaries/` に古いサイドカーが残っていないか確認する(`npm run build-sidecar` で
   都度再生成されるので通常は気にしなくてよい)
5. `./scripts/release-desktop.sh <version>` を実行する

`scripts/release-desktop.sh` は release.env 読込・preflight・Git/バージョン整合チェック・
サイドカー同梱ビルド・`cargo tauri build`(署名・公証・.app の staple を含む)・生成物検証・dmg の
公証+staple・checksums.txt 生成・GitHub Release の draft 作成から publish までを一括で行う
(手順の詳細はスクリプト冒頭のコメント、または
[設計書 §4](superpowers/specs/2026-07-21-desktop-release-signing-design.md#4-releaseenv-とスクリプトの仕様)
を参照)。

## 5. 公開後の確認

GitHub から dmg を新しくダウンロードして確認する。

```bash
codesign --verify --deep --strict --verbose=2 /Applications/calsync.app
spctl -a -t exec -vv /Applications/calsync.app
xcrun stapler validate /Applications/calsync.app
```

- Gatekeeper が受け入れる(警告なしで起動する)
- Authority が Developer ID Application である
- .app / dmg の staple 検証が成功する

実ブラウザで dmg をダウンロード → マウント → `/Applications` へコピー → ダブルクリックで警告なしに
起動することまで確認する。

## 6. トラブルシュート

### tauri#11992: externalBin(サイドカー)同梱時の公証失敗

calsync の Go サイドカーは `externalBin` として bundle され、通常は `cargo tauri build` が
Developer ID 署名の対象に含める想定だが、バンドラーのバージョンによってはサイドカーが
署名対象から漏れ、公証が unsigned binary で失敗することが報告されている
([tauri-apps/tauri#11992](https://github.com/tauri-apps/tauri/issues/11992))。

`scripts/release-desktop.sh` はビルド後にサイドカーの署名を個別検証しており、失敗すると
このエラーで中断する。発生した場合はビルド前にサイドカーを手動でプレ署名してから
`cargo tauri build` を実行し直す。

```bash
codesign --force --options runtime --timestamp \
  --sign "$APPLE_SIGNING_IDENTITY" \
  desktop/src-tauri/binaries/calsync-*
```

`$APPLE_SIGNING_IDENTITY` は `release.env` に設定した値と同じもの(`security find-identity -v -p
codesigning` の表示)を使う。プレ署名後に `scripts/release-desktop.sh <version>` を再実行する。

## 7. 公式資料

- [Developer ID 証明書の作成](https://developer.apple.com/help/account/certificates/create-developer-id-certificates/)
- [macOS ソフトウェアの公証](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)
- [App Store Connect API](https://developer.apple.com/help/app-store-connect/get-started/app-store-connect-api)
