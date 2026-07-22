# デスクトップアプリ スケジュール表示・メニューバー常駐・積み残し解消 設計書

作成日: 2026-07-23
ステータス: 承認済みドラフト(実装の入力)
前提: デスクトップアプリ v1(2026-07-21)・統合カレンダービュー(2026-07-21 フェーズ 2)の上に積む。

## 1. スコープ

1. **スケジュール(リスト)表示**: カレンダータブに Google カレンダーの「スケジュール」に相当するリスト表示を追加
2. **メニューバー常駐**: トレイアイコンに「次の予定までの残り時間+タイトル」を表示し、クリックでスケジュールのポップオーバーを開く
3. **積み残し follow-up の全消化**(§5 に列挙)

## 2. スケジュール表示

- FullCalendar の **list plugin**(`@fullcalendar/list`・MIT)を追加し、ツールバーの表示切替に「スケジュール」(`listWeek`)を足す(週 / 月 / スケジュール)
- データ経路・色分け・クリック挙動は既存のカレンダービューと完全共有(取得済み events の別レンダリングにすぎない)

## 3. メニューバー常駐

### 3.1 トレイ表示

- Tauri v2 のトレイ(`@tauri-apps/api/tray` の `TrayIcon`。Cargo の `tray-icon` feature)をメインウィンドウの JS から生成・更新する(殻の Rust にロジックを足さない方針を維持)
- アイコンはテンプレートアイコン(ライト/ダーク自動反転)。タイトル文字列(macOS はアイコン横にテキスト表示可)に**次の予定**を表示:
  - 60 分未満: 「15分後 週次定例…」 / 60 分以上・当日: 「14:00 週次定例…」 / 当日に無ければ翌日以降 7 日以内: 「明日9:00 …」「7/25 …」
  - 対象は**時刻あり予定のみ**(終日は対象外)。タイトルは**全体で 24 文字**に切り詰め(超過は「…」)
  - 次の予定が 7 日以内に無い場合はタイトルなし(アイコンのみ)
- 更新周期: 1 分ごとに表示を再計算。イベントデータは 5 分ごと+ウィンドウのイベント取得成功時に更新(`/api/events` を今日〜+7 日で取得)。この整形(`nextEventLabel(events, now)`)は純関数として export し vitest でテストする

### 3.2 ポップオーバー

- トレイクリックで**専用の小ウィンドウ**(`WebviewWindow`、frameless・約 380x540・always-on-top・タスクバー非表示)をトレイ座標近くに表示。フォーカスが外れたら隠す
- 内容: 今日〜+7 日のスケジュールリスト(§2 と同じリスト部品の簡易版: 日付見出し+時刻+色チップ+タイトル)。上部に「アプリを開く」(メインウィンドウ表示)と「終了」ボタン
- ポップオーバーは `?panel=1` クエリで同じ React アプリを別モードとして描画する(ビルド成果物は 1 つ)。API 接続情報(port/token)はメインウィンドウから Tauri イベントで受け渡す(localStorage に書かない)

### 3.3 ライフサイクル

- **メインウィンドウを閉じてもアプリは終了しない**(hide に変更)。常駐の終了はポップオーバーの「終了」ボタン(`@tauri-apps/plugin-process` の exit)
- Dock アイコンは v1 では表示のまま(Accessory 化はスコープ外)
- サイドカーは従来どおりアプリプロセスの生存に連動(stdin EOF)

### 3.4 権限・設定

- capabilities: `core:tray:default`・`core:image:default`・`core:menu:default`(必要時)・ウィンドウ生成/表示制御(`core:window:allow-*` の必要最小)・`process:allow-exit` を追加。ポップオーバーウィンドウにも同じ capability を適用(`windows` 配列)
- `Cargo.toml`: `tauri = { version = "2", features = ["tray-icon"] }`

## 4. リコンサイル実行(アプリ内メンテナンス)

積み残し (b) の実装仕様。**書き込み系はデーモン停止中のみ**の不変条件を、appserver がメンテナンス窓として一括処理することで守る:

- `POST /api/maintenance/reconcile`(launchd モード限定・409 ガードは既存と同じ): 実行フローは (1) `launchctl bootout` (2) 自バイナリ(`os.Executable()`)で `reconcile --config <path> --data <dir>` をサブプロセス実行・**stdout/stderr をログファイル(`<data>/reconcile-<UTC 時刻>.log`)へ保存** (3) 成否に関わらず `launchctl bootstrap` で再開。非同期実行で、`GET /api/maintenance/state` が `{phase: idle|running|done|error, log_path, error}` を返す(auth フローと同じポーリングパターン)
- 実行中はデーモン制御・設定保存の失敗があり得るため、フロントは maintenance running 中はバナー「リコンサイル実行中(数分かかります)。この間、保存と再起動はできません」を全タブに表示し、該当ボタンを無効化する
- UI 導線: (1) アカウント追加ウィザード完了画面に「既存予定の反映は深夜の自動リコンサイルで行われます。今すぐ反映する場合は実行してください」+実行ボタン (2) ダッシュボードのデーモン状態カードに「リコンサイル実行」ボタン

## 5. 積み残し follow-up(このブランチで全消化)

| # | 内容 | 層 |
| --- | --- | --- |
| F1 | google の ListCalendars エラープレフィックスを `google[%s]:` 慣習に統一 | Go |
| F2 | appserver: `Token == ""` なら `Serve` 起動を拒否(防御) | Go |
| F3 | daemon 制御の launchctl 失敗で stderr 空なら `err.Error()` を message に | Go |
| F4 | `POST /api/auth/start` で account_id を事前検証(`auth.validateAccountID` 相当を公開して使用。ブラウザ往復前に 400) | Go |
| F5 | `GET /api/config` の ReadFile→Stat を Stat→ReadFile→Stat 一致確認に(TOCTOU 縮小) | Go |
| F6 | events キャッシュの期限切れエントリ掃除(set 時に expired を全掃除) | Go |
| F7 | yamledit のコメントドリフト修正: 実機で「digest_calendars の項目コメントが show_origin_in_description 行へ移動」を観測。mergeComments のマップ値ノード位置対応を調査し、再現テスト(実機の構造を模したフィクスチャ)+修正 | Go |
| F8 | Dashboard: 初回 getConfig 失敗時の再試行ボタン+成功時のエラークリア | Front |
| F9 | ConfigForm: providers 枝の spread-then-override 統一 | Front |
| F10 | sidecar.ts: handshake timeout 後に遅れて spawn した child を kill | Front |
| F11 | dev の StrictMode 二重 spawn ガード(spawn 済みなら再 spawn しない module スコープガード。挙動は dev のみ影響) | Front |
| F12 | フォルダ選択時の検証: 選択ディレクトリに `calsync.yaml` が無ければ「data ディレクトリを選んでいますか?」警告(選び直し/そのまま使う) | Front |
| F13 | AccountAdd: authCancel の HTTP 失敗を可視化(エラー行表示) | Front |
| F14 | CSP 強化: `default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' http://127.0.0.1:*; img-src 'self' data:` を設定(FullCalendar/React の inline style を許容しつつ外部を遮断)。リリース前に実 UI 動作をスモーク必須 | conf |

## 6. スコープ外

- Dock 非表示(Accessory)・ログイン時自動起動・トレイからの予定作成
- ポップオーバーのリッチ表示(会議参加ボタン等)
- Windows / Linux のトレイ対応

## 7. テスト・検証

- Go: 各 F 項目の回帰テスト+maintenance API(fake runner で bootout→reconcile→bootstrap の順序・失敗時も bootstrap されること・log_path 生成)
- Front: `nextEventLabel`(残り分/当日時刻/翌日/7日超なし/24 文字切り詰め)・スケジュールリスト整形・F 系の対象ロジックを vitest
- トレイ・ポップオーバー・CSP は実機でしか検証できないため、リリース後にユーザー確認(既知の検証手段: 自動アップデートで配信 → 目視)
