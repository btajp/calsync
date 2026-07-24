# デスクトップアプリ 予定詳細のアプリ内表示 設計書

作成日: 2026-07-24
ステータス: 承認済みドラフト(実装の入力)
前提: 統合カレンダービュー(2026-07-21)・トレイ/ポップオーバー(2026-07-23)の上に積む。

## 1. 目的

予定クリック時にブラウザの Google/Outlook カレンダーへ飛ばすのではなく、**アプリ内で詳細を表示**し、そこから **Web 会議に参加**できるようにする。カレンダータブとメニューバーのポップオーバーの両方が対象。

## 2. データ

- `GET /api/events` の各イベントに `description`(プレーンテキスト。Graph は Prefer text / Google は HTML 除去済み — 既存 `DigestEntry.Description` の写像)を追加する。他のフィールドは既存のまま(meeting_url / html_link は実装済み)
- 場所(location)フィールドは NormalizedEvent に存在しないため対象外

## 3. UI

### 3.1 共通の詳細表示部品(`EventDetail`)

- 表示: タイトル / 日時(終日は「終日」・複数日は範囲)/ アカウント(色チップ+id。dedupe 統合された場合は全 account_ids)/ 説明文(改行保持・URL は自動リンク化し https のみ既定ブラウザで開く)
- アクション: **「会議に参加」ボタン**(meeting_url があるときのみ。https 検証つきで既定ブラウザ/会議アプリへ)・「カレンダーで開く」リンク(html_link。従来動作の退避先)・閉じる
- 実装は 1 コンポーネントをカレンダービューとポップオーバーで共用(ビルド成果物は同一)

### 3.2 カレンダータブ

- イベントクリック → モーダル(オーバーレイ)で EventDetail を表示。背景クリック / ✕ / Esc で閉じる
- FullCalendar へは `toFullCalendarEvents` の extendedProps に EventOut 全体を持たせて受け渡す

### 3.3 ポップオーバー

- スケジュール項目クリック → パネル内で詳細ビューに切り替え(「← 戻る」でリストへ)。ポップオーバーの blur→hide 挙動は不変
- `ScheduleItem` に元の EventOut への参照を持たせる

## 4. テスト

- Go: events レスポンスに description が載る回帰テスト
- Front: 説明文のリンク化(`linkifyDescription` 純関数: URL 分割・https 判定)・EventDetail の表示条件(会議ボタン有無・終日/範囲表示)・ScheduleItem の参照持ち回りを vitest
- 実機: リリース後確認(クリック動線・会議参加)

## 5. スコープ外

- 参加/欠席の返信・予定編集・出席者一覧・場所表示・リマインダー
