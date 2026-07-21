# デスクトップアプリ 統合カレンダービュー(フェーズ 2)設計書

作成日: 2026-07-21
ステータス: 承認済みドラフト(実装の入力)
前提: デスクトップアプリ v1 スペック(2026-07-21-desktop-app-design.md)§14 の続き。同スペックが未決としていた 3 論点(データ経路・トークン競合・表示コンポーネント)を確定する。

## 1. 目的

全アカウントの実予定を 1 つの週/月カレンダー画面に重ねて表示する(Google カレンダー風)。**ブロッカー(calsync が作った「予定あり」)は除外**し、free の予定・digest 専用カレンダーの予定は含める。

## 2. データ経路(§14 論点 1 の確定)

**プロバイダ API のライブ取得**を採用する。SQLite イベントキャッシュは busy のみ・digest カレンダー非対象のため表示用途には不足。

- 取得は Slack ダイジェストと同じ経路(`Provider.Changes(ctx, ref, "", window)` のカーソルなしフル取得)を任意期間に一般化する。**newCursor は捨てる**(カーソル規律に抵触しない — ダイジェストで実証済みのパターン)
- 対象カレンダー: 各アカウントの `Calendars`(監視対象)+ `DigestCalendars`(ダイジェスト専用)
- 除外判定はダイジェストの `digestIncludes` と同じ 3 層: 削除・辞退・**ブロッカー(mappings 一次+ `calsync-origin` タグ二次)**。free は含める
- 実装: `internal/engine/notify.go` の `collectDigest` / `digestIncludes` を任意 `model.Window` で動く形に一般化し(`Engine.CollectWindow(ctx, w) ([]DigestEntry, []string)` 相当)、既存ダイジェストは 1 日窓でそれを呼ぶ。**既存ダイジェストの挙動・テストは不変であること**。終日判定の「現地日付の文字列比較」は複数日窓では日付文字列の範囲交差に一般化する

## 3. トークン競合の回避(§14 論点 2 の確定)

appserver がプロバイダを構築する際、**リフレッシュしない静的トークンソース**を使う(`oauth2.StaticTokenSource` 相当をトークンファイルから都度ロード)。

- 理由: 稼働中デーモンと appserver が同じトークンをリフレッシュし合うと、Microsoft の refresh token ローテーションでどちらかが失効側を掴む競合が起きうる(v1 スペック §14 で指摘済み)。読み取り専用の静的利用なら構造的に競合しない
- デーモンは毎分の同期でトークンを更新・永続化しているため、ディスク上の access token は通常有効。期限切れ(エッジ)や 401 はそのアカウントを `failed` に載せ、UI がバナー表示する(「一時的に取得できないアカウント: … 数分後に再試行」)。**appserver からのトークン書き込みは一切行わない**
- `internal/clients` に `BuildReadOnlyProvider`(静的トークンソース版)を追加する

## 4. API

`GET /api/events?from=<RFC3339>&to=<RFC3339>`(appserver・Bearer 必須)

- **launchd モード限定**(doctor と同じ 409 ガード)。理由: ブロッカー除外の一次判定に mappings(SQLite・OpenReadOnly)が必要で、DB は launchd 検出時のみ触れる不変条件のため。container は既存ガードで 409
- 制約: 窓の最大幅 62 日(月ビュー+前後余白を包含)。逸脱は 400
- レスポンス:

```json
{
  "events": [
    {
      "account_id": "personal",
      "title": "…",
      "start": "2026-07-21T10:00:00+09:00",
      "end": "2026-07-21T11:00:00+09:00",
      "all_day": false,
      "all_day_start": "",
      "meeting_url": "https://…",
      "html_link": "https://…"
    }
  ],
  "failed": ["outlook"]
}
```

- `events` は `DigestEntry` の写像(dedupe 統合後。`account_id` は代表 = `AccountIDs[0]`、全由来は `account_ids` で返す)。終日イベントは `all_day: true` + `all_day_start`(YYYY-MM-DD)
- 同一窓の連続取得を抑えるため、appserver 内に (from,to) キーの 60 秒メモリキャッシュを持つ(ビュー切替の連打対策。手動更新ボタンはキャッシュをバイパスする `refresh=1` を付ける)

## 5. UI(§14 論点 3 の確定)

**FullCalendar**(`@fullcalendar/react` + `daygrid` + `timegrid`。MIT)を採用。新タブ「カレンダー」。

- ビュー: 週(timeGridWeek・既定)/ 月(dayGridMonth)切替。FullCalendar 標準の前後ナビ・今日ボタン
- 表示範囲の変化(`datesSet`)で `/api/events` を取得。ローディング・エラー表示あり
- 色分け: アカウントごとに固定パレットを巡回割当(Slack ダイジェストの色割当と同じ発想。凡例を表示)
- イベントクリックで `html_link` を既定ブラウザで開く(あれば)。`meeting_url` はツールチップ表示のみ(v1 は装飾最小)
- 除外・統合はサーバー側で完結しているため、フロントは受け取った events を FullCalendar 形式に変換するだけ。この変換(`toFullCalendarEvents`)は純関数として export し vitest でテストする

## 6. スコープ外

- 予定の作成・編集・削除(閲覧専用)
- アカウント別の表示トグル・検索・リマインド表示
- manual モードでの提供(launchd 前提。セットアップ完了後に使う画面のため)
- キャッシュの永続化・オフライン表示

## 7. テスト・検証

- Go: `CollectWindow` の複数日窓(終日の範囲交差・タイムゾーン跨ぎ・ブロッカー除外)を fake provider でテスト。既存ダイジェストのテストが全て無変更で通ること(挙動不変の証明)。`/api/events` は launchd ガード・窓幅制限・キャッシュ・failed 伝搬を httptest でテスト
- フロント: `toFullCalendarEvents`(終日・時刻あり・色割当)の vitest。typecheck / build
- 実機: リリース後に本物のアカウントで表示確認(自動アップデートで配信)
