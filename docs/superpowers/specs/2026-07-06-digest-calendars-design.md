# calsync 通知専用カレンダー(digest_calendars)設計書

作成日: 2026-07-06
ステータス: 承認済みドラフト(実装計画の入力)
親設計書: [2026-07-06-slack-notifications-v2-design.md](2026-07-06-slack-notifications-v2-design.md)(v2。本書は差分)

## 1. 概要・動機

「他アカウントへブロッカーを配布したくないが、朝のダイジェストには載せたい」カレンダーのための第 3 の役割を追加する(実例: 個人アカウントの「ゴミの日」カレンダー — ゴミの日は予定として通知されてほしいが、仕事カレンダーを「予定あり」でブロックしてはならない)。

既存の役割は「監視(= ブロック元・`calendars`)」と「ブロッカー置き場(`blocker_calendar`)」の 2 つ。監視対象に足す回避策は、(a) busy 予定が 1 件でもあれば全アカウントへ配布される(イベント単位の transparency 頼みで保証がない)、(b) カーソル・キャッシュ・日次リコンサイルのオーバーヘッドが用途に対して過剰、のため不採用。

## 2. 設計

```yaml
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    digest_calendars: ["xxxxx@group.calendar.google.com"]  # 通知専用(ブロック元にならない)
```

- **`accounts[].digest_calendars []string`**(新設・省略可): ダイジェストの**ライブ取得にだけ**参加するカレンダー ID のリスト
- **同期エンジンには一切関与しない**: `calendars`(監視)に含まれないため、tick の SyncCalendar・カーソル・events キャッシュ・日次リコンサイル・ブロッカー配布の対象に**構造的に**ならない。エンジン側の決定則・不変条件は無変更
- `collectDigest` のカレンダーループを `acct.Calendars` → `acct.DigestCalendars` の順の連結にする(取得失敗の failed 集約はアカウント単位で従来どおり)。ブロッカー除外(IsBlocker/OriginTag)・dedupe・ソートは既存規則がそのまま適用される(同一アカウント内の重複招待も iCalUID+開始時刻 dedupe が既に吸収する)
- **リマインドは対象外**(キャッシュに入らないため構造的に発火しない)。v1 制約(free・終日のリマインドなし)と同列の制約として明記

## 3. 検証(config.Load)

- `digest_calendars` は **google アカウントのみ許容**。microsoft はエラー(v1 の Graph 実装は `/me/calendarView` 固定でプライマリ以外を取得できないため — 既存の「microsoft は primary のみ」制約と同型のメッセージ)
- 同一アカウント内で `calendars` と `digest_calendars` の**重複はエラー**(二重取得・二重表示の防止)
- `digest_calendars` 内の重複もエラー。空文字列はエラー

## 4. テスト計画

- **config**: google で通る / microsoft でエラー / calendars との重複エラー / リスト内重複・空文字列エラー / 省略時は空
- **engine(fake)**: digest_calendars のイベントがダイジェストに入る / 同カレンダーの busy イベントでも**ブロッカーが作られない**(tick を回して fake の Blockers が空のまま — 本機能の核心の回帰テスト)/ 監視カレンダーとの dedupe / digest_calendars の取得失敗が failed に載る
- 全テスト `-race -count=1`

## 5. ドキュメント

- README: `digest_calendars` の説明+**カレンダー ID の調べ方**(Google カレンダー設定 → 対象カレンダー → 「カレンダーの統合」→ カレンダー ID)+「リマインド対象外」の明記
- CHANGELOG `[Unreleased]` に追記

**実測記録(2026-07-06 17:22)**: 実環境でゴミの日カレンダーの終日予定がダイジェスト先頭に [personal] のアカウント色で掲載されること・ブロッカーが配布されないこと・アカウント別色分け(personal=青 / work-a=黄)をユーザー目視で確認。

## 6. スコープ外

- 通知専用カレンダーのリマインド(必要になったら「監視するが配布しない」役割として別途設計)
- microsoft の非プライマリカレンダー対応(v1 制約のまま)
