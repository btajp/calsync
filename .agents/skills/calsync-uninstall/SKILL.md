---
name: calsync-uninstall
description: calsync の撤去(完全アンインストール・一部アカウントの削除・ブロッカーの掃除)を対話的に支援する。順序を誤るとカレンダーにブロッカーが残る/手動削除しかなくなるため、状況を聞いてから安全な順序でガイドする。トリガー: アンインストールしたい / やめたい / ブロッカーを消したい / アカウントを外したい / カレンダーに「予定あり」が残っている
---

# calsync アンインストール支援

**鉄則: トークン(`./data`)を消すのは最後。** リモートのブロッカー削除には各アカウントの認証が必要で、先にトークンを消すと各カレンダーの手動削除しか手段がなくなる。手順の全体像は README の[アンインストール](../../../README.md#アンインストール)が正 — このスキルは分岐判断と検証を支援する。

## Step 0: 状況の把握(最初に聞く)

1. **何をしたいか**: (a) 完全撤去 / (b) 一部アカウントだけ外す / (c) ブロッカーだけ一旦消したい(運用は続ける)
2. **運用形態**: macOS ネイティブ(launchd)か Docker か(`launchctl print gui/$(id -u)/com.btajp.calsync` が state を返せば launchd、`docker compose ps` に calsync がいれば Docker)
3. **全アカウントの認証が生きているか**: `./calsync status --data ./data` で全行 `ok` か(`reauth_required` があるとそのアカウントのリモート掃除ができない → 先に `auth add` で復活させるのが最短)

分岐:
- (b) 一部アカウントのみ → README の[アカウントの削除](../../../README.md#アカウントの削除)の手順で完結(**`accounts remove` → calsync.yaml から削除、の順序を絶対に守る**)。以下は不要
- (c) ブロッカーだけ消す → Step 1〜2 のみ実施し、`data/` とアプリ登録は残す(再開するときは calsync.yaml を戻して再起動 → 次の同期で再配布される)
- (a) 完全撤去 → Step 1〜6 をすべて実施

## Step 1: デーモン停止

```bash
launchctl bootout gui/$(id -u)/com.btajp.calsync   # launchd
docker compose down                                 # Docker
```

検証: launchd は `launchctl print gui/$(id -u)/com.btajp.calsync` がエラーになること、Docker は `docker compose ps` に calsync がいないこと。**書き込み系コマンド(`accounts remove` / `reconcile`)はデーモン停止中しか動かない**(flock)。コンテナ運用では、稼働中にホストから `./data` の SQLite に触るのも禁止(読み取りだけで破損実績あり)。

## Step 2: 全カレンダーからブロッカーを掃除

アカウントを 1 つずつ `accounts remove`(この時点では calsync.yaml のエントリを消さない — 消すと provider が組めず `--force` 必須になり、そのアカウントの受領ブロッカーがリモートに残る):

```bash
./calsync accounts remove <id> --config ./data/calsync.yaml --data ./data
```

- 全アカウントに対して繰り返すと、配布済み・受領済みブロッカーが全カレンダーから消える
- `--force` は「消せないものをスキップしてローカル状態だけ消す」— そのカレンダーのブロッカーは**手動削除が必要**になる。認証切れが理由なら、`--force` の前に `auth add <id>` での復活を必ず提案する
- `digest_calendars`(通知専用カレンダー)は監視対象外でブロッカーの配布先にもならないため掃除対象外(`blocker_calendar` との重複は設定検証が拒否する)。いずれにせよ下の目視確認は全カレンダーに対して行うこと

検証: 各アカウントのカレンダーを目視し、ブロッカー(既定タイトル「予定あり」。`detail_sync` ペアはタイトル転記されている場合あり)が残っていないこと。

## Step 3: 常駐の解除

```bash
./scripts/macos/uninstall-launchd.sh   # launchd: bootout + plist 削除(冪等。バイナリ・data/ は残す)
docker compose down --rmi local        # Docker: イメージも消す場合
```

## Step 4: ローカルデータとバイナリの削除

```bash
rm -rf ./data                     # トークン・SQLite・設定。復元不能 — Step 2 完了後にのみ実行
rm -f ~/.local/bin/calsync        # launchd 運用のバイナリ
rm -rf desktop/src-tauri/target   # デスクトップアプリ(desktop/)を使っていた場合のビルド成果物(サイドカーの calsync バイナリ含む)
```

デスクトップアプリ(`desktop/`)から `npm run tauri build` で `.app` を生成し `/Applications` 等へコピーしていた場合はそれも削除する。**アプリ自体にアンインストール操作はない**(アカウント削除・ブロッカー掃除は本スキルの Step 0 の分岐に従うこと)。

リポジトリの clone 自体も不要なら削除してよい。

## Step 5: アカウント側のアクセス権取り消し

- Google: https://myaccount.google.com/connections で calsync の OAuth クライアントを削除
- Microsoft(個人): https://account.live.com/consent/Manage でアクセス許可を削除
- Microsoft(組織): https://myapps.microsoft.com から取り消し(組織設定によっては管理者作業)

## Step 6: クラウド側の登録削除(自分で作成した場合のみ)

- GCP: OAuth クライアント(calsync 専用プロジェクトならプロジェクトごと)
- Entra ID: アプリの登録
- Slack: 通知を使っていた場合はワークスペースからアプリを削除

## 落とし穴早見

| 症状 / 誘惑 | 正しい対応 |
| --- | --- |
| 先に `./data` を消してしまった | リモートのブロッカーは API では消せない。各カレンダーでタイトル(既定「予定あり」)を検索して手動削除 |
| calsync.yaml から先にアカウントを消した | `accounts remove` が provider を組めない。エントリを一旦戻してから remove する |
| デーモン稼働中に `accounts remove` が失敗する | flock による正常な排他。Step 1 の停止を先に |
| `reauth_required` のアカウントがある | `--force` の前に `auth add <id>` で復活させてから remove(リモートも消える) |
| コンテナ稼働中にホストから sqlite3 で覗きたい | 禁止(読み取りだけで DB 破損実績)。必ず停止してから |
