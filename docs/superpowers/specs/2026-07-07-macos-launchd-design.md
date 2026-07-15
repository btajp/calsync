# calsync macOS ネイティブ常駐(launchd)設計書

作成日: 2026-07-07
ステータス: 承認済みドラフト(実装計画の入力)
関連: [2026-07-03-calsync-design.md](2026-07-03-calsync-design.md)(v1 全体設計)

## 1. 概要・動機

macOS では Docker Desktop 経由の常駐をやめ、**calsync バイナリを launchd の LaunchAgent として直接常駐**できるようにする。Linux では従来どおり Docker / compose 運用を維持する(本機能で Docker サポートは一切変更しない)。

動機(2026-07-06〜07 実障害): Docker Desktop の自動アップデートで仮想ネットワークが壊れ、calsync が 16 時間無言停止した。macOS の Docker Desktop は「VM+GUI アプリ+自動更新」という不安定要素の積み重ねであり、純 Go・CGO なし・単一静的バイナリの calsync にとってコンテナの利点はほぼない。ネイティブ化で以下が根本解消する:

- Docker Desktop / VM 境界起因の停止(今回の障害クラス)
- VirtioFS 境界の SQLite 破損リスク(CLAUDE.md 既知の落とし穴 — ネイティブでは flock / WAL が同一ホスト内で正しく機能する)
- スリープ復帰: launchd 常駐ならプロセスは起床後の最初の tick で継続し、当日分のダイジェストは遅延送信される(nextAt 方式の `now >= next` 判定は wall clock のため。日付を跨いだ分は既存仕様どおり放棄)

**Go コードの変更はゼロ**。追加するのはインストールスクリプト・plist テンプレート・ドキュメントのみ。

## 2. 成果物

| ファイル | 内容 |
| --- | --- |
| `scripts/macos/com.btajp.calsync.plist.template` | LaunchAgent テンプレート(プレースホルダ置換方式) |
| `scripts/macos/install-launchd.sh` | ビルド → 配置 → plist 生成 → `launchctl bootstrap` まで行う冪等スクリプト |
| `scripts/macos/uninstall-launchd.sh` | `launchctl bootout` + plist 削除(バイナリ・data/ は残す) |
| README / CLAUDE.md / calsync-setup スキル / CHANGELOG | 運用手順と注意の更新 |

## 3. 設計

### 3.1 LaunchAgent(plist)

- ラベル: `com.btajp.calsync`。配置先: `~/Library/LaunchAgents/com.btajp.calsync.plist`(**chmod 600** — トークンを含むため)
- `ProgramArguments`: `<BIN>/calsync run --config <DATA>/calsync.yaml --data <DATA>`(すべて絶対パス)
- `RunAtLoad: true` + `KeepAlive: true` — ログイン時自動起動+クラッシュ自動再起動(`restart: unless-stopped` 相当)
- `EnvironmentVariables`: `SLACK_BOT_TOKEN`(または `bot_token_env` で指定した名前)を**plist に直接埋め込む**。launchd の LaunchAgent はシェルプロファイルを読まないため env はここで渡すしかない。ファイルは 600 で保護し、インストールスクリプトが現在のシェル環境変数から値を転記する
- `TZ` は指定しない(システムのタイムゾーンを継承 — `reconcile_at` / `morning_digest` はシステム TZ で解釈される。compose の `TZ: Asia/Tokyo` と等価になることを README に明記)
- ログ: `StandardOutPath` / `StandardErrorPath` → `~/Library/Logs/calsync.log`(単一ファイルに集約)
- `ThrottleInterval: 10`(クラッシュループ時の再起動間隔)

### 3.2 install-launchd.sh(冪等)

1. 前提チェック: macOS であること / リポジトリルートで実行 / `bot_token_env` の環境変数が現在のシェルに存在すること(なければ名前を表示してエラー)
2. `go build -o ~/.local/bin/calsync ./cmd/calsync`(配置先は `CALSYNC_BIN` で上書き可。リポジトリ内の作業ツリーを指さない — checkout に影響されない安定パスに置く)
3. data ディレクトリ(既定 `<repo>/data`、`CALSYNC_DATA` で上書き可)の存在確認
4. **稼働中の Docker コンテナがあれば警告して中断**(`docker compose ps` が calsync を返す場合。二重起動は flock で防がれるが、明示的に「先に `docker compose down` せよ」と案内する — 移行事故防止)
5. テンプレートからプレースホルダ(`__BIN__` / `__DATA__` / `__TOKEN_ENV_NAME__` / `__TOKEN_VALUE__` / `__LOG__`)を置換して plist 生成・600 で配置
6. `launchctl bootout gui/$UID/com.btajp.calsync 2>/dev/null || true` → `launchctl bootstrap gui/$UID <plist>` → `launchctl kickstart gui/$UID/com.btajp.calsync`
7. 起動確認(`launchctl print gui/$UID/com.btajp.calsync` の state と、ログ末尾のエラー有無)を表示して終了

更新フロー(README に明記): `git pull` → `install-launchd.sh` 再実行(再ビルド+plist 再生成+再起動まで冪等にやり直す)。

### 3.3 運用上の注意(ドキュメントに明記)

- **移行**: `docker compose down` → `install-launchd.sh`。data/ は同一ファイルをそのまま使う(単一プロセスの flock 前提は維持される)
- CLAUDE.md の「コンテナ稼働中はホストから SQLite に触るな」は**コンテナ運用時の注意**である旨に更新(ネイティブ運用では同一ホストの `status` / `doctor`(OpenReadOnly)は安全 — 従来どおり。書き込み系はデーモン停止中のみ、は共通)
- スリープ: Mac がスリープ中は同期・通知とも停止し、起床後の最初の tick で再開する。`morning_digest` 時刻にスリープしていた場合、**同日中に起床すれば遅延送信される**(日付を跨いだら既存仕様どおり放棄)
- トークンを変更した場合は `install-launchd.sh` を再実行(plist に転記し直す)
- アンインストール: `uninstall-launchd.sh`(データは消さない)

## 4. テスト計画

Go コード変更ゼロのため単体テストは不要。検証は実機で行う:

1. `bash -n` による両スクリプトの構文チェック+`shellcheck`(あれば)
2. 実機で install → `launchctl print` で running 確認 → `calsync status`(OpenReadOnly)で全アカウント ok → Slack テスト発火(ダイジェスト一時時刻)
3. `kill` でプロセスを落とし、KeepAlive による自動再起動を確認
4. uninstall → プロセス消滅と plist 削除を確認 → 再 install(冪等性)

## 5. スコープ外

- Linux の systemd unit(需要が出たら同型で追加可能)
- Apple Container 対応(VM 境界が残るため見送り — 検討記録として本スペックに残す)
- Docker サポートの変更(一切なし。Linux とリモート運用の標準は compose のまま)
