# Changelog

このプロジェクトの特筆すべき変更はすべてこのファイルに記録します。

フォーマットは [Keep a Changelog](https://keepachangelog.com/ja/1.1.0/) に、バージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従います。

## [Unreleased]

### Fixed

- **appserver: コンテナモードのガードを書き込み/認可系エンドポイントにも適用**: 仕様(§9)は「コンテナ運用のホストからは DB 読み取りを含む全機能を停止して案内表示モードに落とす」と定めていたが、実装は DB 読み取り(status)・doctor・デーモン操作のみをガードしており、`PUT /api/config`・`POST /api/auth/start`・`GET /api/accounts/{id}/calendars` はコンテナモードでも動作していた(ホストからのトークンファイル書き込みがコンテナ内デーモンの Microsoft refresh token ローテーションと競合しうる)。この 3 エンドポイントにコンテナモード検出時 409 `container_mode` を返すガードを追加。manual/launchd モードの挙動は変更なし。デスクトップアプリ側も、起動直後の状態確認でコンテナモードを検出したらタブ UI の代わりに案内画面を表示するよう変更
- **appserver: `GET /api/status` の `tokens`(アカウント 0 件時)・`GET /api/accounts/{id}/calendars` の `calendars`(0 件時)が JSON `null` を返しデスクトップアプリがクラッシュする不具合を修正**: Go の nil スライスがそのまま JSON エンコードされていたため。両エンドポイントでレスポンスを空配列に初期化し、フロント側(`Dashboard.tsx` の `buildOverview`・`AccountAdd.tsx` のカレンダー取得)にも `?? []` の防御を追加
- **appserver: launchd の plist はあるが `launchctl print` が未ロードを返す状態(stale plist)で、docker 版 calsync コンテナが稼働していても container モードを検出できていなかった不具合を修正**: デーモン検出順序を変更し、`launchctl print` 失敗時も docker のコンテナ検出を優先して試すようにした(ホストが誤ってコンテナ運用中の DB に読み取りアクセスしてしまうのを防止)
- **appserver: HTTP リクエストの `Host` ヘッダを検証していなかった不具合を修正**: `requireToken` に Host 検証(`127.0.0.1:` / `localhost:` で始まらないリクエストは 403)を追加し、DNS rebinding 経由でのトークン窃取・API 呼び出しを防止

### Changed

- `digest_calendars` と `blocker_calendar` の重複を設定エラーに(受領ブロッカーの置き場を通知専用カレンダーと兼ねる構成は「digest 専用にはブロッカーが無い」前提を崩すため拒否)
- **docker-compose.yaml の Slack トークン受け渡しキーを `SLACK_BOT_TOKEN`(`bot_token_env` の既定値)に統一**。旧キー `SLACK_TOKEN_CALSYNC` を `.env` / `bot_token_env` で使っていた compose 利用者は、キー名を揃え直すか `bot_token_env` を明示すること(不一致だとトークンが空になり起動を拒否する)
- README とセットアップスキル(calsync-setup)を最新化: macOS launchd ネイティブ常駐を推奨として冒頭・図・手順に反映、Slack ダイジェストの説明を v2.2 実装(色付き attachment グルーピング・上限 20 件・unfurl 抑止)に更新、コンテナ稼働中のホストからの SQLite アクセス禁止を明記、設定リファレンスに全キーへの参照を追加

### Added

- **デスクトップアプリの署名済みリリースパイプライン**: `scripts/release-desktop.sh` で dmg を Developer ID Application 署名 + Notary Service 公証付きでビルドし、GitHub Releases(タグ `desktop-v<version>`)へ公開できるように。リリース専用の bundle オーバーレイ(`desktop/src-tauri/tauri.release.conf.json`)を追加し、手順は [デスクトップアプリ 署名・公証・GitHub 公開ランブック](docs/desktop-release-runbook.md) に記載。self-updater・CI 自動リリース・SBOM/provenance は対象外(`git pull` + 再ビルドが正)
- **macOS デスクトップアプリ(Tauri v2、`desktop/`)と `calsync appserver` サブコマンド**: ターミナルを使わずにダッシュボード(デーモン状態・構成俯瞰・doctor 実行・launchd の停止/起動/再起動)、設定(`data/calsync.yaml`)のフォーム編集(コメント保持保存・mtime 競合検出・再起動誘導)、アカウント追加ウィザード(前提チェック→OAuth→カレンダー選択→YAML 追記→再起動誘導)を行える GUI アプリを追加。デーモン本体は `calsync appserver` サブコマンド(127.0.0.1 限定・起動ごとのワンタイム Bearer トークン・stdout 1 行 JSON ハンドシェイク・stdin EOF で自動終了)をローカル API として提供し、Tauri の Rust 殻がサイドカーとして起動する。launchd 管理外で docker の calsync コンテナ稼働を検出した場合は DB 読み取りを含む全機能を停止する案内表示モードに落ちる(コンテナ運用のホストからの SQLite アクセス禁止という既存不変条件を維持)。アプリはアカウント削除機能を持たず `calsync-uninstall` へ誘導する。デーモン本体のビルド(CGO なし・Go のみ)には影響しない。ビルドには Rust + Node.js が別途必要(`cd desktop && npm install && npm run build-sidecar && npm run tauri dev` / `npm run tauri build`)
- **Google OAuth スコープに `calendar.calendarlist.readonly` を追加**: デスクトップアプリのアカウント追加ウィザードでのカレンダー選択 UI に使用。新規認可時のみ自動付与され、既存トークンでは再認可するまでカレンダー一覧が取得できない(内部的には Google API の 403。アプリ画面には appserver 経由の 502 エラーとして表示され、カレンダー ID の手入力フォームにフォールバックする)
- **アンインストール手順とアンインストール支援スキル**: README にアンインストール節(ブロッカー掃除 → 常駐解除 → データ削除 → アクセス権取り消し → クラウド側登録の削除、の安全な順序)を追加。エージェント向けに `.agents/skills/calsync-uninstall`(完全撤去・一部アカウント削除・ブロッカーのみ掃除の分岐と落とし穴を対話的にガイド)を追加
- **detail_sync のペア別 visibility**: `detail_sync[].visibility`(`private`(既定)/ `default` / `public`)でペアのブロッカーの公開設定を制御可能に(Google: visibility / Microsoft: sensitivity へ写像、default・public はどちらも normal)。未指定は従来どおり非公開で、既存設定への影響なし(無風)。変更は次回リコンサイルで既存ブロッカーにも遡及
- **ペア別タイトル/説明同期(`detail_sync`)**: トップレベル `detail_sync` で指定した origin => target アカウントペアに限り、ブロッカーのタイトル/説明を元イベントから転記(`fields: [title, description]` で選択)。既定は従来どおり完全匿名で、未設定なら保存ハッシュも従来と完全同一(アップグレード無風)。内容をハッシュに合成しているため元イベントの変更は次のポーリングで追従し、設定変更は次回リコンサイルで既存分にも遡及。併せて (1) Google の patch にタイトルを追加、(2) 両プロバイダの 409 復旧時に内容整合 patch を追加、(3) リコンサイル収容・再構築行は sentinel により 1 回だけ自己修復 patch されるように(ペア解除後に DB 再構築を挟んでも転記内容が残留しない)
- **macOS ネイティブ常駐(launchd)**: Docker Desktop の VM/自動更新起因の停止を避けるため、calsync バイナリを `launchd` の LaunchAgent として直接常駐できるように(`scripts/macos/install-launchd.sh` / `uninstall-launchd.sh` / plist テンプレート)。Go コードの変更なし。Linux / Docker 運用は変更なし
- **通知専用カレンダー(`digest_calendars`)**: ブロッカー配布せずダイジェストにだけ載せるカレンダーを指定可能に(google のみ・リマインド対象外)
- **Slack 通知の Block Kit 化(v2)**: ダイジェストを予定ごとのブロック表示にし、件名をカレンダーの当該予定へのリンクに、Zoom / Meet / Teams の会議 URL を「参加」ボタンにした(conferenceData / onlineMeeting → location・本文の URL 検出の順で抽出)。開始前リマインドに会議参加ボタンと本文全文(プレーンテキスト化・3,000 字制限内に切り詰め)を追加。通知プレビューには従来のテキスト形式を fallback として維持し、blocks が不正な場合はテキストのみで 1 回縮退再送する
- **Slack 通知 v2.1(実表示フィードバック反映)**: ダイジェストの予定行を予定ごとの色付き attachment(`accounts` の定義順で固定パレットを巡回割当、未知アカウントは灰色)に変更し、表示上限を 46 件から 20 件に短縮(超過は「他 N 件」)。リマインドも単一 attachment 化。`chat.postMessage` に `unfurl_links` / `unfurl_media` を常に付与し htmlLink・会議 URL のプレビュー展開を抑止。縮退再送のトリガーに `invalid_attachments` を追加
- **Slack 通知 v2.2(実表示フィードバック反映)**: ダイジェストの attachment を「予定ごとに 1 つ」から「時系列順で連続する同一色(先頭アカウント)の予定を 1 つにまとめる」run-length グルーピングに変更。Slack クライアントが attachment 数超過時に「+ N more attachments」へ自動折りたたむのを避けるための対応。表示上限は「予定(section)20 件」で変更なし(20 件目がグループ途中でも打ち切る)。section 単位の参加ボタン・色分け・時系列順は維持

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

- **Slack リマインドの二重表示を修正**。リマインドはトップレベル blocks を持たず attachments のみで構成されるため、Slack がトップレベル `text`(fallback)を本文としても描画し attachment と重複表示していた(実 Slack で確認済み。blocks を持つダイジェストでは発生しない)。`post()` はトップレベル `text` を `blocks` があるとき(ダイジェスト)だけ送るよう変更し、リマインドの通知用テキストは `attachment.fallback` フィールドに設定するようにした
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
