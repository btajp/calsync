# calsync デスクトップアプリ 署名・公証・リリース設計書

作成日: 2026-07-21
ステータス: 承認済みドラフト(実装の入力)
前提: デスクトップアプリ v1(2026-07-21-desktop-app-design.md)の実装済みブランチに積む

## 1. 目的と方針

デスクトップアプリの macOS 配布物(dmg)を Developer ID 署名+Notary Service 公証付きで GitHub Releases に公開できるようにする。方式は同組織の solo-eikaiwa で実運用済みのパイプラインの簡略版を移植する。

| 項目 | 方針 |
| --- | --- |
| 配布先 | GitHub Releases |
| 署名 | Developer ID Application(hardened runtime) |
| 公証 | notarytool(App Store Connect API キー方式) |
| 成果物 | dmg・checksums.txt・`.app.tar.gz`・`.sig`・`latest.json` |
| 更新 | Tauri self-updater(`tauri-plugin-updater`)。起動時の自動チェック+手動チェックの両方に対応 |

## 2. 流用する資産(新規作成ゼロ)

署名・公証の資格情報はアプリ単位ではなくチーム/マシン単位のため、solo-eikaiwa で構築済みのものをそのまま使う:

- Developer ID Application 証明書(キーチェーン。`security find-identity -v -p codesigning` で確認)
- 公証用 App Store Connect API キー(Key ID / Issuer ID / `~/.appstoreconnect/private_keys/AuthKey_<KEY_ID>.p8`)

Store 用 App ID・provisioning は使わない。bundle identifier は既存の `com.btajp.calsync.desktop` のままで公証できる。updater 署名鍵(Tauri minisign 鍵)は本設計で新規に用意する(§3)。

## 3. self-updater

`tauri-plugin-updater` + `tauri-plugin-process` による自己更新。Apple の署名・公証(§2)とは別に、更新アーティファクト自体を Tauri の minisign 鍵で署名する。

| 項目 | 方針 |
| --- | --- |
| 鍵 | minisign 鍵ペア。秘密鍵はチーム/マシン単位で `~/.tauri/calsync-updater.key` に保管(リポジトリに含めない) |
| 公開鍵 | `tauri.conf.json` の `plugins.updater.pubkey` に埋め込み(公開リポジトリに書いてよい。署名検証専用) |
| 成果物 | `calsync.app.tar.gz`(更新配布用アーカイブ)+ `.sig`(署名) |
| エンドポイント | `https://github.com/btajp/calsync/releases/latest/download/latest.json` |
| チェック | 起動時に一度(失敗は無視)+ ヘッダの手動リンク |
| インストール | `downloadAndInstall()` → `relaunch()`(`@tauri-apps/plugin-process`) |

**鍵の継続性**: 秘密鍵を失うと、既存インストール済みアプリへ新しい更新を配信する手段がなくなる(pubkey が変わると `tauri.conf.json` を書き換えた新バージョンを手動配布するしかなく、self-update の連鎖が切れる)。秘密鍵はバックアップを取り、公開鍵のローテーションは行わない。

**latest.json**: `scripts/release-desktop.sh` が生成し、GitHub Release のアセットに含める(solo-eikaiwa と同形式)。

```json
{
  "version": "0.2.0",
  "pub_date": "2026-07-21T00:00:00Z",
  "platforms": {
    "darwin-aarch64": {
      "signature": "<.sig ファイルの中身>",
      "url": "https://github.com/btajp/calsync/releases/download/desktop-v0.2.0/calsync.app.tar.gz"
    }
  }
}
```

**endpoint の注意(運用制約)**: `releases/latest/download/<file>` は GitHub API 上「そのリポジトリの全タグを横断した最新の安定版(non-prerelease・non-draft)リリース」を指す。calsync はデーモン本体(タグ `v*`)とデスクトップアプリ(タグ `desktop-v*`)を同一リポジトリの別リリース系列として公開しているため、**デーモンのリリースを `desktop-v*` より後に publish すると、`releases/latest` がデーモンのリリース(`latest.json` を含まない)を指してしまい、デスクトップアプリの更新チェックが 404 になる**。当面は次のいずれかで運用する:

1. デーモン本体のリリースにも(ダミーでよいので)`latest.json` を同梱する、または
2. デーモン本体のリリースは prerelease 扱いで publish し、`releases/latest` が指す対象からデーモンのリリースを除外する

本設計・実装時点ではデーモン本体は GitHub Releases を使っていない(Docker イメージ配布のみ)ため即座には顕在化しないが、将来デーモン単体のタグ付きリリースを GitHub Releases に追加する場合は、上記のいずれかを先に決めてから行うこと。

## 4. 成果物と変更点

1. **`desktop/src-tauri/tauri.release.conf.json`**: リリース専用オーバーレイ。`bundle.targets` を `["app", "dmg"]` に、`bundle.createUpdaterArtifacts` を `true` にする(`.app.tar.gz` + `.sig` を生成)。既定の `tauri.conf.json` は `targets: ["app"]` のまま(開発ビルドを遅くしない)
2. **`scripts/release-desktop.sh`**: リリース一括スクリプト(§5)。updater 署名鍵の存在確認・生成物検証・latest.json 生成を追加
3. **`docs/desktop-release-runbook.md`**: 初回準備(証明書・API キー・updater 鍵の確認)と公開手順・事後確認のランブック。README からリンク
4. CHANGELOG `[Unreleased]` 追記

## 5. release.env とスクリプトの仕様

シークレットはリポジトリ外の `~/.config/calsync/release.env`(chmod 600)。初回実行でテンプレートを生成して終了する。

```bash
APPLE_SIGNING_IDENTITY="Developer ID Application: YOUR ORG (TEAMID)"
APPLE_API_KEY="ABC123DEFG"
APPLE_API_ISSUER="00000000-0000-0000-0000-000000000000"
APPLE_API_KEY_PATH="$HOME/.appstoreconnect/private_keys/AuthKey_ABC123DEFG.p8"
TAURI_SIGNING_PRIVATE_KEY="$HOME/.tauri/calsync-updater.key"
TAURI_SIGNING_PRIVATE_KEY_PASSWORD=""
```

`TAURI_SIGNING_PRIVATE_KEY_PASSWORD` は鍵にパスワードを設定していなければ空のままでよい(必須変数ではない)。

`scripts/release-desktop.sh <version>` のステップ(失敗したら即中断):

1. release.env 読込+必須変数(`TAURI_SIGNING_PRIVATE_KEY` 含む)と `.p8`・updater 秘密鍵ファイルの存在確認。Apple ID 方式の env(`APPLE_ID` 等)は unset して API キー方式に固定(tauri-cli は Apple ID 方式を優先評価するため)
2. preflight: `gh`・`cargo`・`npm`・`xcrun notarytool` の存在確認
3. Git 前提: push 済み・クリーンな `main` からのみ実行可(タグ・Source アーカイブと配布物の一致を保証)。タグ `desktop-v<version>` が未使用であること
4. バージョン整合: `desktop/package.json`・`desktop/src-tauri/tauri.conf.json`・`desktop/src-tauri/Cargo.toml` の version と CHANGELOG の節が一致
5. `cd desktop && npm ci && npm run build-sidecar`
6. `CI=true npm run tauri build -- --config src-tauri/tauri.release.conf.json`(`CI=true` は create-dmg の Finder AppleScript 依存を外すため。署名・公証・staple は bundler が env から自動実行。updater アーティファクトは `TAURI_SIGNING_PRIVATE_KEY` で署名される)
7. 生成物検証: `.app`・dmg・`.app.tar.gz`・`.sig` の存在、`codesign --verify --deep --strict`、`xcrun stapler validate`(.app)、`spctl -a -t exec -vv`。**サイドカー(`Contents/MacOS/` 配下の calsync バイナリ)の署名も個別に `codesign --verify` する**(§7 リスク参照)。`.app.tar.gz` / `.sig` の欠落は `TAURI_SIGNING_PRIVATE_KEY` の設定漏れを疑う
8. dmg 自体の公証+staple(`xcrun notarytool submit --wait` → `xcrun stapler staple`。Tauri が staple するのは .app のみ)
9. checksums.txt 生成(shasum -a 256)
10. latest.json 生成(`signature` に `.sig` ファイルの中身をそのまま埋め込む)
11. GitHub Release: `desktop-v<version>` タグで draft 作成 → 全アセット(dmg・checksums.txt・`.app.tar.gz`・latest.json)添付後に publish(原子的公開)。ノートは CHANGELOG の該当節を抽出

デーモン本体のリリース(タグ `v*`)とは独立させるため、タグは `desktop-v*` プレフィックスを使う(§3 の endpoint 注意も参照)。

## 6. 事後確認(ランブックに記載)

```bash
codesign --verify --deep --strict --verbose=2 <app>
spctl -a -t exec -vv <app>
xcrun stapler validate <app> && xcrun stapler validate <dmg>
```

ブラウザで dmg を実ダウンロード → マウント → /Applications へコピー → 警告なしで起動、まで確認する。self-updater の E2E(旧バージョンを起動 → 更新検出 → ダウンロード → 再起動後に新バージョンで起動)は次回リリースで実測する(§7)。

## 7. リスクと実測項目

- **tauri#11992(externalBin 同梱時の公証失敗報告)**: calsync の Go サイドカーは `externalBin` 宣言なので bundler の署名対象に入る想定だが、初回リリースで実測する。失敗した場合の回避策(solo-eikaiwa の whisper-bin と同じプレ署名: ビルド前に `codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" desktop/src-tauri/binaries/calsync-*`)をランブックに記載しておく
- 公証には Apple Developer Program($99/年)の有効な membership が必要(solo-eikaiwa で契約済みの前提)
- **self-updater の E2E は未実測**: `tauri-plugin-updater` の `check()` / `downloadAndInstall()` / `relaunch()` の一連の流れは、実際に旧バージョンの `.app` から新バージョンへ更新できることを次回リリースで確認する
- 本設計は v1 スペックの「署名・公証・配布はスコープ外」を上書きする追補。デスクトップアプリ v1 スペック §15 の公証スパイク項目は初回リリース実測で消し込む

## 8. スコープ外

- CI での自動リリース(ローカル実行のみ)
- SBOM・provenance・第三者 NOTICE(solo-eikaiwa 相当の重装備は必要になったら)
- Intel(x86_64)・universal ビルド(Apple Silicon のみ)
- Windows / Linux 版の updater(macOS のみ配布のため対象外)
