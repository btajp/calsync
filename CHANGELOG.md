# Changelog

このプロジェクトの特筆すべき変更はすべてこのファイルに記録します。

フォーマットは [Keep a Changelog](https://keepachangelog.com/ja/1.1.0/) に、バージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従います。

## [Unreleased]

### Added

- **Slack 通知の Block Kit 化(v2)**: ダイジェストを予定ごとのブロック表示にし、件名をカレンダーの当該予定へのリンクに、Zoom / Meet / Teams の会議 URL を「参加」ボタンにした(conferenceData / onlineMeeting → location・本文の URL 検出の順で抽出)。開始前リマインドに会議参加ボタンと本文全文(プレーンテキスト化・3,000 字制限内に切り詰め)を追加。通知プレビューには従来のテキスト形式を fallback として維持し、blocks が不正な場合はテキストのみで 1 回縮退再送する

- **Slack 通知(#10)**: 朝のダイジェスト(指定時刻に当日の実予定を全アカウント横断で通知。ライブ取得のため free の予定も件名付きで含む)と開始前リマインド(指定時間前に通知。イベントキャッシュ+送信記録テーブルで再起動しても二重送信しない)。`notifications.slack`(`bot_token_env` / `channel` / `morning_digest` / `remind_before`)で設定し、トークンは環境変数のみ。件名は Slack 仕様のエスケープ済み(メンションインジェクション防止)

- **ブロッカーの元アカウント表示(per-account オプトイン、#7)**: `accounts[].show_origin_in_description: true` で、そのアカウントのカレンダーに作られるブロッカーの説明欄に元アカウントの ID を記載(Google: description / Microsoft: body)。既定は従来どおり完全匿名。変更検出ハッシュにポリシー成分を合成しているため、トグルの ON/OFF は次回リコンサイルで既存ブロッカーにも遡及反映される

- **相互 Busy ブロッカー同期エンジン**: 複数の Google カレンダー / Microsoft 365(個人 Microsoft アカウント含む)を差分ポーリング(Google: syncToken / Graph: calendarView delta)で監視し、Busy 予定を他の全アカウントへ「予定あり」ブロッカーとしてミラーする Hub & Spoke 構成
- **無限ループ防止**: ローカル mappings テーブルによる一次判定+イベント拡張プロパティタグ(`calsync-origin`)による二次判定・自己修復
- **冪等なブロッカー作成**: Google はクライアント生成イベント ID、Microsoft は `transactionId` による二重作成防止(クラッシュ・再送に安全)
- **同一会議の重複抑止**: 自分の複数アカウントが同じ会議に招待されている場合、iCalUID 照合でブロッカーを抑止(`dedupe_same_meeting`、既定オン)。実予定が消えた場合は自動昇格
- **リコンサイル**(日次+`calsync reconcile`): 同期ウィンドウのスライド、set-difference による差分照合、孤児ブロッカーの収容・掃除、手動削除されたブロッカーの復元、pending 状態の解決、DB 全損時のタグからの再構築
- **カーソル失効の自己回復**: Google 410 / Graph 410・`syncStateNotFound` を検出してフル再同期(mappings は破壊しない)
- **アカウント単位の障害分離**: トークン失効したアカウントだけ `reauth_required` で停止し他は継続。再認証後はリコンサイルで自動バックフィル
- **OAuth**: 認可コード+PKCE+ループバック(Google / Microsoft)、Device Code フロー(Microsoft、`--device-code`)、refresh token ローテーション追従の永続化
- **CLI**: `run` / `sync --once` / `reconcile` / `status` / `doctor` / `auth add`・`auth list` / `accounts remove --force`
- **状態管理**: SQLite 1 ファイル(WAL、flock による多重起動防止、`status`/`doctor` 用の読み取り専用オープン)
- **配布**: multi-stage Dockerfile(distroless、CGO 無効)、docker-compose 例、セットアップ要件を網羅した README(GCP「In production」必須手順、Entra アプリ登録、Docker での認証手順、プライバシー注記)

### Added

- **AGENTS.md**(AI エージェント向けプロジェクトガイド。`CLAUDE.md` はシンボリックリンク)と **`.agents/skills/calsync-setup`** スキル(セットアップ・アカウント追加の対話支援。組織/個人アカウントの分岐、ブラウザプロファイルを跨ぐ認可の注意、認可後のトークン実体検証を実測知見として収録。`.claude/skills` はシンボリックリンク)

### Fixed

- **DB 全損からの再構築時にループ防止が機能しない重大バグを修正**。Reconcile が mappings 再構築(タグからの復元)より先に配布を実行する順序だったため、Graph delta がタグを返せない制約下で Microsoft カレンダー上の受領ブロッカーが実予定と誤認され全カレンダーへ再ミラーされた(実障害: 複製957件)。対策として (1) フェーズ0「タグからの mappings 先行再構築」を配布より前に追加 (2) set-difference の alive 判定にもループ遮断を適用 (3) origin 消滅 active mapping の掃除フェーズを追加。修正後のリコンサイル1回で複製957件が全自動除去され、正常状態(527予定・2,635 mappings)への復元を実機確認
- **同一カレンダーの5回連続同期失敗でカーソルを FullResync により再初期化するフォールバックを追加**。実運用で Graph が壊れた deltaLink に対し文書化された失効シグナル(410 / syncStateNotFound)ではなく持続的な 504 を返し続け、同じカーソルを無限リトライする事象が発生したため(手動復旧はコンテナ停止+cursor クリアで可能だが、以後は自動回復する)

- **認可 URL に `prompt=select_account` を強制付与**。既定ブラウザに同意済みセッションが残っていると認可 UI なしで別アカウントのトークンが発行・保存される事故が実際に発生したため(誤トークンで作られたブロッカーは `accounts remove` で復旧可能なことも確認)、常にアカウント選択画面を挟む
- **Microsoft ループバック認証の redirect_uri を「`localhost`・パスなし」形式に修正**。個人 Microsoft アカウント(login.live.com)はアプリ登録 `http://localhost` に対してポートは無視するがパスは照合するため、従来の `127.0.0.1:<port>/callback` 形式では認可が `invalid_request` で拒否されていた(実測 2026-07-03。MSAL と同じ形式に統一)

### Changed

- README: Google の「6 ヶ月」系制限は常時稼働していれば定期作業不要であることを明記。Google Workspace 組織で GCP が無効な場合の構成(個人アカウントの GCP プロジェクトでクライアント共用)と第三者アプリアクセス制御の注意、Entra 新 UI(Authentication (Preview) → 設定タブ)での設定位置を追記

### 実機検証の状況(2026-07-03)

- 実 Google / Microsoft API で確認済み: OAuth 認可(両プロバイダ・未検証アプリのクリックスルー含む)、最小スコープでの疎通(Google `calendar.events` のみ / MSA の `MailboxSettings.Read`)、双方向の差分同期(syncToken / calendarView delta)、双方向のブロッカー作成(6件+1件)、transparent な終日予定の除外
- 2026-07-04 追記: Graph 終日イベントの取り込み(日付ズレなし)、手動削除ブロッカーのリコンサイル復元と Google 409 蘇生(繰り返しインスタンス由来含む)、DB 全損からのタグ再構築を実機確認 — 主要スパイクは全消化(残りは設計書 15 章参照、低リスクの参考確認のみ)
- 2026-07-04 追記: `docker build`(33.5MB イメージ)と `docker compose up -d` での常駐稼働を実機確認。6アカウント(Google×4 + Microsoft 個人 + Gmail 個人)・元予定 526 件・ブロッカー 2,630 件のバックフィルと毎分ポーリングの継続稼働、Graph 一時 504 のアカウント分離も実運用で確認

### 既知の制約(v1)

- Microsoft アカウントはプライマリカレンダーのみ監視・書き込み可
- 過去方向の同期はしない。ブロッカーのマージはしない(元予定 1 : ブロッカー 1)
- 単一インスタンス前提(同一データディレクトリでの多重起動は flock で拒否)。**コンテナ稼働中はホストから SQLite に読み取りアクセスもしないこと**(WAL/flock は VM 境界を跨いで機能せず、破損実績あり。状態確認は docker exec 経由の status を使う)
- 実 API での検証状況は上記「実機検証の状況」を参照(主要経路は確認済み・一部未実測)
