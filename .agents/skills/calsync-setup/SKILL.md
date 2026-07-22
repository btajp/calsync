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
5. macOS で GUI(デスクトップアプリ、`desktop/`)を使いたいか、CLI で進めたいか(GUI は下記 Step 3〜4 を一気通貫にでき、完了画面からフルリコンサイル(Step 5 のバックフィル実行)の即時実行もできる。ただし Step 1・2 の GCP/Entra 側の準備は代行せず、Step 5 の sqlite3 での突合・実カレンダーの目視確認までは自動化しない。詳細は「GUI(デスクトップアプリ)でアカウントを追加する場合」参照)

**推奨は状況で分岐する。まずどちらに該当するか確認してから伝える**:

- (a) 対象が GWS 組織アカウント中心で、その組織の GCP が使えるなら、**組織プロジェクト + 同意画面 Internal** が最推奨(未検証アプリ警告が出ない・審査不要・最も摩擦が少ない)
- (b) 混在/個人中心、または退職・権限変更でプロジェクトを失いたくない場合は、**個人アカウント側に External + In production** で作る。登録場所とサインインできるアカウントは独立なので、個人側に作っても組織アカウントを認可できる
  - なお (b) の場合、GWS アカウント側の認可は組織の API アクセス制御で管理者の信頼登録が必要になることがある(Step 4 参照)

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

## GUI(デスクトップアプリ)でアカウントを追加する場合

macOS では `desktop/` のデスクトップアプリのアカウント追加ウィザードで、以下の Step 3〜4(設定追記・OAuth 認可・カレンダー選択)を GUI 上で一気通貫にできる。**GCP/Entra 側の準備(Step 1・2)はアプリでは代行しない**。YAML 追記後の「再起動して反映」ボタン、完了画面の「リコンサイルを実行」ボタンはいずれも Step 6 で launchd をすでに導入済みの場合のみ機能する(未導入なら手順案内にフォールバック。詳細は下記の箇条書きを参照)。

- ウィザードの流れ: 前提チェック(`credentials_file` / `client_id` が未設定なら Step 1・2 への案内が出る)→ id・provider 入力 → OAuth 認可(ブラウザ)→ カレンダー選択(Google のみ。Microsoft は primary 固定)→ YAML 追記 → 完了画面(「再起動して反映」ボタン+「リコンサイルを実行」ボタン)
- OAuth フローは既存の `internal/auth` をそのまま再利用しているため、下記 Step 4 で強調したブラウザプロファイル・アカウント選択の注意点(誤った既定プロファイルで無言認可されるリスク、`prompt=select_account` での選択画面確認)はアプリ経由でも同様に当てはまる
- Google のカレンダー選択には `calendar.calendarlist.readonly` スコープが必要。**このウィザードでの認可(新規)には自動付与されるが、既存アカウントは再認可するまで付与されておらずカレンダー一覧が取得できない**(内部的には Google API の 403。アプリ画面には appserver 経由の 502 エラーとして表示される)。その場合アプリはカレンダー ID の手入力にフォールバックするので、そのまま入力を案内してよい(必須ではなく再認可を待つ必要はない)
- アプリはアカウント削除機能を持たない。削除したい場合は `calsync-uninstall` スキルの CLI 手順へ誘導すること
- コンテナ運用の場合、アプリはコンテナ稼働を検出すると全機能を止めた案内表示のみのモードになる。コンテナ運用でのアカウント追加は下記 Step 3〜6(CLI)で進めること
- 完了画面の「リコンサイルを実行」は下記 Step 5 の手順 2(`calsync reconcile` 相当。実行中は保存・デーモン操作ができないバナーが出て数分かかる)を代替する。手順 1(doctor)はダッシュボードの「doctor を実行」ボタンでも代替できるが、手順 3(sqlite3 での突合)と手順 4(実カレンダーの目視確認)はアプリでは自動化されないため、必要ならユーザーに案内すること

## Step 3: 設定ファイルとアカウント追記

- `calsync.yaml` の `accounts` に `id`(短い英数字。**コロン禁止**)/ `provider` / `email` を追記
- Microsoft は v1 ではプライマリカレンダーのみ(calendars 指定は `primary` 固定)
- 設定ファイルは `./data/calsync.yaml` の 1 ファイルに置く(コンテナからは `/data/calsync.yaml` として同じファイルが見える。二重管理しない)。`credentials_file` は運用方式で異なる: Docker はコンテナ内絶対パス `/data/google-client.json`、macOS launchd はホストの絶対パス(例: `<repo>/data/google-client.json`)。コンテナ→ネイティブ移行時にコンテナ内パスのままだと `install-launchd.sh` がエラーで検知する

## Step 4: 認可(最重要・1アカウントずつ)

**ブラウザプロファイルの落とし穴**(実際に事故った):

- `calsync auth add <id> --config ./data/calsync.yaml --data ./data` は既定プロファイルでブラウザを自動オープンする。対象アカウントが別プロファイルにある場合、**自動で開いたタブは使わず閉じ、ターミナルに表示された URL を正しいプロファイルへコピペ**する
- 認可 URL には `prompt=select_account` が付いており必ずアカウント選択画面が出る。**ここで対象アカウントを選び間違えると、別アカウントのトークンがその id で保存される**。選択画面でメールアドレスを必ず確認させること
- 組織アカウント(GWS)で「**このアプリはブロックされます**」が出たら: その組織の管理コンソール → セキュリティ → API の制御 → アプリのアクセス制御 → 「アプリのアクセス権を管理」→「新しいアプリを設定」→ client ID で検索 → 「**信頼できる**」に設定(管理者権限がなければ管理者へ依頼)。個人アカウントなら「詳細 → 移動」でクリックスルー可能
- Microsoft 組織アカウントで `AADSTS65001` / `AADSTS90094` → 管理者同意が必要(README のトラブルシューティング参照)

**認可のたびに必ず検証する**(無言の誤認可を検出するため):

```bash
./calsync auth list --config ./data/calsync.yaml --data ./data   # トークンの有無
# トークンの実アカウント確認(Google。トークン値は表示しない)
TOKEN=$(python3 -c "import json;print(json.load(open('data/tokens/<id>.json'))['access_token'])")
curl -s -H "Authorization: Bearer $TOKEN" \
  'https://www.googleapis.com/calendar/v3/calendars/primary/events?maxResults=1&fields=summary' \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('summary'))"
# 出力がそのアカウントのメールアドレス(またはユーザーが付けた primary カレンダー名)と一致するか
```

Microsoft の場合は同じ要領でトークンを読み、`curl -s -H "Authorization: Bearer $TOKEN" https://graph.microsoft.com/v1.0/me` を叩いて `userPrincipalName` が対象アカウントと一致するか確認する(トークン値は表示しない)。

誤ったアカウントで認可してしまった場合: `./calsync accounts remove <id> --config ./data/calsync.yaml --data ./data` で掃除(誤トークンのまま実行してよい — 誤配置ブロッカーも同じトークン経由で正しく消える)→ 正しいアカウントで `auth add <id> --config ./data/calsync.yaml --data ./data` をやり直す。

## Step 5: バックフィルと稼働確認

1. 全アカウント認可後: `./calsync doctor --config ./data/calsync.yaml --data ./data`(全アカウント API check ok を確認)
2. `./calsync reconcile --config ./data/calsync.yaml --data ./data` — 既存予定の相互配布。**アカウント数 N、予定数 E に対して最大 E×(N-1) 件の書き込み**が走るため数分かかることを事前に伝える
3. `./calsync status --data ./data` と SQLite での確認: `sqlite3 data/calsync.db "SELECT origin_account,target_account,status,COUNT(*) FROM mappings GROUP BY 1,2,3;"`(この sqlite3 直接アクセスはデーモン起動前/停止中のみ。稼働開始後の状態確認は `calsync status` / `doctor` を使い、特にコンテナ運用ではホストから SQLite に読み取りアクセスもしないこと — WAL は VM 境界を跨げず破損実績あり)
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
3. **Install to Workspace** を実行し、`xoxb-` から始まる Bot User OAuth Token を控える。**トークンは `calsync.yaml` に書かない**。渡し方は常駐方式で分岐する: (a) macOS launchd — `export SLACK_BOT_TOKEN=xoxb-...` をシェルのプロファイルに設定した上で `./scripts/macos/install-launchd.sh` を再実行する(plist に権限 600 で埋め込まれる。トークンを変更したときも同スクリプトを再実行する)。(b) Docker — `.env`(コミット禁止)に置き、compose の `environment` で受け渡す。**`calsync.yaml` の `bot_token_env`・compose の `environment` のキー名・`.env` の変数名は 3 つとも一致させること**(既定・同梱の compose は `SLACK_BOT_TOKEN`)。不一致だとトークンが空になり `calsync run` が起動を拒否する
4. `channel` の調べ方: チャンネル宛てはチャンネル詳細画面の一番下にある Channel ID(`C…`/`G…`)、DM 宛てはユーザーのプロフィール →「その他」→ **メンバー ID をコピー**(`U…`)
5. **プライベートチャンネル宛ての場合は、そのチャンネルで `/invite @<App名>` して Bot を招待する**(招待し忘れると送信時に `not_in_channel` エラーになる)
6. `calsync.yaml` に `notifications.slack`(`bot_token_env` / `channel` / `morning_digest` / `remind_before`。両方省略はエラー)を追記する。`remind_before` は `poll_interval`(既定 1m)以上の正の Go duration が必須(下回ると設定エラー)。`morning_digest` の時刻はプロセスのローカル TZ で解釈される(`reconcile_at` と同じ — Docker は compose の `TZ`、launchd はホストのローカル TZ)。反映は launchd なら `install-launchd.sh` を再実行、Docker なら `docker compose up -d --build`(古いイメージのまま新しい設定キーを追加すると `KnownFields(true)` が未知キーとして拒否し起動失敗ループになるため、設定追加とイメージ更新は必ずセットで行う)。**`calsync run` を手動起動しない**(常駐プロセスと `flock` が競合するだけでなく、常駐側の設定に反映されない)。トークン検証は起動時のみ行われる

## セットアップ後の追加オプション(任意)

初期セットアップが終わった後、必要に応じて案内する。いずれも `calsync.yaml` への追記のみで、反映には Step 6/7 で触れた常駐プロセスの再起動(または次回リコンサイル)が必要。手順・YAML 例の詳細は README の該当節に譲り、ここでは重複記載しない。

- **`show_origin_in_description`**(アカウント単位、既定 false): そのアカウントに作られるブロッカーの説明欄に元アカウントの id を表示する。README「ブロッカーの元アカウント表示(オプション)」参照
- **`detail_sync`**(トップレベル、ペア単位): 指定した一方通行ペアに限りタイトル/説明を転記する(`fields: [title, description]` / `visibility: private|default|public`)。組織カレンダーへ転記するペアは編集権限者・管理者・API アクセス者に内容が見える点を確認してから設定する。README「ペア別にタイトル/説明も同期する(detail_sync)」参照
- **`digest_calendars`**(アカウント単位、**google のみ**): 他アカウントへブロッカー配布はせず、朝のダイジェスト通知にだけ載せたいカレンダーを指定する(リマインド対象外)。README「通知専用カレンダー(digest_calendars)」参照

## トラブルシューティング早見

| 症状 | 原因と対処 |
| --- | --- |
| Google: 7日ごとに再認証 | 同意画面が Testing → In production に publish |
| Google: このアプリはブロックされます | GWS の強制ブロック → 管理コンソールで client ID を信頼登録 |
| Google: invalid_client | 6 ヶ月以上停止していたためクライアントが自動削除された(稼働中は発生しない)。30 日以内なら GCP コンソールで復元、超過なら再作成 + JSON 差し替え + auth add 再実行 |
| MS: invalid_request(redirect_uri) | 登録直後の伝播ラグ → 数分待って再試行。恒常的なら登録 URI が `http://localhost`(パスなし)か確認 |
| MS: AADSTS7000218 | 「パブリック クライアント フローを許可」が無効 → 有効化 |
| 認可が一瞬で終わった/画面が出なかった | 別アカウントで無言認可された可能性 → Step 4 の検証を必ず実行 |
| data directory is locked | デーモン稼働中に書き込み系コマンドを実行した(または二重起動) |
