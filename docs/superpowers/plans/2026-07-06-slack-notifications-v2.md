# Slack 通知 v2(Block Kit・会議 URL・Description)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ダイジェスト/リマインドを Block Kit 化し、会議 URL(Zoom/Meet/Teams)の参加ボタンとリマインドへの description 全文表示を追加する。

**Architecture:** v1 の title と同じ配管で `NormalizedEvent` / events キャッシュに `MeetingURL` / `Description` / `HTMLLink` を追加。Slack 層に `blocks.go`(レンダリング+エスケープ後切り詰め+URL 検証)を新設し、`chat.postMessage` を `{channel, text, blocks}` に拡張(text は v1 テキストの fallback)。`invalid_blocks` 時のみ text 単体で 1 回縮退再送。

**Tech Stack:** Go 1.25(依存追加なし。HTML 除去は std `html` + regexp、Block Kit は素の JSON)。

**Spec:** `docs/superpowers/specs/2026-07-06-slack-notifications-v2-design.md`(以下「スペック」。v1 スペック=親設計書の挙動は明記なき限り不変)。

## Global Constraints

- 検証コマンド(v1 と同一): `go build -o ./calsync ./cmd/calsync` / `go test ./... -race -count=1` / `go vet ./...` / `gofmt -l internal/ cmd/`(出力なし)
- **`go mod tidy` 禁止・依存追加なし**。コミットは Conventional Commits(英語)、コメントは日本語
- `MeetingURL` / `Description` / `HTMLLink` を **`model.TimeHash` の入力に絶対に含めない**
- **切り詰めは必ず「escapeText 適用後の文字列に rune 単位」**で行い、実体参照の途中で切らない(スペック 7 章)
- URL は**出所を問わずレンダリング直前に検証**(`https://` 前方一致・空白 `|` `<` `>` なし・2,000 rune 以内)。不合格はリンク化/ボタン化しない
- 通知の失敗で Run を落とさない(v1 のまま)

## タスク依存関係

Task 1 → Task 2・3・4(並行可)。Task 5 は 1・2 の後。Task 6 は 5 の後。Task 7 は 6 の後。Task 8 は最後。

---

### Task 1: model — 3 フィールドと ExtractMeetingURL

**Files:**
- Modify: `internal/model/model.go`(NormalizedEvent)
- Create: `internal/model/meeting.go`
- Test: `internal/model/model_test.go`(追記)、`internal/model/meeting_test.go`(新規)

**Interfaces:**
- Produces: `NormalizedEvent.MeetingURL / Description / HTMLLink string`、`func ExtractMeetingURL(location, description string) string`

- [ ] **Step 1: 失敗するテストを書く**

`internal/model/model_test.go` の `TestTimeHashIgnoresTitle` をリネームせず、新テストを追加:

```go
// MeetingURL / Description / HTMLLink も表示専用であり TimeHash に影響しない(スペック 2 章)。
func TestTimeHashIgnoresDisplayFields(t *testing.T) {
	base := NormalizedEvent{
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
	}
	full := base
	full.MeetingURL = "https://zoom.us/j/123"
	full.Description = "本文"
	full.HTMLLink = "https://calendar.google.com/event?eid=x"
	require.Equal(t, TimeHash(base), TimeHash(full))
}
```

新規 `internal/model/meeting_test.go`:

```go
package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractMeetingURL(t *testing.T) {
	tests := []struct {
		name, location, description, want string
	}{
		{"zoom with subdomain in location", "https://work-a.zoom.us/j/89335149431?pwd=abc", "", "https://work-a.zoom.us/j/89335149431?pwd=abc"},
		{"zoom without subdomain", "", "join: https://zoom.us/j/123456789", "https://zoom.us/j/123456789"},
		{"zoom my-path", "", "https://zoom.us/my/example", "https://zoom.us/my/example"},
		{"meet", "", "https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij"},
		{"teams", "", "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x", "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x"},
		{"location wins over description", "https://meet.google.com/loc-loc-loc", "https://zoom.us/j/999", "https://meet.google.com/loc-loc-loc"},
		{"leftmost match within a field (meet before zoom)", "", "先: https://meet.google.com/aaa-bbbb-ccc 後: https://zoom.us/j/1", "https://meet.google.com/aaa-bbbb-ccc"},
		{"parenthesized url drops trailing paren", "", "(https://meet.google.com/abc-defg-hij)", "https://meet.google.com/abc-defg-hij"},
		{"trailing period dropped", "", "https://zoom.us/j/123456789.", "https://zoom.us/j/123456789"},
		{"pipe terminates url", "", "https://meet.google.com/abc|x", "https://meet.google.com/abc"},
		{"http is ignored", "", "http://zoom.us/j/123", ""},
		{"no match", "会議室A", "資料を読んでおく", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ExtractMeetingURL(tt.location, tt.description))
		})
	}
}
```

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/model/ -race -count=1 -run 'DisplayFields|ExtractMeetingURL'`
Expected: FAIL(コンパイルエラー: フィールド・関数未定義)

- [ ] **Step 3: 実装**

`internal/model/model.go` — `Title` の直後に追加:

```go
	Title       string // 件名(Slack 通知の表示専用。TimeHash には含めない — v1 スペック 4.1)
	MeetingURL  string // 会議 URL(v2 スペック 3.2 の規則で抽出。表示時に URL 検証を通す)
	Description string // 本文プレーンテキスト(Graph: Prefer で text 化 / Google: 簡易 HTML 除去済み)
	HTMLLink    string // カレンダー上の当該予定への URL(Google: htmlLink / Graph: webLink)
```

新規 `internal/model/meeting.go`:

```go
package model

import (
	"regexp"
	"strings"
)

// meetingURLRe は location / description から会議 URL を拾うフォールバック正規表現
// (v2 スペック 3.2)。https:// 固定(スキーム注入を構造的に排除)。zoom はサブドメイン
// 省略可。終端は空白・" < > | まで(mrkdwn 構文文字を URL に混入させない)。
var meetingURLRe = regexp.MustCompile(
	`https://(?:[a-z0-9-]+\.)?zoom\.us/(?:j|my)/[^\s"<>|]+` +
		`|https://meet\.google\.com/[^\s"<>|]+` +
		`|https://teams\.microsoft\.com/l/meetup-join/[^\s"<>|]+`)

// ExtractMeetingURL は location → description の順に会議 URL を探す。
// 各フィールド内では出現位置が最も先頭の一致を採用(leftmost match。パターン種別間に
// 優先順位は付けない)。切り出し後、末尾の ) ] . , ; を除去する(括弧囲い・文末句読点の
// 巻き込み防止。v2 スペック 3.2)。
func ExtractMeetingURL(location, description string) string {
	for _, s := range []string{location, description} {
		if m := meetingURLRe.FindString(s); m != "" {
			return strings.TrimRight(m, ")].,;")
		}
	}
	return ""
}
```

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/model/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/model/
git commit -m "feat: add meeting URL, description, and calendar link to NormalizedEvent"
```

---

### Task 2: store — events 3 列と UpcomingEvent 拡張

**Files:**
- Modify: `internal/store/store.go`(const schema の events + `migrate()`)
- Modify: `internal/store/events.go`(UpsertEvent / GetEvent)
- Modify: `internal/store/reminders.go`(UpcomingEvent / ListUpcomingEvents)
- Test: `internal/store/migrate_test.go`(拡張)、`internal/store/reminders_test.go`(拡張)

**Interfaces:**
- Consumes: Task 1 の 3 フィールド
- Produces: `UpcomingEvent.MeetingURL / Description / HTMLLink string`(Task 5 の checkReminders が読む)

- [ ] **Step 1: 失敗するテストを書く**

`internal/store/migrate_test.go` の `TestOpenMigratesEventsTitleColumn` に追記(既存 DB 作成 SQL は**変更しない** — v1 相当の旧スキーマから 4 列全部が足されることの検証になる):

```go
	// v2: 3 列も冪等 ALTER で追加され、ラウンドトリップする(v2 スペック 3.3)
	require.NoError(t, st2.UpsertEvent(ref, model.NormalizedEvent{
		ID: "ev-v2", ICalUID: "v2@example.com", Title: "件名",
		MeetingURL:  "https://zoom.us/j/123",
		Description: "本文テキスト",
		HTMLLink:    "https://calendar.google.com/event?eid=x",
		StartUTC:    time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC),
		EndUTC:      time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		IsBusy:      true,
	}))
	got3, err := st2.GetEvent(ref, "ev-v2")
	require.NoError(t, err)
	require.Equal(t, "https://zoom.us/j/123", got3.MeetingURL)
	require.Equal(t, "本文テキスト", got3.Description)
	require.Equal(t, "https://calendar.google.com/event?eid=x", got3.HTMLLink)
```

※ `st2` は既存テスト末尾の 2 回目 Open のハンドル。`st2.Close()` の**前**に挿入する。

`internal/store/reminders_test.go` の `TestListUpcomingEvents` — `remEvent` ヘルパーに 3 フィールドを追加し、取得検証を追記:

```go
func remEvent(id, ical, title string, start time.Time) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID: id, ICalUID: ical, Title: title,
		MeetingURL:  "https://zoom.us/j/" + id,
		Description: "desc-" + id,
		HTMLLink:    "https://cal.example.com/" + id,
		StartUTC:    start, EndUTC: start.Add(30 * time.Minute), IsBusy: true,
	}
}
```

`TestListUpcomingEvents` 末尾に:

```go
	require.Equal(t, "https://zoom.us/j/in-window", got[0].MeetingURL)
	require.Equal(t, "desc-in-window", got[0].Description)
	require.Equal(t, "https://cal.example.com/in-window", got[0].HTMLLink)
```

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/store/ -race -count=1`
Expected: FAIL(コンパイルエラーまたは空値)

- [ ] **Step 3: 実装**

(a) `internal/store/store.go` const schema、events の `title` 行の直後に:

```sql
  meeting_url   TEXT NOT NULL DEFAULT '',
  description   TEXT NOT NULL DEFAULT '',
  html_link     TEXT NOT NULL DEFAULT '',
```

(b) `migrate()` を ALTER のループに書き換え:

```go
// migrate は既存 DB への後方互換の列追加を行う。方針(v1 スペック 4.2 で明文化):
// 新規 DB は const schema、既存 DB は Open 時の冪等 ALTER(duplicate column のみ無視)。
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE events ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN meeting_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN html_link TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range alters {
		if _, err := db.Exec(q); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("events schema migration: %w", err)
			}
		}
	}
	return nil
}
```

(c) `internal/store/events.go` — `UpsertEvent` の INSERT 列・VALUES・DO UPDATE SET・引数に `meeting_url` / `description` / `html_link` を追加(引数は `ev.Title` の後に `ev.MeetingURL, ev.Description, ev.HTMLLink`)。`GetEvent` の SELECT に 3 列を追加し `sql.NullString` で受けて復元。

(d) `internal/store/reminders.go` — `UpcomingEvent` に `MeetingURL, Description, HTMLLink string` を追加。`ListUpcomingEvents` の SELECT を

```sql
SELECT account_id, calendar_id, event_id, ical_uid, title, meeting_url, description, html_link, start_utc, end_utc
```

に変更し、Scan も同順で追加(3 列とも `sql.NullString`)。

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/store/ -race -count=1` → PASS。続けて `go test ./... -race -count=1` → PASS

- [ ] **Step 5: コミット**

```bash
git add internal/store/
git commit -m "feat: persist meeting URL, description, and calendar link in the events cache"
```

---

### Task 3: google プロバイダ — 抽出・HTML 除去・htmlLink(+fake 契約)

**Files:**
- Modify: `internal/provider/google/changes.go`(normalizeEvent)
- Create: `internal/provider/google/description.go`
- Test: `internal/provider/google/changes_test.go`(追記)、`internal/provider/google/description_test.go`(新規)、`internal/provider/fake/fake_test.go`(追記)

**Interfaces:**
- Consumes: Task 1 の `model.ExtractMeetingURL` と 3 フィールド

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/google/changes_test.go` に追記:

```go
func TestNormalizeEventMeetingFields(t *testing.T) {
	base := func() *calendar.Event {
		return &calendar.Event{
			Id:    "ev1",
			Start: &calendar.EventDateTime{DateTime: "2026-07-10T01:00:00Z"},
			End:   &calendar.EventDateTime{DateTime: "2026-07-10T02:00:00Z"},
		}
	}

	// conferenceData の video エントリポイントが最優先(v2 スペック 3.2)
	ev := base()
	ev.HangoutLink = "https://meet.google.com/fallback"
	ev.ConferenceData = &calendar.ConferenceData{EntryPoints: []*calendar.EntryPoint{
		{EntryPointType: "phone", Uri: "tel:+81-3-0000-0000"},
		{EntryPointType: "video", Uri: "https://work-a.zoom.us/j/89335149431"},
	}}
	got := normalizeEvent(ev)
	require.Equal(t, "https://work-a.zoom.us/j/89335149431", got.MeetingURL)

	// conferenceData が無ければ hangoutLink
	ev = base()
	ev.HangoutLink = "https://meet.google.com/abc-defg-hij"
	require.Equal(t, "https://meet.google.com/abc-defg-hij", normalizeEvent(ev).MeetingURL)

	// どちらも無ければ location/description の正規表現フォールバック
	ev = base()
	ev.Location = "https://zoom.us/j/123456789"
	require.Equal(t, "https://zoom.us/j/123456789", normalizeEvent(ev).MeetingURL)

	// htmlLink と description(HTML 除去済み)
	ev = base()
	ev.HtmlLink = "https://www.google.com/calendar/event?eid=xyz"
	ev.Description = "資料<br>リンク: <a href=\"https://example.com\">here</a>&amp;co"
	got = normalizeEvent(ev)
	require.Equal(t, "https://www.google.com/calendar/event?eid=xyz", got.HTMLLink)
	require.Equal(t, "資料\nリンク: here&co", got.Description)
}
```

新規 `internal/provider/google/description_test.go`:

```go
package google

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripHTML(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text passes through", "普通のテキスト", "普通のテキスト"},
		{"br to newline", "1行目<br>2行目<br/>3行目", "1行目\n2行目\n3行目"},
		{"closing p to newline", "<p>段落1</p><p>段落2</p>", "段落1\n段落2"},
		{"anchor tag removed keeping text", `<a href="https://zoom.us/j/1">参加</a>`, "参加"},
		{"entities unescaped", "A &amp; B &lt;C&gt;", "A & B <C>"},
		{"mixed", `会議<br><a href="https://x">リンク</a>&nbsp;end`, "会議\nリンク end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stripHTML(tt.in))
		})
	}
}
```

`internal/provider/fake/fake_test.go` に追記:

```go
// fake は v2 の表示フィールドも素通しする(実プロバイダと同じ契約)。
func TestChangesPreservesDisplayFields(t *testing.T) {
	f := New()
	cal := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	f.SetFullState(cal, []model.NormalizedEvent{{
		ID: "ev1", MeetingURL: "https://zoom.us/j/1", Description: "d", HTMLLink: "https://cal/x", IsBusy: true,
	}})
	evs, _, err := f.Changes(context.Background(), cal, "", model.Window{})
	require.NoError(t, err)
	require.Equal(t, "https://zoom.us/j/1", evs[0].MeetingURL)
	require.Equal(t, "d", evs[0].Description)
	require.Equal(t, "https://cal/x", evs[0].HTMLLink)
}
```

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/provider/... -race -count=1 -run 'MeetingFields|StripHTML|DisplayFields'`
Expected: google 側 FAIL(未定義)。fake は PASS(素通し契約 — 回帰防止として残す)

- [ ] **Step 3: 実装**

新規 `internal/provider/google/description.go`:

```go
package google

import (
	"html"
	"regexp"
	"strings"
)

var (
	brTagRe  = regexp.MustCompile(`(?i)<br\s*/?>|</p>`)
	anyTagRe = regexp.MustCompile(`<[^>]*>`)
)

// stripHTML は Google の description(HTML 断片を含みうる)を表示用プレーンテキストへ
// 近似変換する(v2 スペック 3.4)。依存追加なしの簡易変換であり完全な HTML パースは
// しない: (1) <br>/</p> を改行に (2) 残タグを除去 (3) std html で実体参照を復元。
// 表示専用の正規化であり同期ロジック(TimeHash 等)には影響しない。
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	s = brTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}
```

`internal/provider/google/changes.go` の `normalizeEvent`、`ev.Title = item.Summary` の直後に:

```go
	ev.HTMLLink = item.HtmlLink
	ev.Description = stripHTML(item.Description)
	ev.MeetingURL = googleMeetingURL(item, ev.Description)
```

同ファイル末尾に:

```go
// googleMeetingURL は会議 URL を抽出する(v2 スペック 3.2 の優先順)。
// conferenceData の video エントリポイント → hangoutLink → location/description の正規表現。
func googleMeetingURL(item *calendar.Event, plainDesc string) string {
	if item.ConferenceData != nil {
		for _, ep := range item.ConferenceData.EntryPoints {
			if ep != nil && ep.EntryPointType == "video" && ep.Uri != "" {
				return ep.Uri
			}
		}
	}
	if item.HangoutLink != "" {
		return item.HangoutLink
	}
	return model.ExtractMeetingURL(item.Location, plainDesc)
}
```

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/provider/... -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/provider/google/ internal/provider/fake/
git commit -m "feat: extract meeting URL, plain-text description, and htmlLink in the Google provider"
```

---

### Task 4: microsoft プロバイダ — フィールド追加と Prefer ヘッダー更新

**Files:**
- Modify: `internal/provider/microsoft/delta.go`(deltaEvent / normalizeDeltaEvent)
- Modify: `internal/provider/microsoft/microsoft.go`(`do()` の Prefer 値)
- Test: `internal/provider/microsoft/delta_test.go`(追記+`requireCommonHeaders` 更新)

**Interfaces:**
- Consumes: Task 1 の `model.ExtractMeetingURL` と 3 フィールド

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/microsoft/delta_test.go` に追記:

```go
func TestNormalizeDeltaEventMeetingFields(t *testing.T) {
	busy := map[string]bool{"busy": true}
	base := func() deltaEvent {
		return deltaEvent{
			ID: "ev1", ShowAs: "busy",
			Start: &graphTime{DateTime: "2026-07-10T01:00:00.0000000", TimeZone: "UTC"},
			End:   &graphTime{DateTime: "2026-07-10T02:00:00.0000000", TimeZone: "UTC"},
		}
	}

	// onlineMeeting.joinUrl が最優先(v2 スペック 3.2)
	de := base()
	de.OnlineMeetingURL = "https://legacy.example.com"
	de.OnlineMeeting = &graphOnlineMeeting{JoinURL: "https://teams.microsoft.com/l/meetup-join/19%3ax"}
	got, err := normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://teams.microsoft.com/l/meetup-join/19%3ax", got.MeetingURL)

	// joinUrl が無ければ onlineMeetingUrl
	de = base()
	de.OnlineMeetingURL = "https://legacy.example.com"
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://legacy.example.com", got.MeetingURL)

	// どちらも無ければ location/body の正規表現フォールバック
	de = base()
	de.Location = &graphLocation{DisplayName: "https://work-a.zoom.us/j/86032012178"}
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://work-a.zoom.us/j/86032012178", got.MeetingURL)

	// body(Prefer で text 化済み)と webLink の素通し
	de = base()
	de.Body = &graphBody{ContentType: "text", Content: "アジェンダ\n1. 進捗"}
	de.WebLink = "https://outlook.live.com/calendar/item/xyz"
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "アジェンダ\n1. 進捗", got.Description)
	require.Equal(t, "https://outlook.live.com/calendar/item/xyz", got.HTMLLink)
}
```

`requireCommonHeaders` の Prefer 期待値を新しい併記値に更新(delta_test.go:46 付近):

```go
	require.Equal(t, `IdType="ImmutableId", outlook.body-content-type="text"`, rr.Header.Get("Prefer"), ...)
```

※ 全リクエスト共通の値なので分岐は不要(v2 スペック 3.4 の決定)。blockers_test.go も同ヘルパー経由のため自動で新期待値になる。

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/provider/microsoft/ -race -count=1`
Expected: FAIL(未定義型+Prefer 期待値不一致)

- [ ] **Step 3: 実装**

(a) `internal/provider/microsoft/microsoft.go` の `do()`:

```go
	// 全 Graph リクエスト共通: ImmutableId(v1 スペック)+ body の text 化(v2 スペック 3.4)。
	// delta 以外のエンドポイントは応答から id しか読まないため body-content-type は無害
	req.Header.Set("Prefer", `IdType="ImmutableId", outlook.body-content-type="text"`)
```

(b) `internal/provider/microsoft/delta.go` — 型追加と deltaEvent 拡張:

```go
type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type graphOnlineMeeting struct {
	JoinURL string `json:"joinUrl"`
}
```

`deltaEvent` に追加(`Subject` の下):

```go
	Body             *graphBody          `json:"body"`
	Location         *graphLocation      `json:"location"`
	OnlineMeeting    *graphOnlineMeeting `json:"onlineMeeting"`
	OnlineMeetingURL string              `json:"onlineMeetingUrl"`
	WebLink          string              `json:"webLink"`
```

`normalizeDeltaEvent` の `ev.Title = de.Subject` の直後に:

```go
	ev.HTMLLink = de.WebLink
	if de.Body != nil {
		// Prefer: outlook.body-content-type="text" によりプレーンテキスト(実測 2026-07-06)
		ev.Description = de.Body.Content
	}
	loc := ""
	if de.Location != nil {
		loc = de.Location.DisplayName
	}
	switch {
	case de.OnlineMeeting != nil && de.OnlineMeeting.JoinURL != "":
		ev.MeetingURL = de.OnlineMeeting.JoinURL
	case de.OnlineMeetingURL != "":
		ev.MeetingURL = de.OnlineMeetingURL
	default:
		ev.MeetingURL = model.ExtractMeetingURL(loc, ev.Description)
	}
```

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/provider/microsoft/ -race -count=1` → PASS。続けて `go test ./... -race -count=1` → PASS

- [ ] **Step 5: コミット**

```bash
git add internal/provider/microsoft/
git commit -m "feat: extract meeting URL, text body, and webLink in the Graph provider"
```

---

### Task 5: engine — DigestEntry 拡張と dedupe ペア規則

**Files:**
- Modify: `internal/engine/notify.go`(DigestEntry / appendDigestEntry / collectDigest の entry 構築 / checkReminders の entry 構築)
- Test: `internal/engine/notify_test.go`(追記)

**Interfaces:**
- Consumes: Task 1・2 の 3 フィールド
- Produces: `DigestEntry.MeetingURL / Description / HTMLLink string`(Task 6 の blocks が読む)

- [ ] **Step 1: 失敗するテストを書く**

`internal/engine/notify_test.go` に追記:

```go
// dedupe 統合: Title と HTMLLink は「最初に HTMLLink が非空のアカウント」からペアで
// 採用し、MeetingURL / Description は独立に最初の非空(v2 スペック 4 章)。
func TestCollectDigestMergesDisplayFieldsPairwise(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc)

	// a(YAML 先頭): HTMLLink なし・件名あり・URL なし
	evA := timedEvent("ev-a", "same@x", "a側の件名", start, true)
	// b: HTMLLink あり・件名なし・URL あり
	evB := timedEvent("ev-b", "same@x", "", start, true)
	evB.HTMLLink = "https://outlook.live.com/calendar/item/b"
	evB.MeetingURL = "https://zoom.us/j/777"
	evB.Description = "b側の本文"

	f.SetFullState(refA, []model.NormalizedEvent{evA})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, []model.NormalizedEvent{evB})
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, failed := e.collectDigest(context.Background(), day)
	require.Empty(t, failed)
	require.Len(t, entries, 1)
	// ペア規則: HTMLLink を持つ b の (Title="", HTMLLink) が採用される(混成しない)
	require.Equal(t, "https://outlook.live.com/calendar/item/b", entries[0].HTMLLink)
	require.Equal(t, "", entries[0].Title)
	// 独立規則: URL / Description は最初の非空
	require.Equal(t, "https://zoom.us/j/777", entries[0].MeetingURL)
	require.Equal(t, "b側の本文", entries[0].Description)
}

// 全アカウントで HTMLLink が空なら Title は v1 規則(最初の非空)のまま。
func TestCollectDigestTitleFallbackWithoutLinks(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc)
	f.SetFullState(refA, []model.NormalizedEvent{timedEvent("ev-a", "same@x", "", start, true)})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"},
		[]model.NormalizedEvent{timedEvent("ev-b", "same@x", "b側の件名", start, true)})
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, _ := e.collectDigest(context.Background(), day)
	require.Len(t, entries, 1)
	require.Equal(t, "b側の件名", entries[0].Title)
}

// リマインドの entry に 3 フィールドがキャッシュから伝搬する。
func TestCheckRemindersCarriesDisplayFields(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	start := time.Date(2026, 7, 5, 9, 55, 0, 0, time.UTC)
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev1", ICalUID: "u@x", Title: "会議",
		MeetingURL: "https://zoom.us/j/1", Description: "本文", HTMLLink: "https://cal/x",
		StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))
	e.checkReminders(context.Background())
	require.Len(t, fn.reminders, 1)
	require.Equal(t, "https://zoom.us/j/1", fn.reminders[0].entry.MeetingURL)
	require.Equal(t, "本文", fn.reminders[0].entry.Description)
	require.Equal(t, "https://cal/x", fn.reminders[0].entry.HTMLLink)
}
```

※ `timedEvent` ヘルパーが返す `model.NormalizedEvent` に対しフィールドを後付けしている。ヘルパー自体は変更しない。

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'DisplayFields|TitleFallback'`
Expected: FAIL(DigestEntry にフィールドなし)

- [ ] **Step 3: 実装**

(a) `DigestEntry` に追加(`AccountIDs` の上):

```go
	MeetingURL  string
	Description string // ダイジェストの blocks では使わない(リマインド用。v2 スペック 4 章)
	HTMLLink    string
```

(b) `appendDigestEntry` — entry 構築に 3 フィールドを追加し、マージ分岐を差し替え:

```go
	entry := DigestEntry{
		Title:       ev.Title,
		StartUTC:    ev.StartUTC,
		EndUTC:      ev.EndUTC,
		IsAllDay:    ev.IsAllDay,
		AllDayStart: ev.AllDayStart,
		MeetingURL:  ev.MeetingURL,
		Description: ev.Description,
		HTMLLink:    ev.HTMLLink,
		AccountIDs:  []string{accountID},
	}
	...
	if i, ok := byKey[key]; ok {
		ex := &(*entries)[i]
		if !slices.Contains(ex.AccountIDs, accountID) {
			ex.AccountIDs = append(ex.AccountIDs, accountID)
		}
		// Title と HTMLLink は同一アカウントからペアで採用する(v2 スペック 4 章):
		// 最初に HTMLLink が非空のアカウントの (Title, HTMLLink) を使う。
		// 全アカウントで HTMLLink が空の間は Title のみ v1 規則(最初の非空)で埋める
		if ex.HTMLLink == "" {
			if ev.HTMLLink != "" {
				ex.Title, ex.HTMLLink = ev.Title, ev.HTMLLink
			} else if ex.Title == "" && ev.Title != "" {
				ex.Title = ev.Title
			}
		}
		if ex.MeetingURL == "" {
			ex.MeetingURL = ev.MeetingURL
		}
		if ex.Description == "" {
			ex.Description = ev.Description
		}
		return
	}
```

(c) `checkReminders` の entry 構築に追加:

```go
		entry := DigestEntry{
			Title:       u.Title,
			StartUTC:    u.StartUTC,
			EndUTC:      u.EndUTC,
			MeetingURL:  u.MeetingURL,
			Description: u.Description,
			HTMLLink:    u.HTMLLink,
			AccountIDs:  []string{u.Ref.AccountID},
		}
```

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/engine/ -race -count=1` → PASS

- [ ] **Step 5: コミット**

```bash
git add internal/engine/
git commit -m "feat: carry meeting URL, description, and calendar link through digest and reminders"
```

---

### Task 6: slack — blocks.go(レンダリング・切り詰め・URL 検証)

**Files:**
- Create: `internal/notify/slack/blocks.go`
- Test: `internal/notify/slack/blocks_test.go`(新規)

**Interfaces:**
- Consumes: Task 5 の DigestEntry 3 フィールド、既存の `escapeText` / `timeRange` / `accountsLabel` / `jaWeekdays`(format.go)
- Produces(Task 7 が使う): `digestBlocks(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location) []block` / `reminderBlocks(e engine.DigestEntry, lead time.Duration, loc *time.Location) []block`

- [ ] **Step 1: 失敗するテストを書く**

新規 `internal/notify/slack/blocks_test.go`:

```go
package slack

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
)

func TestDigestBlocksLayout(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "終日イベント", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"a"},
			HTMLLink: "https://cal/allday"},
		entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b", "c"),
	}
	entries[1].HTMLLink = "https://outlook.live.com/calendar/item/x"
	entries[1].MeetingURL = "https://zoom.us/j/123"

	blocks := digestBlocks(digestDay, entries, []string{"acct-x"}, jst)
	require.Equal(t, "header", blocks[0].Type)
	require.Equal(t, "7/5(日) の予定", blocks[0].Text.Text)

	// 終日: リンクラベル・(終日) プレフィクス・ボタンなし
	require.Equal(t, "section", blocks[1].Type)
	require.Contains(t, blocks[1].Text.Text, "*(終日)*")
	require.Contains(t, blocks[1].Text.Text, "<https://cal/allday|終日イベント>")
	require.Nil(t, blocks[1].Accessory)

	// 時刻指定: 太字レンジ・リンク・複数アカウント併記・参加ボタン
	require.Contains(t, blocks[2].Text.Text, "*10:00–11:00*")
	require.Contains(t, blocks[2].Text.Text, "<https://outlook.live.com/calendar/item/x|設計レビュー>")
	require.Contains(t, blocks[2].Text.Text, "[b, c]")
	require.NotNil(t, blocks[2].Accessory)
	require.Equal(t, "https://zoom.us/j/123", blocks[2].Accessory.URL)
	require.Equal(t, "参加", blocks[2].Accessory.Text.Text)

	// 取得失敗 context
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "⚠ acct-x: 取得失敗")
}

func TestDigestBlocksCapsAt46(t *testing.T) {
	var entries []engine.DigestEntry
	for i := 0; i < 50; i++ {
		entries = append(entries, entry("e", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a"))
	}
	blocks := digestBlocks(digestDay, entries, nil, jst)
	require.LessOrEqual(t, len(blocks), 49) // header 1 + sections 46 + context 他N件 1(+失敗 context なし)
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "…他 4 件")
}

func TestDigestBlocksZeroEvents(t *testing.T) {
	blocks := digestBlocks(digestDay, nil, nil, jst)
	require.Len(t, blocks, 2)
	require.Contains(t, blocks[1].Text.Text, "今日の予定はありません")
}

// URL 検証: https 以外・禁止文字入り・超長はリンク化/ボタン化しない(v2 スペック 7 章)。
func TestBlocksRejectInvalidURLs(t *testing.T) {
	e := entry("件名", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "http://insecure.example.com"
	e.MeetingURL = "https://zoom.us/j/1 23" // 空白入り
	blocks := digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.NotContains(t, blocks[1].Text.Text, "<http")
	require.Contains(t, blocks[1].Text.Text, "件名") // プレーン表示
	require.Nil(t, blocks[1].Accessory)

	e.MeetingURL = "https://zoom.us/j/" + strings.Repeat("9", 2100) // 2,000 rune 超
	blocks = digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Nil(t, blocks[1].Accessory)
}

// 超長件名はエスケープ後 200 rune で切り詰め(1 予定で section 3,000 字を超えない)。
func TestBlocksTruncateLongTitle(t *testing.T) {
	e := entry(strings.Repeat("あ", 500), time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "https://cal/x"
	blocks := digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Less(t, utf8.RuneCountInString(blocks[1].Text.Text), 300)
	require.Contains(t, blocks[1].Text.Text, "…")
}

func TestReminderBlocks(t *testing.T) {
	e := entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b")
	e.MeetingURL = "https://zoom.us/j/123"
	e.HTMLLink = "https://cal/x"
	e.Description = "アジェンダ\n1. 進捗"

	blocks := reminderBlocks(e, 8*time.Minute, jst)
	require.Len(t, blocks, 2)
	require.Contains(t, blocks[0].Text.Text, "⏰ *8分後*")
	require.Contains(t, blocks[0].Text.Text, "<https://cal/x|設計レビュー>")
	require.NotNil(t, blocks[0].Accessory)
	require.Equal(t, "アジェンダ\n1. 進捗", blocks[1].Text.Text)

	// 空白のみ description は section を出さない(v2 スペック 6 章)
	e.Description = " \r\n\t"
	blocks = reminderBlocks(e, 8*time.Minute, jst)
	require.Len(t, blocks, 1)

	// 1 分未満は「まもなく」
	blocks = reminderBlocks(e, 20*time.Second, jst)
	require.Contains(t, blocks[0].Text.Text, "*まもなく*")
}

// description のエスケープとエスケープ後切り詰め(v2 スペック 7 章)。
func TestReminderDescriptionEscapeAndTruncate(t *testing.T) {
	e := entry("x", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "a")

	// <!channel> インジェクション
	e.Description = "<!channel> 全員集合 & <b>太字</b>"
	blocks := reminderBlocks(e, time.Minute, jst)
	require.NotContains(t, blocks[1].Text.Text, "<!channel>")
	require.Contains(t, blocks[1].Text.Text, "&lt;!channel&gt;")

	// & 連続本文: エスケープ後(1 文字 → &amp; の 5 文字)に切り詰めるため 3,000 未満に収まる
	e.Description = strings.Repeat("&", 2000)
	blocks = reminderBlocks(e, time.Minute, jst)
	got := blocks[1].Text.Text
	require.LessOrEqual(t, utf8.RuneCountInString(got), 2905+len([]rune("…(省略)")))
	// 実体参照の途中で切れていない(末尾は …(省略) の直前が完全な &amp;)
	require.NotRegexp(t, `&a?m?p?$`, strings.TrimSuffix(got, "…(省略)"))
}

func TestTruncateEscaped(t *testing.T) {
	// 実体参照の途中に切断位置が当たる場合は参照ごと落とす
	s := strings.Repeat("あ", 8) + "&amp;" // 13 rune
	require.Equal(t, strings.Repeat("あ", 8)+"…", truncateEscaped(s, 10, "…"))
	// 上限以内はそのまま
	require.Equal(t, s, truncateEscaped(s, 13, "…"))
}
```

※ `entry` / `jst` / `digestDay` は format_test.go の既存ヘルパーを使う(`entry` は AccountIDs 可変長)。

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/notify/slack/ -race -count=1 -run 'Blocks|Truncate'`
Expected: FAIL(未定義)

- [ ] **Step 3: 実装**

新規 `internal/notify/slack/blocks.go`:

```go
package slack

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btajp/calsync/internal/engine"
)

// Block Kit の最小型(chat.postMessage の blocks ペイロード用。v2 スペック 8 章)。
type block struct {
	Type      string         `json:"type"`
	Text      *textObject    `json:"text,omitempty"`
	Elements  []textObject   `json:"elements,omitempty"` // context 用
	Accessory *buttonElement `json:"accessory,omitempty"`
}

type textObject struct {
	Type string `json:"type"` // "mrkdwn" | "plain_text"
	Text string `json:"text"`
}

type buttonElement struct {
	Type string     `json:"type"` // 常に "button"
	Text textObject `json:"text"` // plain_text
	URL  string     `json:"url"`
}

const (
	// maxDigestBlockEvents: Slack の 50 ブロック上限に対し
	// header 1 + sections 46 + context 他N件 1 + context 取得失敗 1 = 49。
	// 50 ちょうどを避ける意図的な 1 ブロックの安全マージン(v2 スペック 5 章)
	maxDigestBlockEvents = 46
	maxLabelRunes        = 200  // リンクラベル(件名)の上限
	maxDescRunes         = 2900 // description section の上限(text 3,000 の安全マージン)
	maxURLRunes          = 2000 // リンク・ボタン URL の上限
)

// validRenderURL はレンダリング直前の URL 検証(v2 スペック 7 章)。
// 構造化フィールド由来の URL は https 保証がないため、出所を問わず適用する。
func validRenderURL(u string) bool {
	if !strings.HasPrefix(u, "https://") || utf8.RuneCountInString(u) > maxURLRunes {
		return false
	}
	return !strings.ContainsAny(u, " |<>\t\r\n")
}

// truncateEscaped は escapeText 適用済みの文字列を rune 単位で limit に切り詰める。
// 切断位置が実体参照(&…;)の途中なら参照ごと落とす(v2 スペック 7 章)。
func truncateEscaped(s string, limit int, ellipsis string) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	cut := string(runes[:limit])
	if i := strings.LastIndex(cut, "&"); i >= 0 && !strings.Contains(cut[i:], ";") {
		cut = cut[:i]
	}
	return cut + ellipsis
}

// linkLabel はリンクラベル用の件名を組み立てる(escapeText + `|` 置換 + 200 rune 切り詰め)。
func linkLabel(title string) string {
	if title == "" {
		return "(件名なし)"
	}
	s := strings.ReplaceAll(escapeText(title), "|", "/")
	return truncateEscaped(s, maxLabelRunes, "…")
}

// linkedTitle は件名を HTMLLink へのリンクにする。URL 検証不合格ならプレーン表示。
func linkedTitle(e engine.DigestEntry) string {
	label := linkLabel(e.Title)
	if validRenderURL(e.HTMLLink) {
		return "<" + e.HTMLLink + "|" + label + ">"
	}
	return label
}

// joinButton は会議 URL の「参加」ボタンを返す。URL 検証不合格なら nil(v2 スペック 5 章)。
func joinButton(meetingURL string) *buttonElement {
	if !validRenderURL(meetingURL) {
		return nil
	}
	return &buttonElement{Type: "button", Text: textObject{Type: "plain_text", Text: "参加"}, URL: meetingURL}
}

func digestBlocks(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location) []block {
	d := day.In(loc)
	blocks := []block{{
		Type: "header",
		Text: &textObject{Type: "plain_text", Text: fmt.Sprintf("%d/%d(%s) の予定", int(d.Month()), d.Day(), jaWeekdays[d.Weekday()])},
	}}
	if len(entries) == 0 {
		blocks = append(blocks, block{Type: "section", Text: &textObject{Type: "mrkdwn", Text: "今日の予定はありません"}})
	}
	shown := entries
	if len(shown) > maxDigestBlockEvents {
		shown = shown[:maxDigestBlockEvents]
	}
	for _, e := range shown {
		prefix := "*(終日)*"
		if !e.IsAllDay {
			prefix = "*" + timeRange(e, d, loc) + "*"
		}
		blocks = append(blocks, block{
			Type:      "section",
			Text:      &textObject{Type: "mrkdwn", Text: prefix + "  " + linkedTitle(e) + " " + accountsLabel(e.AccountIDs)},
			Accessory: joinButton(e.MeetingURL),
		})
	}
	if n := len(entries) - len(shown); n > 0 {
		blocks = append(blocks, block{Type: "context", Elements: []textObject{{Type: "mrkdwn", Text: fmt.Sprintf("…他 %d 件", n)}}})
	}
	if len(failedAccounts) > 0 {
		parts := make([]string, 0, len(failedAccounts))
		for _, id := range failedAccounts {
			parts = append(parts, "⚠ "+escapeText(id)+": 取得失敗")
		}
		blocks = append(blocks, block{Type: "context", Elements: []textObject{{Type: "mrkdwn", Text: strings.Join(parts, " / ")}}})
	}
	return blocks
}

func reminderBlocks(e engine.DigestEntry, lead time.Duration, loc *time.Location) []block {
	mins := int(lead.Round(time.Minute) / time.Minute)
	prefix := "まもなく"
	if mins >= 1 {
		prefix = fmt.Sprintf("%d分後", mins)
	}
	day := e.StartUTC.In(loc)
	line := fmt.Sprintf("⏰ *%s* %s %s %s", prefix, timeRange(e, day, loc), linkedTitle(e), accountsLabel(e.AccountIDs))
	blocks := []block{{
		Type:      "section",
		Text:      &textObject{Type: "mrkdwn", Text: line},
		Accessory: joinButton(e.MeetingURL),
	}}
	// 非空 = TrimSpace 後に長さ > 0。表示にも trim 後を使う(v2 スペック 6 章)
	if desc := strings.TrimSpace(e.Description); desc != "" {
		blocks = append(blocks, block{
			Type: "section",
			Text: &textObject{Type: "mrkdwn", Text: truncateEscaped(escapeText(desc), maxDescRunes, "…(省略)")},
		})
	}
	return blocks
}
```

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/notify/slack/ -race -count=1`
Expected: PASS(既存 format テスト含む)

- [ ] **Step 5: コミット**

```bash
git add internal/notify/slack/
git commit -m "feat: render digest and reminder as Block Kit with join buttons"
```

---

### Task 7: slack — blocks 送信と invalid_blocks 縮退

**Files:**
- Modify: `internal/notify/slack/slack.go`(SendDigest / SendReminder / post)
- Test: `internal/notify/slack/slack_test.go`(追記)

**Interfaces:**
- Consumes: Task 6 の digestBlocks / reminderBlocks

- [ ] **Step 1: 失敗するテストを書く**

`internal/notify/slack/slack_test.go` に追記:

```go
// blocks がペイロードに含まれ、text は fallback として残る。
func TestPostMessageIncludesBlocks(t *testing.T) {
	var got struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.NotEmpty(t, got.Text) // fallback(v1 テキスト)
	require.NotEmpty(t, got.Blocks)
	require.Equal(t, "section", got.Blocks[0]["type"])
}

// invalid_blocks のときだけ fallback text 単体で 1 回縮退再送する(v2 スペック 8 章)。
func TestInvalidBlocksFallsBackToTextOnce(t *testing.T) {
	var calls []map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		calls = append(calls, body)
		if _, hasBlocks := body["blocks"]; hasBlocks {
			w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.Len(t, calls, 2)
	_, hasBlocks := calls[1]["blocks"]
	require.False(t, hasBlocks) // 再送は text のみ
}

// 縮退再送も失敗したら通常分類(ここでは non-retryable)で返る。
func TestInvalidBlocksFallbackFailurePropagates(t *testing.T) {
	n := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.True(t, errors.Is(err, notify.ErrNonRetryable))
	require.Equal(t, 2, n) // 縮退は 1 回だけ
}

// invalid_blocks 以外のエラーでは縮退しない。
func TestNonBlocksErrorsDoNotFallBack(t *testing.T) {
	n := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.Equal(t, 1, n)
}
```

- [ ] **Step 2: FAIL 確認**

Run: `go test ./internal/notify/slack/ -race -count=1 -run 'Blocks|FallsBack|FallBack'`
Expected: FAIL(blocks 未送信)

- [ ] **Step 3: 実装**

`internal/notify/slack/slack.go`:

```go
func (c *Client) SendDigest(ctx context.Context, day time.Time, entries []engine.DigestEntry, failedAccounts []string) error {
	return c.post(ctx,
		formatDigest(day, entries, failedAccounts, c.loc()),
		digestBlocks(day, entries, failedAccounts, c.loc()))
}

func (c *Client) SendReminder(ctx context.Context, e engine.DigestEntry, lead time.Duration) error {
	return c.post(ctx, formatReminder(e, lead, c.loc()), reminderBlocks(e, lead, c.loc()))
}

// post は blocks 付きで投稿する(text は通知プレビュー・非対応面用の fallback)。
// blocks 起因のエラー(invalid_blocks 系)はイベントデータ(件名・本文)由来でありうる
// ため、fallback text 単体で 1 回だけ縮退再送する(v2 スペック 8 章)。
func (c *Client) post(ctx context.Context, text string, blocks []block) error {
	ch, err := c.channelID(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{"channel": ch, "text": text}
	if len(blocks) > 0 {
		payload["blocks"] = blocks
	}
	_, err = c.call(ctx, "chat.postMessage", payload)
	if err != nil && len(blocks) > 0 && strings.Contains(err.Error(), "invalid_blocks") {
		log.Printf("slack: invalid blocks; resending as plain text: %v", err)
		_, err = c.call(ctx, "chat.postMessage", map[string]any{"channel": ch, "text": text})
	}
	return err
}
```

import に `log` を追加。既存の `post(ctx, text)` 呼び出しは上記 2 メソッドのみなのでシグネチャ変更で完結する。

- [ ] **Step 4: PASS 確認**

Run: `go test ./internal/notify/slack/ -race -count=1` → PASS。続けて `go test ./... -race -count=1` → PASS

- [ ] **Step 5: コミット**

```bash
git add internal/notify/slack/
git commit -m "feat: send Block Kit payloads with plain-text fallback on invalid_blocks"
```

---

### Task 8: ドキュメント+最終検証

**Files:**
- Modify: `README.md`(Slack 通知節の表示例・注意書き更新)
- Modify: `CHANGELOG.md`(`[Unreleased]` `### Added` 先頭)
- Modify: `docs/superpowers/specs/2026-07-06-slack-notifications-v2-design.md`(実測記録の追記は初回稼働後のため、この Task では 12 章スパイクの現状注記のみ確認)

- [ ] **Step 1: README 更新**

Slack 通知節: 通知の内容説明を Block Kit 版(予定ごとのブロック・件名はカレンダーへのリンク・会議 URL は「参加」ボタン・リマインドは本文全文付き)に更新し、**「会議 URL(パスワード付き Zoom リンク)と予定の本文が通知先チャンネルに流れる」**注意書きを強化する。既存の章立て・トーンに合わせる。

- [ ] **Step 2: CHANGELOG 追記**

`[Unreleased]` の `### Added` 先頭に:

```markdown
- **Slack 通知の Block Kit 化(v2)**: ダイジェストを予定ごとのブロック表示にし、件名をカレンダーの当該予定へのリンクに、Zoom / Meet / Teams の会議 URL を「参加」ボタンにした(conferenceData / onlineMeeting → location・本文の URL 検出の順で抽出)。開始前リマインドに会議参加ボタンと本文全文(プレーンテキスト化・3,000 字制限内に切り詰め)を追加。通知プレビューには従来のテキスト形式を fallback として維持し、blocks が不正な場合はテキストのみで 1 回縮退再送する
```

- [ ] **Step 3: 最終検証一式**

```bash
go build -o ./calsync ./cmd/calsync
go test ./... -race -count=1
go vet ./...
gofmt -l internal/ cmd/        # 出力なしが正
docker compose config -q
rm -f ./calsync
```

Expected: すべて成功。

- [ ] **Step 4: コミット**

```bash
git add README.md CHANGELOG.md
git commit -m "docs: document Block Kit notifications and meeting URL extraction"
```

---

## Plan Self-Review メモ

- スペック網羅: 3.1/3.2 → Task 1、3.3 → Task 2、3.4 → Task 3・4、4 章 → Task 5、5・6・7 章 → Task 6、8 章 → Task 7、9 章 → Task 8。12 章スパイク 1〜4 は初回稼働時の実測(実装計画外)
- 型整合: `digestBlocks` / `reminderBlocks` / `block` / `truncateEscaped` / `validRenderURL` / `linkLabel` は Task 6 定義 → Task 7 消費。`DigestEntry` 3 フィールドは Task 5 定義 → Task 6 消費。`UpcomingEvent` 3 フィールドは Task 2 定義 → Task 5 消費
- Task 6 のテストは format_test.go の既存ヘルパー(`entry`/`jst`/`digestDay`)前提 — 実装者は既存シグネチャ(`entry(title, start, end, accts...)`)を確認してから書くこと
