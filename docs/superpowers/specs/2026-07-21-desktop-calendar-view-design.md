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
- **タイムゾーン契約**: from/to は閲覧者のローカルオフセット付き RFC3339 を送ること(終日イベントの日付境界は from/to のオフセットで解釈される。UTC を送ると JST 環境で終日が 1 日ずれる)
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
      "all_day_end": "",
      "meeting_url": "https://…",
      "html_link": "https://…"
    }
  ],
  "failed": ["outlook"]
}
```

- `events` は `DigestEntry` の写像(dedupe 統合後。`account_id` は代表 = `AccountIDs[0]`、全由来は `account_ids` で返す)。終日イベントは `all_day: true` + `all_day_start`(YYYY-MM-DD)。複数日にまたがる終日イベントは排他的終了日を `all_day_end`(YYYY-MM-DD)に設定する(単日終日イベントは空文字。`model.NormalizedEvent.AllDayEnd` → `DigestEntry.AllDayEnd` → `EventOut.AllDayEnd` の 3 層で運ぶ)。フロントは `all_day_end` があれば FullCalendar の排他的終了日としてそのまま使う(同じ排他的終了日の規約なので変換不要)
- 同一窓の連続取得を抑えるため、appserver 内に (from,to) キーの 60 秒メモリキャッシュを持つ(ビュー切替の連打対策。手動更新ボタンはキャッシュをバイパスする `refresh=1` を付ける)
- **stale-while-revalidate(2026-07-28 体感速度対策)**: TTL 切れでも 30 分の猶予内なら古い内容に `stale: true` を付けて即返し、バックグラウンド(single-flight・リクエスト独立の 60 秒 timeout ctx)で取り直してキャッシュを最新化する。フロントは stale 応答の約 4 秒後に 1 回だけ再取得する(再取得でも stale なら以後は自然な再取得に任せ、無限リトライにしない)。`refresh=1` は従来どおり同期取得
- **取得の並列化(2026-07-28)**: `Engine.CollectWindow` はアカウント間の取得を並列に行う(アカウント内のカレンダーは従来どおり直列)。dedupe(`appendDigestEntry`)の「設定順で最初の非空を採用」という決定的規則を保つため、並列なのは取得だけで、マージは全取得完了後に設定順の単一ループで行う

## 5. UI(§14 論点 3 の確定)

**FullCalendar**(`@fullcalendar/react` + `daygrid` + `timegrid`。MIT)を採用。新タブ「カレンダー」。

- ビュー: 週(timeGridWeek・既定)/ 月(dayGridMonth)切替。FullCalendar 標準の前後ナビ・今日ボタン
- 現在時刻・今日の強調(2026-07-28 実機フィードバック反映): 週/日ビューに `nowIndicator`(現在時刻の水平線)を表示。今日の背景は `--fc-today-bg-color` を濃い青系に上書きし、列見出し/日付番号をピル型で強調。「今日」ボタンは押せる状態(今日が表示範囲外)のとき青色で目立たせる
- 現在時刻ラインの Google 風強化(同日 2 回目のフィードバック: 1px は見えにくい): today 列は 2px 実線+左端に丸ドット(::before)、週全体に半透明の細ライン(::after を ±100vw に伸ばし .fc-scroller のクリップで収める)。ラインの親 `.fc-timegrid-now-indicator-container` は既定 overflow:hidden のため visible に上書きが必要(子は now ライン系のみで副作用なし)。既定の三角矢印はドットと重複するため非表示
- 表示範囲の変化(`datesSet`)で `/api/events` を取得。ローディング・エラー表示あり
- 色分け: アカウントごとに固定パレットを巡回割当(Slack ダイジェストの色割当と同じ発想。凡例を表示)
- イベントクリックで `html_link` を既定ブラウザで開く(あれば)。`meeting_url` はツールチップ表示のみ(v1 は装飾最小)
- 除外・統合はサーバー側で完結しているため、フロントは受け取った events を FullCalendar 形式に変換するだけ。この変換(`toFullCalendarEvents`)は純関数として export し vitest でテストする
- 描画規則(2026-07-29 実機フィードバック反映): 予定枠に収まらない文字はクリップ(`.fc-timegrid-event { overflow: hidden }`)し、全文はホバーの title 属性(タイトル+会議 URL)とクリックの詳細モーダルで見せる。被っている予定は harness の right を `0 !important` で上書きして列右端まで伸ばし、FullCalendar のインライン z-index による Google 風カスケードにする(長時間ブロックと重なる予定が半分以下に潰れるのを防ぐ)
- 重なりの左インセット圧縮(同日 2 回目のフィードバック): 純関数 `compactOverlapLefts` が「1 段 = 5%」へ詰め替える。段数は FullCalendar が harness の inline z-index に入れる stackDepth+1 から読む。**開始位置(top ±3px)が同じ予定同士は FullCalendar の横並びを維持**(詰めると後の予定が前をほぼ完全に覆うため。Google も同時開始は横並び)。DOM への適用は datesSet / events 変化 / resize 後の rAF で再実行(FullCalendar の再レンダリングがインライン style を戻すため)
- 週ビューはタイトル先頭+時刻範囲を後置(幅が足りなければ末尾からクリップ — 表示できるときは終了時刻も見せる。0.5.5 の開始のみ表示は 0.5.6 で範囲に戻した)。30 分予定の最初の 1 行が「時刻だけ」になるのを防ぐ。月/リストは時刻先頭のまま。スロット高さは 2em(既定 1.5em)
- 同時開始クラスタのリスト箱(2026-07-29 同時刻表示検討・案1、0.6.0): `clusterSameStart` が同一 instant 開始の時刻あり予定を束ね、`toFullCalendarEvents` が 1 つの合成イベント(end = 最遅終了、extendedProps.cluster)に変換。描画は「1 行 = 1 予定(折り返さない)」の全幅行リスト+「N件」バッジ+行間ディバイダー+左端レール(本数=件数、高さ=長さ比)。行クリックは eventClick 内で data-cluster-index を closest 検索して該当予定の詳細を開く(FullCalendar のネイティブ eventClick が先に発火するため行側 stopPropagation では二重発火を防げない)。行順は終了の遅い順→タイトル昇順。レビュー反映: リスト描画は週/日ビュー限定(月/リストは背景箱が無く白系装飾が不可視のため連結タイトル)、レールは pointer-events:none+最大 4 本+ガター幅は本数連動、1 行目はバッジ分の右余白を確保、ツールチップは会議 URL も含む、ホバー展開中はレール非表示(高さの意味が失われるため)
- タイトル正規化(案4、0.6.0): `splitTitlePrefix` が先頭の【…】(8 文字以内・接頭辞のみのタイトルは除外)をチップ化。週ビューの単独予定とクラスタ行に適用
- ホバー全幅展開(案3、0.6.0): `:hover` の CSS のみで harness を left:0 !important・z-index 60 に、イベント本体を height:auto に展開して全文を見せる。重なり詰めの inline left は !important を付けない(この :hover が勝てるように)。0.6.1: 対象を `:not(.fc-timegrid-event-harness-inset)`(= stackForward 0、前に誰も重なっていない予定)に限定 — 背面の長時間ブロックが展開すると前面の予定を覆い隠し、その予定が矩形内包されている場合はホバー到達不能になるため(実機で観測)

## 6. スコープ外

- 予定の作成・編集・削除(閲覧専用)
- アカウント別の表示トグル・検索・リマインド表示
- manual モードでの提供(launchd 前提。セットアップ完了後に使う画面のため)
- キャッシュの永続化・オフライン表示

## 7. テスト・検証

- Go: `CollectWindow` の複数日窓(終日の範囲交差・タイムゾーン跨ぎ・ブロッカー除外)を fake provider でテスト。既存ダイジェストのテストが全て無変更で通ること(挙動不変の証明)。`/api/events` は launchd ガード・窓幅制限・キャッシュ・failed 伝搬を httptest でテスト
- フロント: `toFullCalendarEvents`(終日・時刻あり・色割当)の vitest。typecheck / build
- 実機: リリース後に本物のアカウントで表示確認(自動アップデートで配信)
