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
| 成果物 | dmg・checksums.txt |
| 更新 | なし(self-updater は本設計のスコープ外。`git pull` + 再ビルドが正) |

## 2. 流用する資産(新規作成ゼロ)

署名・公証の資格情報はアプリ単位ではなくチーム/マシン単位のため、solo-eikaiwa で構築済みのものをそのまま使う:

- Developer ID Application 証明書(キーチェーン。`security find-identity -v -p codesigning` で確認)
- 公証用 App Store Connect API キー(Key ID / Issuer ID / `~/.appstoreconnect/private_keys/AuthKey_<KEY_ID>.p8`)

Store 用 App ID・provisioning・updater 鍵は使わない。bundle identifier は既存の `com.btajp.calsync.desktop` のままで公証できる。

## 3. 成果物と変更点

1. **`desktop/src-tauri/tauri.release.conf.json`(新規)**: リリース専用オーバーレイ。`bundle.targets` を `["app", "dmg"]` にする。既定の `tauri.conf.json` は `["app"]` のまま(開発ビルドを遅くしない)
2. **`scripts/release-desktop.sh`(新規)**: リリース一括スクリプト(§4)
3. **`docs/desktop-release-runbook.md`(新規)**: 初回準備(証明書・API キーの確認)と公開手順・事後確認のランブック。README からリンク
4. CHANGELOG `[Unreleased]` 追記

## 4. release.env とスクリプトの仕様

シークレットはリポジトリ外の `~/.config/calsync/release.env`(chmod 600)。初回実行でテンプレートを生成して終了する。

```bash
APPLE_SIGNING_IDENTITY="Developer ID Application: YOUR ORG (TEAMID)"
APPLE_API_KEY="ABC123DEFG"
APPLE_API_ISSUER="00000000-0000-0000-0000-000000000000"
APPLE_API_KEY_PATH="$HOME/.appstoreconnect/private_keys/AuthKey_ABC123DEFG.p8"
```

`scripts/release-desktop.sh <version>` のステップ(失敗したら即中断):

1. release.env 読込+必須変数と `.p8` の存在確認。Apple ID 方式の env(`APPLE_ID` 等)は unset して API キー方式に固定(tauri-cli は Apple ID 方式を優先評価するため)
2. preflight: `gh`・`cargo`・`npm`・`xcrun notarytool` の存在確認
3. Git 前提: push 済み・クリーンな `main` からのみ実行可(タグ・Source アーカイブと配布物の一致を保証)。タグ `desktop-v<version>` が未使用であること
4. バージョン整合: `desktop/package.json`・`desktop/src-tauri/tauri.conf.json`・`desktop/src-tauri/Cargo.toml` の version と CHANGELOG の節が一致
5. `cd desktop && npm ci && npm run build-sidecar`
6. `CI=true npm run tauri build -- --config src-tauri/tauri.release.conf.json`(`CI=true` は create-dmg の Finder AppleScript 依存を外すため。署名・公証・staple は bundler が env から自動実行)
7. 生成物検証: `.app` と dmg の存在、`codesign --verify --deep --strict`、`xcrun stapler validate`(.app)、`spctl -a -t exec -vv`。**サイドカー(`Contents/MacOS/` 配下の calsync バイナリ)の署名も個別に `codesign --verify` する**(§6 リスク参照)
8. dmg 自体の公証+staple(`xcrun notarytool submit --wait` → `xcrun stapler staple`。Tauri が staple するのは .app のみ)
9. checksums.txt 生成(shasum -a 256)
10. GitHub Release: `desktop-v<version>` タグで draft 作成 → 全アセット添付後に publish(原子的公開)。ノートは CHANGELOG の該当節を抽出

デーモン本体のリリース(タグ `v*`)とは独立させるため、タグは `desktop-v*` プレフィックスを使う。

## 5. 事後確認(ランブックに記載)

```bash
codesign --verify --deep --strict --verbose=2 <app>
spctl -a -t exec -vv <app>
xcrun stapler validate <app> && xcrun stapler validate <dmg>
```

ブラウザで dmg を実ダウンロード → マウント → /Applications へコピー → 警告なしで起動、まで確認する。

## 6. リスクと実測項目

- **tauri#11992(externalBin 同梱時の公証失敗報告)**: calsync の Go サイドカーは `externalBin` 宣言なので bundler の署名対象に入る想定だが、初回リリースで実測する。失敗した場合の回避策(solo-eikaiwa の whisper-bin と同じプレ署名: ビルド前に `codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" desktop/src-tauri/binaries/calsync-*`)をランブックに記載しておく
- 公証には Apple Developer Program($99/年)の有効な membership が必要(solo-eikaiwa で契約済みの前提)
- 本設計は v1 スペックの「署名・公証・配布はスコープ外」を上書きする追補。デスクトップアプリ v1 スペック §15 の公証スパイク項目は初回リリース実測で消し込む

## 7. スコープ外

- self-updater・latest.json(更新は git pull + 再ビルド)
- CI での自動リリース(ローカル実行のみ)
- SBOM・provenance・第三者 NOTICE(solo-eikaiwa 相当の重装備は必要になったら)
- Intel(x86_64)・universal ビルド(Apple Silicon のみ)
