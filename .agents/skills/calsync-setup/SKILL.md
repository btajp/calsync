---
name: calsync-setup
description: calsync の初期セットアップとアカウント追加を対話的に支援する。GCP / Entra アプリ登録の場所選び(個人か組織か)、Google Workspace / Microsoft 365 組織アカウント固有の注意、ブラウザプロファイルを跨ぐ認可の落とし穴、認可後の検証までを実測済みの知見でガイドする。トリガー: セットアップしたい / アカウントを追加したい / 認可がうまくいかない / GCP・Entra の設定
---

# calsync セットアップ支援

ユーザーの環境・アカウント構成は毎回違う。**先に状況を聞き、分岐してからガイドする**こと。手順を一括で貼らない。1ステップ進むごとに検証コマンドで確認してから次へ進む。

## Step 0: 状況の把握(最初に聞く)

1. 追加したいアカウントの一覧(プロバイダ / メールアドレス / **組織用か個人用か**)
2. Google を使う場合: GCP プロジェクトをどのアカウントで作るか決まっているか
3. Microsoft を使う場合: Entra テナントを持っているか(会社のものか、自分のものか)
4. ブラウザのプロファイル運用(アカウントごとにプロファイルを分けているか)

**推奨を明確に伝える**: GCP プロジェクト / Entra アプリ登録は**個人アカウント側に作る**。会社のテナント・プロジェクトに作ると退職や権限変更でアプリごと失われる。登録場所とサインインできるアカウントは独立なので、個人側に作っても組織アカウントを認可できる。

## Step 1: GCP セットアップ(Google を使う場合)

README「セットアップ 1」の手順に従う。エージェントとして特に強調すべき点:

- OAuth 同意画面は External + **公開ステータス「In production」必須**。Testing のままだと refresh token が7日で失効し常時同期が成立しない(実測済み)
- クライアントは「**デスクトップ アプリ**」タイプ。作成直後に JSON をダウンロード(再表示不可)し、`data/google-client.json` に配置、権限 600
- スコープ登録画面は空のままでよい(実行時に要求される)
- 検証: JSON のトップレベルキーが `installed` であること(`web` だと誤タイプ)

## Step 2: Entra セットアップ(Microsoft を使う場合)

README「セットアップ 2」の手順に従う。強調点:

- テナントは無料でよい。個人 Microsoft アカウントに紐付くテナント(既定のディレクトリ)があればそれで OK。アプリ登録にサブスクリプション・課金は不要
- アカウントの種類は「**任意の組織ディレクトリ + 個人の Microsoft アカウント**」(これで組織 365 と個人 Outlook の両対応)
- リダイレクト URI は「モバイル アプリケーションとデスクトップ アプリケーション」プラットフォームに **`http://localhost`(パスなし・ポートなし)**
- 「パブリック クライアント フローを許可」= はい。**新 UI では Authentication (Preview) → 「設定」タブ**にある(旧 UI は Advanced settings)
- API のアクセス許可(委任): `Calendars.ReadWrite` + `MailboxSettings.Read`(管理者同意ボタンは個人利用では不要)
- **登録直後の認可で `invalid_request`(redirect_uri が不正)が出たら、設定ミスを疑う前に数分待って再試行**。MSA(login.live.com)側への伝播ラグが原因のことがある(実測済み)

## Step 3: 設定ファイルとアカウント追記

- `calsync.yaml` の `accounts` に `id`(短い英数字。**コロン禁止**)/ `provider` / `email` を追記
- Microsoft は v1 ではプライマリカレンダーのみ(calendars 指定は `primary` 固定)
- コンテナ運用の場合は `/data/calsync.yaml` にも同内容を置き、`credentials_file` は `/data/google-client.json`(コンテナ内絶対パス)にする

## Step 4: 認可(最重要・1アカウントずつ)

**ブラウザプロファイルの落とし穴**(実際に事故った):

- `calsync auth add <id>` は既定プロファイルでブラウザを自動オープンする。対象アカウントが別プロファイルにある場合、**自動で開いたタブは使わず閉じ、ターミナルに表示された URL を正しいプロファイルへコピペ**する
- 認可 URL には `prompt=select_account` が付いており必ずアカウント選択画面が出る。**ここで対象アカウントを選び間違えると、別アカウントのトークンがその id で保存される**。選択画面でメールアドレスを必ず確認させること
- 組織アカウント(GWS)で「**このアプリはブロックされます**」が出たら: その組織の管理コンソール → セキュリティ → API の制御 → アプリのアクセス制御 → 「アプリのアクセス権を管理」→「新しいアプリを設定」→ client ID で検索 → 「**信頼できる**」に設定(管理者権限がなければ管理者へ依頼)。個人アカウントなら「詳細 → 移動」でクリックスルー可能
- Microsoft 組織アカウントで `AADSTS65001` / `AADSTS90094` → 管理者同意が必要(README のトラブルシューティング参照)

**認可のたびに必ず検証する**(無言の誤認可を検出するため):

```bash
./calsync auth list   # トークンの有無
# トークンの実アカウント確認(Google。トークン値は表示しない)
TOKEN=$(python3 -c "import json;print(json.load(open('data/tokens/<id>.json'))['access_token'])")
curl -s -H "Authorization: Bearer $TOKEN" \
  'https://www.googleapis.com/calendar/v3/calendars/primary/events?maxResults=1&fields=summary' \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('summary'))"
# 出力がそのアカウントのメールアドレス(またはユーザーが付けた primary カレンダー名)と一致するか
```

誤ったアカウントで認可してしまった場合: `./calsync accounts remove <id>` で掃除(誤トークンのまま実行してよい — 誤配置ブロッカーも同じトークン経由で正しく消える)→ 正しいアカウントで `auth add` をやり直す。

## Step 5: バックフィルと稼働確認

1. 全アカウント認可後: `./calsync doctor`(全アカウント API check ok を確認)
2. `./calsync reconcile` — 既存予定の相互配布。**アカウント数 N、予定数 E に対して最大 E×(N-1) 件の書き込み**が走るため数分かかることを事前に伝える
3. `./calsync status` と SQLite での確認: `sqlite3 data/calsync.db "SELECT origin_account,target_account,status,COUNT(*) FROM mappings GROUP BY 1,2,3;"`
4. ユーザーに実カレンダーを目視確認してもらう(「予定あり」が期待どおり並ぶか・重複がないか)

## Step 6: 常駐化

常駐方式はユーザーの OS で分岐する。**先にどちらか確認すること**。

- **macOS**: `launchd` によるネイティブ常駐を推奨する(Docker Desktop の VM/自動更新起因の停止を避けられる)。`./scripts/macos/install-launchd.sh` を実行するだけ(冪等。ビルド → plist 生成 → 登録 → 起動まで自動)。トークンを変更したときも同じスクリプトを再実行する。アンインストールは `./scripts/macos/uninstall-launchd.sh`(バイナリ・data/ は残る)。詳細は README「macOS ネイティブ常駐(推奨)」節を参照
  - この場合、同一ホストの `status` / `doctor`(OpenReadOnly)はデーモン稼働中でも安全(VirtioFS 境界が存在しないため)。書き込み系コマンド(`sync` / `reconcile` / `accounts remove`)はデーモン停止中のみ、という制約はコンテナ運用と共通
- **Linux / その他(標準)**: `docker compose up -d --build`。設定は `./data/calsync.yaml`、TZ は compose の `TZ` 環境変数(reconcile_at の解釈に使われる)
  - コンテナ稼働中の状態確認は `docker compose logs` と `docker exec calsync /calsync status --config /data/calsync.yaml --data /data`。**コンテナ稼働中はホストから data/ の SQLite に一切触れない(読み取りの sqlite3 も禁止 — WAL は VM 境界を跨げず DB 破損の実績あり)。書き込み系コマンドはコンテナ停止中のみ**
- アカウントを後から追加する場合: 認可(Step 4)はホストで行い、常駐プロセス(launchd または コンテナ)を再起動

## Step 7: Slack 通知のセットアップ(オプション)

朝のダイジェスト・開始前リマインドを使いたい場合のみ。`notifications.slack` を設定しなければ通知機能は完全に無効なので、不要ならこの節はスキップしてよい。

1. [api.slack.com/apps](https://api.slack.com/apps) → **Create New App** → **From scratch** で Slack App を作成する
2. **OAuth & Permissions** → **Bot Token Scopes** に追加: `chat:write`(必須)/ `im:write`(DM 宛てなら必須)/ `chat:write.public`(Bot 未参加の公開チャンネル宛てなら必須)
3. **Install to Workspace** を実行し、`xoxb-` から始まる Bot User OAuth Token を控える。**トークンは `calsync.yaml` に書かず、`.env`(コミット禁止)経由で `SLACK_BOT_TOKEN` 環境変数として渡す**(compose の `environment` に `SLACK_BOT_TOKEN: ${SLACK_BOT_TOKEN}` を追記)
4. `channel` の調べ方: チャンネル宛てはチャンネル詳細画面の一番下にある Channel ID(`C…`/`G…`)、DM 宛てはユーザーのプロフィール →「その他」→ **メンバー ID をコピー**(`U…`)
5. **プライベートチャンネル宛ての場合は、そのチャンネルで `/invite @<App名>` して Bot を招待する**(招待し忘れると送信時に `not_in_channel` エラーになる)
6. `calsync.yaml` に `notifications.slack`(`bot_token_env` / `channel` / `morning_digest` / `remind_before`。両方省略はエラー)を追記し、`calsync run` を起動する。トークン検証は起動時のみ行われる

## トラブルシューティング早見

| 症状 | 原因と対処 |
| --- | --- |
| Google: 7日ごとに再認証 | 同意画面が Testing → In production に publish |
| Google: このアプリはブロックされます | GWS の強制ブロック → 管理コンソールで client ID を信頼登録 |
| MS: invalid_request(redirect_uri) | 登録直後の伝播ラグ → 数分待って再試行。恒常的なら登録 URI が `http://localhost`(パスなし)か確認 |
| MS: AADSTS7000218 | 「パブリック クライアント フローを許可」が無効 → 有効化 |
| 認可が一瞬で終わった/画面が出なかった | 別アカウントで無言認可された可能性 → Step 4 の検証を必ず実行 |
| data directory is locked | デーモン稼働中に書き込み系コマンドを実行した(または二重起動) |
